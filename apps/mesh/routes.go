package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"

	meshv1 "github.com/grendalaget/varde/go/gen/mesh/v1"
	"github.com/grendalaget/varde/go/meshproto"
)

// routeState is the client side of one service: TCP/UDP listeners on the
// loopback IP forwarding to the current host.
type routeState struct {
	n         *Node
	serviceID string
	ip        string

	mu     sync.Mutex
	host   string // current host_node_id; "" = stopped
	epoch  uint64
	ports  []*meshv1.PortSpec
	tcpLns map[uint32]net.Listener
	udpLn  map[uint32]*net.UDPConn
	flows  map[udpFlowKey]*udpFlow
	closed bool
	// drainDeadline/drainHost/drainEpoch: while set, UDP flows opened under
	// the previous host/epoch keep draining (TCP splices always drain
	// naturally). A warm migration's route flip sets it so the old host can
	// hand clients off.
	drainDeadline time.Time
	drainHost     string
	drainEpoch    uint64
}

// SetRoutes applies a full replacement of the route table.
func (n *Node) SetRoutes(req *meshv1.SetRoutesRequest) []*meshv1.RouteBindError {
	n.mu.Lock()
	defer n.mu.Unlock()
	want := map[string]*meshv1.ServiceRoute{}
	for _, r := range req.GetRoutes() {
		want[r.GetServiceId()] = r
	}
	for id, rt := range n.routes {
		if _, ok := want[id]; !ok {
			rt.closeAll()
			delete(n.routes, id)
		}
	}
	var errs []*meshv1.RouteBindError
	for _, r := range req.GetRoutes() {
		rt := n.routes[r.GetServiceId()]
		if rt == nil {
			rt = &routeState{
				n: n, serviceID: r.GetServiceId(), ip: r.GetLoopbackIp(),
				tcpLns: map[uint32]net.Listener{}, udpLn: map[uint32]*net.UDPConn{},
				flows: map[udpFlowKey]*udpFlow{},
			}
			n.routes[r.GetServiceId()] = rt
		}
		if e := rt.apply(r); e != nil {
			errs = append(errs, &meshv1.RouteBindError{ServiceId: r.GetServiceId(), Message: e.Error()})
		}
	}
	return errs
}

// apply rebinds listeners and, on host/epoch change, drops existing streams.
func (rt *routeState) apply(r *meshv1.ServiceRoute) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.closed {
		return errors.New("route closed")
	}
	rt.ip = r.GetLoopbackIp()
	rt.ports = r.GetPorts()
	prevHost, prevEpoch := rt.host, rt.epoch
	hostChanged := prevHost != r.GetHostNodeId() || prevEpoch != r.GetEpoch()
	rt.host = r.GetHostNodeId()
	rt.epoch = r.GetEpoch()

	wantTCP, wantUDP := map[uint32]struct{}{}, map[uint32]struct{}{}
	for _, p := range r.GetPorts() {
		if p.GetProtocol() == meshv1.Protocol_PROTOCOL_UDP {
			wantUDP[p.GetPort()] = struct{}{}
		} else {
			wantTCP[p.GetPort()] = struct{}{}
		}
	}
	for port, ln := range rt.tcpLns {
		if _, ok := wantTCP[port]; !ok {
			_ = ln.Close()
			delete(rt.tcpLns, port)
		}
	}
	for port, ln := range rt.udpLn {
		if _, ok := wantUDP[port]; !ok {
			_ = ln.Close()
			delete(rt.udpLn, port)
		}
	}
	ip := net.ParseIP(rt.ip)
	if ip == nil {
		return fmt.Errorf("bad loopback_ip %q", rt.ip)
	}
	for port := range wantTCP {
		if _, ok := rt.tcpLns[port]; ok {
			continue
		}
		ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: ip, Port: int(port)})
		if err != nil {
			return fmt.Errorf("bind tcp %s:%d: %w", rt.ip, port, err)
		}
		rt.tcpLns[port] = ln
		go rt.tcpAcceptLoop(ln, port)
	}
	for port := range wantUDP {
		if _, ok := rt.udpLn[port]; ok {
			continue
		}
		c, err := net.ListenUDP("udp", &net.UDPAddr{IP: ip, Port: int(port)})
		if err != nil {
			return fmt.Errorf("bind udp %s:%d: %w", rt.ip, port, err)
		}
		rt.udpLn[port] = c
		go rt.udpReadLoop(c, port)
	}
	if hostChanged {
		// existing streams end; new ones go to the new host — unless a warm
		// migration granted a drain window for the previous host/epoch.
		drainOK := r.GetDrainHostNodeId() == prevHost &&
			r.GetDrainEpoch() == prevEpoch &&
			time.Now().UnixMilli() < r.GetDrainUntilUnixMs()
		if drainOK && len(rt.flows) > 0 {
			// The drain window only covers the immediately previous host:
			// on consecutive flips, flows older than (prevHost,prevEpoch)
			// would otherwise never be reaped.
			for key, f := range rt.flows {
				if f.hostNodeID != prevHost || f.epoch != prevEpoch {
					f.closeLocked()
					delete(rt.flows, key)
				}
			}
			rt.drainDeadline = time.UnixMilli(r.GetDrainUntilUnixMs())
			rt.drainHost = prevHost
			rt.drainEpoch = prevEpoch
			go rt.reapDrainedFlows(rt.drainDeadline)
		} else {
			for _, f := range rt.flows {
				f.closeLocked()
			}
			rt.flows = map[udpFlowKey]*udpFlow{}
		}
	} else if len(rt.flows) > 0 && !rt.drainDeadline.IsZero() && r.GetDrainHostNodeId() == "" {
		// Same host re-asserted without drain fields: the drain is over.
		rt.endDrainLocked()
	}
	return nil
}

// reapDrainedFlows closes leftover drained flows once the drain window
// ends. deadline is the window it was armed for; a later flip may have
// refreshed or cleared rt.drainDeadline, so bail unless it still matches.
func (rt *routeState) reapDrainedFlows(deadline time.Time) {
	t := time.NewTimer(time.Until(deadline) + time.Second)
	defer t.Stop()
	<-t.C
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !rt.drainDeadline.Equal(deadline) {
		return
	}
	rt.endDrainLocked()
}

// endDrainLocked closes only the flows opened under the drained host/epoch —
// flows created after the flip belong to the new host and must survive.
// Caller holds rt.mu.
func (rt *routeState) endDrainLocked() {
	host, epoch := rt.drainHost, rt.drainEpoch
	rt.drainDeadline = time.Time{}
	rt.drainHost, rt.drainEpoch = "", 0
	for key, f := range rt.flows {
		if f.hostNodeID == host && f.epoch == epoch {
			f.closeLocked()
			delete(rt.flows, key)
		}
	}
}

func (rt *routeState) closeAll() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.closed = true
	for _, ln := range rt.tcpLns {
		_ = ln.Close()
	}
	rt.tcpLns = nil
	for _, c := range rt.udpLn {
		_ = c.Close()
	}
	rt.udpLn = nil
	for _, f := range rt.flows {
		f.closeLocked()
	}
	rt.flows = nil
}

func (rt *routeState) hostInfo() (host string, epoch uint64) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.host, rt.epoch
}

// localTarget returns the local target port when this node is the host.
func (n *Node) localTarget(serviceID string, port uint32) (int, bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	hs := n.hosted[serviceID]
	if hs == nil {
		return 0, false
	}
	for _, hp := range hs.svc.GetPorts() {
		if hp.GetPort() == port {
			return int(hp.GetTargetPort()), true
		}
	}
	return 0, false
}

func (rt *routeState) tcpAcceptLoop(ln net.Listener, port uint32) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go rt.serveTCP(c, port)
	}
}

func (rt *routeState) serveTCP(c net.Conn, port uint32) {
	host, epoch := rt.hostInfo()
	if host == "" {
		_ = c.Close()
		return
	}
	if host == rt.n.nodeID {
		if tp, ok := rt.n.localTarget(rt.serviceID, port); ok {
			up, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", tp))
			if err == nil {
				spliceTCP(c, up)
				return
			}
			rt.n.log.Debug("local target dial failed", "service", rt.serviceID, "port", port, "error", err)
		}
		_ = c.Close()
		return
	}
	peer := rt.n.peer(host)
	if peer == nil {
		rt.n.log.Debug("no peer for host", "service", rt.serviceID, "host", host)
		_ = c.Close()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := peer.openStream(ctx, &meshv1.StreamOpen{Kind: &meshv1.StreamOpen_Service{
		Service: &meshv1.ServiceStream{
			ServiceId: rt.serviceID, Epoch: epoch, Port: port,
			Protocol: meshv1.Protocol_PROTOCOL_TCP,
		},
	}})
	if err != nil {
		rt.n.log.Debug("open service stream failed", "service", rt.serviceID, "host", host, "error", err)
		_ = c.Close()
		return
	}
	peer.splice(st, c)
}

func spliceTCP(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(b, a)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(a, b)
		done <- struct{}{}
	}()
	<-done
	_ = a.Close()
	_ = b.Close()
}

// ---- UDP flows ----

type udpFlow struct {
	id         uint64
	key        udpFlowKey // rt.flows entry; removed on close
	src        *net.UDPAddr
	hostNodeID string       // host this flow was opened under (drain scoping)
	epoch      uint64       // epoch this flow was opened under
	stream     *quic.Stream // stays open for the flow's lifetime
	lastUsed   atomic.Int64 // unixnano
	closed     atomic.Bool
	done       chan struct{}
	peer       *peerState // nil for local
	port       uint32
	rt         *routeState
	local      *net.UDPConn // when host==local
}

// close ends the flow and drops its rt.flows entry, so the client's next
// packet opens a fresh flow instead of feeding one the host already forgot.
// Caller must not hold rt.mu; use closeLocked under it.
func (f *udpFlow) close() {
	if !f.shutdown() || f.rt == nil {
		return
	}
	f.rt.mu.Lock()
	f.removeLocked()
	f.rt.mu.Unlock()
}

// closeLocked is close for callers holding rt.mu.
func (f *udpFlow) closeLocked() {
	if f.shutdown() && f.rt != nil {
		f.removeLocked()
	}
}

// removeLocked deletes f's map entry unless a newer flow already took the
// key. Caller holds rt.mu.
func (f *udpFlow) removeLocked() {
	if f.rt.flows[f.key] == f {
		delete(f.rt.flows, f.key)
	}
}

// shutdown releases the flow's resources; true only for the first call.
func (f *udpFlow) shutdown() bool {
	if !f.closed.CompareAndSwap(false, true) {
		return false
	}
	close(f.done)
	if f.peer != nil {
		f.peer.removeOutboundFlow(f.id)
	}
	if f.stream != nil {
		_ = f.stream.Close()
		f.stream.CancelRead(0)
	}
	if f.local != nil {
		_ = f.local.Close()
	}
	return true
}

func (f *udpFlow) touch() { f.lastUsed.Store(time.Now().UnixNano()) }

// send delivers a client→host payload.
func (f *udpFlow) send(payload []byte) {
	f.touch()
	if f.local != nil {
		_, _ = f.local.Write(payload)
		return
	}
	conn := f.peer.current()
	if conn == nil {
		f.close()
		return
	}
	if err := conn.SendDatagram(meshproto.EncodeFlowDatagram(f.id, payload)); err != nil {
		// oversize datagrams are dropped (and counted) per spec
		if isDatagramTooLarge(err) {
			rtMetricDrop(f.rt.n)
			return
		}
		f.close()
		return
	}
	f.peer.bytesTx.Add(uint64(len(payload)))
}

// isDatagramTooLarge matches quic-go's oversize-datagram error. errors.Is
// with a zero-valued target never matches: its Is compares
// MaxDatagramPayloadSize.
func isDatagramTooLarge(err error) bool {
	var tooLarge *quic.DatagramTooLargeError
	return errors.As(err, &tooLarge)
}

func rtMetricDrop(n *Node) {
	if n.metrics != nil {
		n.metrics.datagramsDropped.Inc()
	}
}

// udpFlowKey identifies a client UDP flow: the loopback port and the
// client's source address. Comparable, so no per-packet allocation.
type udpFlowKey struct {
	port uint32
	src  netip.AddrPort
}

func (rt *routeState) udpReadLoop(c *net.UDPConn, port uint32) {
	buf := make([]byte, 64*1024)
	for {
		nr, src, err := c.ReadFromUDP(buf)
		if err != nil {
			return
		}
		payload := append([]byte(nil), buf[:nr]...)
		rt.mu.Lock()
		key := udpFlowKey{port, src.AddrPort()}
		f := rt.flows[key]
		rt.mu.Unlock()
		if f == nil || f.closed.Load() {
			f = rt.openFlow(key, c, src, port)
			if f == nil {
				continue // no host; drop
			}
		}
		f.send(payload)
	}
}

// openFlow opens a flow for key. rt.mu is held only to snapshot host/epoch
// and to insert: resolving the target takes n.mu (SetRoutes holds n.mu
// while taking rt.mu) and the stream open can take seconds.
func (rt *routeState) openFlow(key udpFlowKey, c *net.UDPConn, src *net.UDPAddr, port uint32) *udpFlow {
	rt.mu.Lock()
	if rt.closed {
		rt.mu.Unlock()
		return nil
	}
	if f := rt.flows[key]; f != nil && !f.closed.Load() {
		rt.mu.Unlock()
		return f
	}
	host, epoch := rt.host, rt.epoch
	rt.mu.Unlock()
	if host == "" {
		return nil
	}
	f := &udpFlow{
		id: rt.n.flowSeq.Add(1), key: key, src: src, port: port, rt: rt,
		hostNodeID: host, epoch: epoch,
		done: make(chan struct{}),
	}
	f.touch()
	if host == rt.n.nodeID {
		tp, ok := rt.n.localTarget(rt.serviceID, port)
		if !ok {
			return nil
		}
		lc, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: tp})
		if err != nil {
			return nil
		}
		f.local = lc
	} else {
		peer := rt.n.peer(host)
		if peer == nil {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		st, err := peer.openStream(ctx, &meshv1.StreamOpen{Kind: &meshv1.StreamOpen_UdpFlow{
			UdpFlow: &meshv1.UdpFlow{
				ServiceId: rt.serviceID, Epoch: epoch, Port: port, FlowId: f.id,
			},
		}})
		if err != nil {
			return nil
		}
		f.peer = peer
		f.stream = st
		peer.registerOutboundFlow(f.id, f)
	}

	rt.mu.Lock()
	if rt.closed || rt.host != host || rt.epoch != epoch {
		// route closed or moved while we were opening
		rt.mu.Unlock()
		f.close()
		return nil
	}
	if other := rt.flows[key]; other != nil && !other.closed.Load() {
		// another packet from this client won the race
		rt.mu.Unlock()
		f.close()
		return other
	}
	rt.flows[key] = f
	rt.mu.Unlock()

	if f.local != nil {
		go rt.localFlowRead(f, c, f.local, src)
	} else {
		go func() {
			// peer closes the flow when the stream dies
			buf := make([]byte, 1)
			_, _ = f.stream.Read(buf)
			f.close()
		}()
	}
	go rt.reapFlow(f)
	return f
}

// localFlowRead pumps replies from the local UDP target back to the client.
func (rt *routeState) localFlowRead(f *udpFlow, listen *net.UDPConn, lc *net.UDPConn, src *net.UDPAddr) {
	buf := make([]byte, 64*1024)
	for {
		_ = lc.SetReadDeadline(time.Now().Add(5 * time.Second))
		nr, err := lc.Read(buf)
		if err != nil {
			select {
			case <-f.done:
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		f.touch()
		_, _ = listen.WriteToUDP(buf[:nr], src)
	}
}

// reapFlow closes flows idle past the timeout.
func (rt *routeState) reapFlow(f *udpFlow) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-f.done:
			return
		case <-t.C:
			if time.Since(time.Unix(0, f.lastUsed.Load())) > rt.n.timings.UDPFlowIdle {
				f.close()
				return
			}
		}
	}
}

// peer() resolves a node id to its peerState.
func (n *Node) peer(id string) *peerState {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.peers[id]
}

// isForceRelay reads the flag safely.
func (n *Node) isForceRelay() bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.forceRelay
}
