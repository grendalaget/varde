package main

import (
	"context"
	"encoding/base64"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"

	meshv1 "github.com/grendalaget/varde/go/gen/mesh/v1"
)

// peerState tracks one remote node: candidate endpoints, the active
// connection, path kind, RTT EWMA and byte counters.
type peerState struct {
	n      *Node
	id     string
	pubB64 string
	pub    []byte

	mu            sync.Mutex
	endpoints     []string
	relayIDs      []string
	conn          *quic.Conn          // active connection
	outboundFlows map[uint64]*udpFlow // flow_id → client-side flow on this peer
	kind          meshv1.PathKind
	relayID       string // set when kind==RELAYED
	rttUS         int64  // EWMA
	bytesTx       atomic.Uint64
	bytesRx       atomic.Uint64
	lastSeen      atomic.Int64

	stopCh  chan struct{}
	stopped atomic.Bool
	wg      sync.WaitGroup
	dialing atomic.Bool
}

func newPeerState(n *Node, p *meshv1.Peer) *peerState {
	return &peerState{
		n: n, id: p.GetNodeId(),
		pub:           p.GetPublicKey(),
		pubB64:        base64.StdEncoding.EncodeToString(p.GetPublicKey()),
		endpoints:     p.GetEndpoints(),
		relayIDs:      p.GetRelayIds(),
		kind:          meshv1.PathKind_PATH_KIND_NONE,
		stopCh:        make(chan struct{}),
		outboundFlows: map[uint64]*udpFlow{},
	}
}

func (p *peerState) update(p2 *meshv1.Peer) {
	p.mu.Lock()
	p.endpoints = p2.GetEndpoints()
	p.relayIDs = p2.GetRelayIds()
	p.mu.Unlock()
}

func (p *peerState) stop() {
	if p.stopped.CompareAndSwap(false, true) {
		close(p.stopCh)
	}
	p.mu.Lock()
	c := p.conn
	p.conn = nil
	p.mu.Unlock()
	if c != nil {
		_ = c.CloseWithError(0, "peer removed")
	}
	p.wg.Wait()
}

func (p *peerState) current() *quic.Conn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn
}

// start launches the path manager.
func (p *peerState) start() {
	p.wg.Add(1)
	go p.manage()
}

// installConn registers a new connection; applies the dedupe rule: when both
// ends dial, the connection initiated by the lexicographically smaller
// node_id wins.
func (p *peerState) installConn(conn *quic.Conn, kind meshv1.PathKind, inbound bool) {
	newInitiator := p.n.nodeID
	if inbound {
		newInitiator = p.id
	}
	p.mu.Lock()
	old := p.conn
	p.mu.Unlock()
	if old != nil && old != conn {
		oldInitiator := p.n.nodeID
		if p.connInboundFlag(old) {
			oldInitiator = p.id
		}
		if newInitiator > oldInitiator {
			_ = conn.CloseWithError(3, "duplicate connection")
			return
		}
		_ = old.CloseWithError(3, "superseded")
	}
	p.mu.Lock()
	p.conn = conn
	prevKind := p.kind
	p.kind = kind
	p.mu.Unlock()
	p.lastSeen.Store(time.Now().UnixMilli())
	p.setInbound(conn, inbound)
	p.wg.Add(1)
	go p.serveConn(conn, kind)
	if prevKind != kind {
		p.n.events.emit(&meshv1.MeshEvent{
			AtUnixMs: time.Now().UnixMilli(),
			Event:    &meshv1.MeshEvent_PeerPathChanged{PeerPathChanged: p.state()},
		})
	}
}

// inbound flags are stored per-conn in a side map on the node.
func (p *peerState) connInboundFlag(c *quic.Conn) bool {
	if v, ok := p.n.connInbound.Load(c); ok {
		return v.(bool)
	}
	return false
}

func (p *peerState) setInbound(c *quic.Conn, inbound bool) {
	p.n.connInbound.Store(c, inbound)
}

func (p *peerState) registerOutboundFlow(flowID uint64, f *udpFlow) {
	p.mu.Lock()
	p.outboundFlows[flowID] = f
	p.mu.Unlock()
}

func (p *peerState) removeOutboundFlow(flowID uint64) {
	p.mu.Lock()
	delete(p.outboundFlows, flowID)
	p.mu.Unlock()
}

// manage keeps a connection to the peer alive: direct dials, relay fallback,
// then direct retries while relayed.
func (p *peerState) manage() {
	defer p.wg.Done()
	for {
		select {
		case <-p.stopCh:
			return
		default:
		}
		if c := p.current(); c != nil {
			// wait until it dies
			select {
			case <-p.stopCh:
				return
			case <-c.Context().Done():
			}
			p.mu.Lock()
			if p.conn == c {
				p.conn = nil
				p.kind = meshv1.PathKind_PATH_KIND_NONE
				p.relayID = ""
			}
			p.mu.Unlock()
			p.n.events.emit(&meshv1.MeshEvent{
				AtUnixMs: time.Now().UnixMilli(),
				Event:    &meshv1.MeshEvent_PeerPathChanged{PeerPathChanged: p.state()},
			})
			continue
		}
		// establish a path
		if p.dialing.CompareAndSwap(false, true) {
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				defer p.dialing.Store(false)
				p.establish()
			}()
		}
		select {
		case <-p.stopCh:
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (p *peerState) establish() {
	if !p.n.isForceRelay() {
		if p.current() != nil || p.stopped.Load() {
			return
		}
		// concurrent dials to every candidate; the round has a total budget —
		// QUIC's own retransmission provides the 500 ms pacing inside it
		if p.dialDirectOnce() {
			return
		}
	}
	// relay fallback
	if p.current() == nil {
		p.dialRelayed()
	}
	// while relayed, retry direct periodically
	if p.current() != nil {
		p.retryDirectLoop()
	}
}

func (p *peerState) getEndpoints() []*net.UDPAddr {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*net.UDPAddr
	for _, e := range p.endpoints {
		if a, err := resolveUDP(e); err == nil {
			out = append(out, a)
		}
	}
	return out
}

// dialDirectOnce tries every candidate endpoint concurrently for up to the
// configured direct budget (interval * rounds; 5 s by default). Returns on
// the first connection that lands.
func (p *peerState) dialDirectOnce() bool {
	eps := p.getEndpoints()
	if len(eps) == 0 {
		p.n.log.Debug("no endpoints to dial", "peer", p.id)
		return false
	}
	budget := p.n.timings.DirectDialInterval * time.Duration(p.n.timings.DirectDialRounds)
	ctx, cancel := context.WithTimeout(p.stopChCtx(), budget)
	defer cancel()
	var wg sync.WaitGroup
	var won atomic.Bool
	for _, ep := range eps {
		wg.Add(1)
		go func(addr *net.UDPAddr) {
			defer wg.Done()
			conn, err := p.n.tr.Dial(ctx, addr, p.n.tlsConfigFor(p.id), quicConfig())
			if err != nil {
				p.n.log.Debug("direct dial failed", "peer", p.id, "addr", addr, "error", err)
				return
			}
			if won.CompareAndSwap(false, true) {
				cancel()
				p.installConn(conn, meshv1.PathKind_PATH_KIND_DIRECT, false)
			} else {
				_ = conn.CloseWithError(4, "another dial won")
			}
		}(ep)
	}
	wg.Wait()
	return p.current() != nil && p.kindIs(meshv1.PathKind_PATH_KIND_DIRECT)
}

func (p *peerState) kindIs(k meshv1.PathKind) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.kind == k
}

// dialRelayed picks the lowest-RTT relay we share with the peer and dials a
// QUIC connection over the virtual relay PacketConn.
func (p *peerState) dialRelayed() bool {
	rc := p.pickRelay()
	if rc == nil {
		p.n.log.Debug("no shared registered relay", "peer", p.id)
		return false
	}
	conn, err := rc.dial(p.stopChCtx(), p.id, p.n.tlsConfigFor(p.id))
	if err != nil {
		p.n.log.Debug("relayed dial failed", "peer", p.id, "relay", rc.id, "error", err)
		return false
	}
	p.mu.Lock()
	p.relayID = rc.id
	p.mu.Unlock()
	p.installConn(conn, meshv1.PathKind_PATH_KIND_RELAYED, false)
	return p.current() != nil
}

func (p *peerState) pickRelay() *relayClient {
	p.mu.Lock()
	shared := map[string]bool{}
	for _, id := range p.relayIDs {
		shared[id] = true
	}
	p.mu.Unlock()
	p.n.mu.RLock()
	defer p.n.mu.RUnlock()
	var best *relayClient
	var bestRTT int64
	for id, rc := range p.n.relays {
		if !shared[id] || !rc.registered() {
			continue
		}
		rtt := rc.rtt()
		if best == nil || rtt < bestRTT {
			best, bestRTT = rc, rtt
		}
	}
	return best
}

// retryDirectLoop retries direct while we are relayed.
func (p *peerState) retryDirectLoop() {
	if p.n.isForceRelay() {
		return
	}
	t := time.NewTicker(p.n.timings.RelayedRetryDirect)
	defer t.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-p.current().Context().Done():
			return
		case <-t.C:
			if !p.kindIs(meshv1.PathKind_PATH_KIND_RELAYED) {
				return
			}
			if p.dialDirectOnce() {
				return // direct installed; relayed conn will be closed by swap
			}
		}
	}
}

func (p *peerState) stopChCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-p.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx
}

func (p *peerState) state() *meshv1.PeerState {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := &meshv1.PeerState{
		NodeId: p.id, Path: p.kind, RttUs: p.rttUS,
		BytesTx: p.bytesTx.Load(), BytesRx: p.bytesRx.Load(),
		LastSeenUnixMs: p.lastSeen.Load(), RelayId: p.relayID,
	}
	if p.conn != nil {
		st.RemoteAddr = p.conn.RemoteAddr().String()
	}
	return st
}

// serveConn runs accept loops for streams + datagrams and the control stream.
func (p *peerState) serveConn(conn *quic.Conn, kind meshv1.PathKind) {
	defer p.wg.Done()
	p.wg.Add(2)
	go p.streamAcceptLoop(conn)
	go p.datagramLoop(conn)
	go p.controlLoop(conn)
	<-conn.Context().Done()
}
