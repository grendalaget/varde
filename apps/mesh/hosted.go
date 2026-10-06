package main

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	meshv1 "github.com/grendalaget/varde/go/gen/mesh/v1"
	"github.com/grendalaget/varde/go/meshproto"
)

// hostedService is what this node accepts inbound traffic for.
type hostedService struct {
	svc *meshv1.HostedService

	mu    sync.Mutex
	flows map[uint64]*inboundFlow // flow_id → flow
}

// inboundFlow is a host-side UDP flow: QUIC datagrams ↔ 127.0.0.1:target.
type inboundFlow struct {
	target  *net.UDPConn
	dst     *net.UDPAddr
	lastUse int64 // unixnano
	done    chan struct{}
	closed  bool
	mu      sync.Mutex
}

// SetHostedServices applies a full replacement; stale (service_id, epoch)
// streams are rejected from then on.
func (n *Node) SetHostedServices(req *meshv1.SetHostedServicesRequest) {
	n.mu.Lock()
	defer n.mu.Unlock()
	want := map[string]*meshv1.HostedService{}
	for _, s := range req.GetServices() {
		want[s.GetServiceId()] = s
	}
	for id, hs := range n.hosted {
		if _, ok := want[id]; !ok {
			hs.closeFlows()
			delete(n.hosted, id)
		}
	}
	for _, s := range req.GetServices() {
		existing := n.hosted[s.GetServiceId()]
		if existing == nil {
			n.hosted[s.GetServiceId()] = &hostedService{svc: s, flows: map[uint64]*inboundFlow{}}
		} else {
			if existing.svc.GetEpoch() != s.GetEpoch() {
				existing.closeFlows()
			}
			existing.svc = s // ports/config are full-replacement too
		}
	}
}

func (hs *hostedService) closeFlows() {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	for _, f := range hs.flows {
		f.close()
	}
	hs.flows = map[uint64]*inboundFlow{}
}

func (f *inboundFlow) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.done)
		_ = f.target.Close()
	}
}

// hostedLookup finds the hosted service for a stream, verifying epoch+port.
func (n *Node) hostedLookup(serviceID string, epoch uint64, port uint32, proto meshv1.Protocol) (*hostedService, uint32, meshv1.RejectReason) {
	n.mu.RLock()
	hs := n.hosted[serviceID]
	n.mu.RUnlock()
	if hs == nil {
		return nil, 0, meshv1.RejectReason_REJECT_REASON_UNKNOWN_SERVICE
	}
	if hs.svc.GetEpoch() != epoch {
		return nil, 0, meshv1.RejectReason_REJECT_REASON_STALE_EPOCH
	}
	for _, hp := range hs.svc.GetPorts() {
		if hp.GetPort() == port && hp.GetProtocol() == proto {
			return hs, hp.GetTargetPort(), 0
		}
	}
	return nil, 0, meshv1.RejectReason_REJECT_REASON_UNKNOWN_PORT
}

// handleServiceStream is the host side of a TCP ServiceStream.
func (p *peerState) handleServiceStream(st *quic.Stream, s *meshv1.ServiceStream) {
	_, target, reason := p.n.hostedLookup(s.GetServiceId(), s.GetEpoch(), s.GetPort(), s.GetProtocol())
	if reason != 0 {
		p.n.emitRouteRejected(s.GetServiceId(), p.id, reason.String())
		rejectStream(st, reason, "not hosted here")
		return
	}
	up, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", target), 10*time.Second)
	if err != nil {
		rejectStream(st, meshv1.RejectReason_REJECT_REASON_TARGET_UNREACHABLE, err.Error())
		return
	}
	if err := acceptStream(st); err != nil {
		_ = up.Close()
		return
	}
	p.splice(st, up)
}

// handleUDPFlowStream registers a host-side UDP flow. The stream stays open
// for the flow's lifetime; payloads are datagrams.
func (p *peerState) handleUDPFlowStream(st *quic.Stream, uf *meshv1.UdpFlow) {
	hs, target, reason := p.n.hostedLookup(uf.GetServiceId(), uf.GetEpoch(), uf.GetPort(), meshv1.Protocol_PROTOCOL_UDP)
	if reason != 0 {
		p.n.emitRouteRejected(uf.GetServiceId(), p.id, reason.String())
		rejectStream(st, reason, "not hosted here")
		return
	}
	dst := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: int(target)}
	c, err := net.DialUDP("udp", nil, dst)
	if err != nil {
		rejectStream(st, meshv1.RejectReason_REJECT_REASON_TARGET_UNREACHABLE, err.Error())
		return
	}
	if err := acceptStream(st); err != nil {
		_ = c.Close()
		return
	}
	f := &inboundFlow{target: c, dst: dst, lastUse: time.Now().UnixNano(), done: make(chan struct{})}
	hs.mu.Lock()
	old := hs.flows[uf.GetFlowId()]
	hs.flows[uf.GetFlowId()] = f
	hs.mu.Unlock()
	if old != nil {
		old.close()
	}
	go func() {
		buf := make([]byte, 64*1024)
		for {
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			nr, err := c.Read(buf)
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
			conn := p.current()
			if conn == nil {
				return
			}
			if err := conn.SendDatagram(meshproto.EncodeFlowDatagram(uf.GetFlowId(), buf[:nr])); err != nil {
				return
			}
			p.bytesTx.Add(uint64(nr))
		}
	}()
	// closing the stream ends the flow
	go func() {
		_, _ = st.Read(make([]byte, 1))
		f.close()
		hs.mu.Lock()
		delete(hs.flows, uf.GetFlowId())
		hs.mu.Unlock()
	}()
}

// handleInternalStream forwards to a local internal service port.
func (p *peerState) handleInternalStream(st *quic.Stream, is *meshv1.InternalStream) {
	p.n.mu.RLock()
	port, ok := p.n.internal[is.GetName()]
	p.n.mu.RUnlock()
	if !ok {
		rejectStream(st, meshv1.RejectReason_REJECT_REASON_UNKNOWN_SERVICE, "unknown internal service")
		return
	}
	up, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 10*time.Second)
	if err != nil {
		rejectStream(st, meshv1.RejectReason_REJECT_REASON_TARGET_UNREACHABLE, err.Error())
		return
	}
	if err := acceptStream(st); err != nil {
		_ = up.Close()
		return
	}
	p.splice(st, up)
}

// datagramLoop receives QUIC datagrams on one conn and routes them to flows.
func (p *peerState) datagramLoop(conn *quic.Conn) {
	defer p.wg.Done()
	for {
		d, err := conn.ReceiveDatagram(conn.Context())
		if err != nil {
			return
		}
		flowID, payload, err := meshproto.DecodeFlowDatagram(d)
		if err != nil {
			continue
		}
		p.bytesRx.Add(uint64(len(payload)))
		p.deliverDatagram(flowID, payload)
	}
}

// deliverDatagram finds the owning flow: host-side inbound flows and
// client-side outbound flows both live in per-peer maps.
func (p *peerState) deliverDatagram(flowID uint64, payload []byte) {
	p.mu.Lock()
	f := p.outboundFlows[flowID]
	p.mu.Unlock()
	if f != nil {
		f.deliver(payload)
		return
	}
	// host side: the owning hosted service's inbound flow
	p.n.mu.RLock()
	var target *inboundFlow
	for _, hs := range p.n.hosted {
		hs.mu.Lock()
		if fl := hs.flows[flowID]; fl != nil {
			target = fl
		}
		hs.mu.Unlock()
	}
	p.n.mu.RUnlock()
	if target != nil {
		target.mu.Lock()
		target.lastUse = time.Now().UnixNano()
		target.mu.Unlock()
		_, _ = target.target.Write(payload)
	}
}

// outbound flow bookkeeping on the client side.
func (f *udpFlow) deliver(payload []byte) {
	f.touch()
	if f.rt != nil {
		if c := f.rt.udpLn[f.port]; c != nil {
			_, _ = c.WriteToUDP(payload, f.src)
		}
	}
}
