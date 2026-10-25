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
}

// inboundFlow is a host-side UDP flow: QUIC datagrams ↔ 127.0.0.1:target.
// Flows are registered on the peerState that opened them — flow ids are
// scoped to the opener's sequence, so two peers may use the same id.
type inboundFlow struct {
	serviceID string
	target    *net.UDPConn
	done      chan struct{}
	closed    bool
	mu        sync.Mutex
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
	for id := range n.hosted {
		if _, ok := want[id]; !ok {
			n.closeInboundFlows(id)
			delete(n.hosted, id)
		}
	}
	for _, s := range req.GetServices() {
		existing := n.hosted[s.GetServiceId()]
		if existing == nil {
			n.hosted[s.GetServiceId()] = &hostedService{svc: s}
		} else {
			if existing.svc.GetEpoch() != s.GetEpoch() {
				n.closeInboundFlows(s.GetServiceId())
			}
			existing.svc = s // ports/config are full-replacement too
		}
	}
}

// closeInboundFlows ends every host-side UDP flow for serviceID, across all
// peers. Caller holds n.mu.
func (n *Node) closeInboundFlows(serviceID string) {
	for _, p := range n.peers {
		p.mu.Lock()
		for id, f := range p.inboundFlows {
			if f.serviceID == serviceID {
				delete(p.inboundFlows, id)
				f.close()
			}
		}
		p.mu.Unlock()
	}
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

// hostedLookup resolves the local target port for a stream, verifying the
// service exists and epoch+port match.
func (n *Node) hostedLookup(serviceID string, epoch uint64, port uint32, proto meshv1.Protocol) (uint32, meshv1.RejectReason) {
	n.mu.RLock()
	hs := n.hosted[serviceID]
	n.mu.RUnlock()
	if hs == nil {
		return 0, meshv1.RejectReason_REJECT_REASON_UNKNOWN_SERVICE
	}
	if hs.svc.GetEpoch() != epoch {
		return 0, meshv1.RejectReason_REJECT_REASON_STALE_EPOCH
	}
	for _, hp := range hs.svc.GetPorts() {
		if hp.GetPort() == port && hp.GetProtocol() == proto {
			return hp.GetTargetPort(), 0
		}
	}
	return 0, meshv1.RejectReason_REJECT_REASON_UNKNOWN_PORT
}

// handleServiceStream is the host side of a TCP ServiceStream.
func (p *peerState) handleServiceStream(st *quic.Stream, s *meshv1.ServiceStream) {
	target, reason := p.n.hostedLookup(s.GetServiceId(), s.GetEpoch(), s.GetPort(), s.GetProtocol())
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
	target, reason := p.n.hostedLookup(uf.GetServiceId(), uf.GetEpoch(), uf.GetPort(), meshv1.Protocol_PROTOCOL_UDP)
	if reason != 0 {
		p.n.emitRouteRejected(uf.GetServiceId(), p.id, reason.String())
		rejectStream(st, reason, "not hosted here")
		return
	}
	c, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(target)})
	if err != nil {
		rejectStream(st, meshv1.RejectReason_REJECT_REASON_TARGET_UNREACHABLE, err.Error())
		return
	}
	if err := acceptStream(st); err != nil {
		_ = c.Close()
		return
	}
	f := &inboundFlow{serviceID: uf.GetServiceId(), target: c, done: make(chan struct{})}
	p.mu.Lock()
	old := p.inboundFlows[uf.GetFlowId()]
	p.inboundFlows[uf.GetFlowId()] = f
	p.mu.Unlock()
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
			if err := conn.SendDatagram(meshproto.EncodeFlowDatagram(uf.GetFlowId()|flowReplyBit, buf[:nr])); err != nil {
				// same policy as udpFlow.send: oversized datagrams are
				// dropped and counted, the flow stays alive
				if isDatagramTooLarge(err) {
					rtMetricDrop(p.n)
					continue
				}
				return
			}
			p.bytesTx.Add(uint64(nr))
		}
	}()
	// closing the stream ends the flow
	go func() {
		_, _ = st.Read(make([]byte, 1))
		f.close()
		p.mu.Lock()
		delete(p.inboundFlows, uf.GetFlowId())
		p.mu.Unlock()
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

// flowReplyBit distinguishes host→client replies from client→host payloads.
// Flow ids are allocated by the client that opened the flow; both directions
// carry the same id, so the host ORs in this bit when replying — otherwise a
// peer that is client of one of our services while we are client of one of
// its services could see identical ids in both maps.
const flowReplyBit uint64 = 1 << 63

// deliverDatagram finds the owning flow: client-side outbound flows get
// replies (bit set); host-side inbound flows get the opener's payloads.
func (p *peerState) deliverDatagram(flowID uint64, payload []byte) {
	p.mu.Lock()
	if flowID&flowReplyBit != 0 {
		f := p.outboundFlows[flowID&^flowReplyBit]
		p.mu.Unlock()
		if f != nil {
			f.deliver(payload)
		}
		return
	}
	f := p.inboundFlows[flowID]
	p.mu.Unlock()
	if f != nil {
		_, _ = f.target.Write(payload)
	}
}

// deliver writes a host reply back to the client socket that opened the
// flow (client side).
func (f *udpFlow) deliver(payload []byte) {
	f.touch()
	if f.rt != nil {
		f.rt.mu.Lock()
		c := f.rt.udpLn[f.port]
		f.rt.mu.Unlock()
		if c != nil {
			_, _ = c.WriteToUDP(payload, f.src)
		}
	}
}
