package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"

	meshv1 "github.com/arnemolland/p2pgames/go/gen/mesh/v1"
	"github.com/arnemolland/p2pgames/go/meshproto"
)

// routeState is the client side of one service: TCP/UDP listeners on the
// loopback IP forwarding to the current host.
type routeState struct {
	n         *Node
	serviceID string
	ip        string

	mu      sync.Mutex
	host    string // current host_node_id; "" = stopped
	epoch   uint64
	ports   []*meshv1.PortSpec
	tcpLns  map[uint32]net.Listener
	udpLn   map[uint32]*net.UDPConn
	flows   map[string]*udpFlow // "port|srcaddr" → flow
	flowSeq atomic.Uint64
	closed  bool
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
				flows: map[string]*udpFlow{},
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
	hostChanged := rt.host != r.GetHostNodeId() || rt.epoch != r.GetEpoch()
	rt.host = r.GetHostNodeId()
	rt.epoch = r.GetEpoch()

	wantTCP, wantUDP := map[uint32]bool{}, map[uint32]bool{}
	for _, p := range r.GetPorts() {
		if p.GetProtocol() == meshv1.Protocol_PROTOCOL_UDP {
			wantUDP[p.GetPort()] = true
		} else {
			wantTCP[p.GetPort()] = true
		}
	}
	for port, ln := range rt.tcpLns {
		if !wantTCP[port] {
			_ = ln.Close()
			delete(rt.tcpLns, port)
		}
	}
	for port, ln := range rt.udpLn {
		if !wantUDP[port] {
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
		// existing streams end; new ones go to the new host
		for _, f := range rt.flows {
			f.close()
		}
		rt.flows = map[string]*udpFlow{}
	}
	return nil
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
		f.close()
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
		}
		_ = c.Close()
		return
	}
	peer := rt.n.peer(host)
	if peer == nil {
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
		_ = c.Close()
		return
	}
	peer.splice(st, c)
}

func spliceTCP(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		buf := make([]byte, 64*1024)
		for {
			nr, err := a.Read(buf)
			if nr > 0 {
				if _, werr := b.Write(buf[:nr]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}()
	go func() {
		buf := make([]byte, 64*1024)
		for {
			nr, err := b.Read(buf)
			if nr > 0 {
				if _, werr := a.Write(buf[:nr]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}()
	<-done
	_ = a.Close()
	_ = b.Close()
}

// ---- UDP flows ----

type udpFlow struct {
	id       uint64
	src      *net.UDPAddr
	stream   *quic.Stream // stays open for the flow's lifetime
	lastUsed atomic.Int64 // unixnano
	closed   atomic.Bool
	done     chan struct{}
	peer     *peerState // nil for local
	port     uint32
	rt       *routeState
	local    *net.UDPConn // when host==local
}

func (f *udpFlow) close() {
	if f.closed.CompareAndSwap(false, true) {
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
	}
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
		f.peer.bytesTx.Add(uint64(len(payload)))
		// oversize datagrams are dropped (and counted) per spec
		if errors.Is(err, &quic.DatagramTooLargeError{}) {
			rtMetricDrop(f.rt.n)
			return
		}
		f.close()
		return
	}
	f.peer.bytesTx.Add(uint64(len(payload)))
}

func rtMetricDrop(n *Node) {
	if n.metrics != nil {
		n.metrics.datagramsDropped.Inc()
	}
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
		key := fmt.Sprintf("%d|%s", port, src)
		f := rt.flows[key]
		rt.mu.Unlock()
		if f == nil {
			f = rt.openFlow(key, c, src, port)
			if f == nil {
				continue // no host; drop
			}
		}
		f.send(payload)
	}
}

func (rt *routeState) openFlow(key string, c *net.UDPConn, src *net.UDPAddr, port uint32) *udpFlow {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if f := rt.flows[key]; f != nil {
		return f
	}
	host, epoch := rt.host, rt.epoch
	if host == "" {
		return nil
	}
	f := &udpFlow{
		id: rt.flowSeq.Add(1), src: src, port: port, rt: rt,
		done: make(chan struct{}),
	}
	f.touch()
	if host == rt.n.nodeID {
		tp, ok := rt.n.localTarget(rt.serviceID, port)
		if !ok {
			return nil
		}
		lc, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: tp})
		if err != nil {
			return nil
		}
		f.local = lc
		go rt.localFlowRead(f, c, lc, src)
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
		go func() {
			// peer closes the flow when the stream dies
			buf := make([]byte, 1)
			_, _ = st.Read(buf)
			f.close()
		}()
	}
	rt.flows[key] = f
	go rt.reapFlow(key, f)
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
func (rt *routeState) reapFlow(key string, f *udpFlow) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-f.done:
			return
		case <-t.C:
			if time.Since(time.Unix(0, f.lastUsed.Load())) > rt.n.timings.UDPFlowIdle {
				f.close()
				rt.mu.Lock()
				delete(rt.flows, key)
				rt.mu.Unlock()
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
