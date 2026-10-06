package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"

	meshv1 "github.com/grendalaget/varde/go/gen/mesh/v1"
	"github.com/grendalaget/varde/go/identity"
)

// Timings are injectable for tests.
type Timings struct {
	DirectDialInterval time.Duration // between concurrent dial rounds
	DirectDialRounds   int           // rounds before giving up (500ms*10 = 5s)
	RelayedRetryDirect time.Duration // re-try direct while relayed
	ControlPingEvery   time.Duration
	RelayRegisterEvery time.Duration
	RelayPingEvery     time.Duration
	UDPFlowIdle        time.Duration
}

func DefaultTimings() Timings {
	return Timings{
		DirectDialInterval: 500 * time.Millisecond,
		DirectDialRounds:   10,
		RelayedRetryDirect: 30 * time.Second,
		ControlPingEvery:   5 * time.Second,
		RelayRegisterEvery: 25 * time.Second,
		RelayPingEvery:     5 * time.Second,
		UDPFlowIdle:        60 * time.Second,
	}
}

// sharedSock wraps the UDP socket handed to quic.Transport. All our own writes
// (relay frames) go through the same socket; WriteTo drops nothing itself, but
// blockDirect is a test hook that silently drops direct (non-relay) packets.
type sharedSock struct {
	udp *net.UDPConn

	relayMu     sync.RWMutex
	relayAddrs  map[string]bool // "ip:port"
	blockDirect atomic.Bool
}

func (s *sharedSock) isRelay(addr net.Addr) bool {
	s.relayMu.RLock()
	defer s.relayMu.RUnlock()
	return s.relayAddrs[addr.String()]
}

func (s *sharedSock) WriteTo(p []byte, addr net.Addr) (int, error) {
	if s.blockDirect.Load() && !s.isRelay(addr) {
		return len(p), nil // dropped: simulated dead direct path
	}
	return s.udp.WriteTo(p, addr)
}

func (s *sharedSock) WriteToUDP(p []byte, addr *net.UDPAddr) (int, error) {
	return s.udp.WriteToUDP(p, addr)
}

func (s *sharedSock) setRelay(addr string, on bool) {
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	if on {
		s.relayAddrs[addr] = true
	} else {
		delete(s.relayAddrs, addr)
	}
}

// net.PacketConn delegation.
func (s *sharedSock) ReadFrom(p []byte) (int, net.Addr, error) { return s.udp.ReadFrom(p) }
func (s *sharedSock) Close() error                             { return s.udp.Close() }
func (s *sharedSock) LocalAddr() net.Addr                      { return s.udp.LocalAddr() }
func (s *sharedSock) SetDeadline(t time.Time) error            { return s.udp.SetDeadline(t) }
func (s *sharedSock) SetReadDeadline(t time.Time) error        { return s.udp.SetReadDeadline(t) }
func (s *sharedSock) SetWriteDeadline(t time.Time) error       { return s.udp.SetWriteDeadline(t) }

// Node is the whole mesh state for this process.
type Node struct {
	log     *slog.Logger
	timings Timings

	mu          sync.RWMutex
	nodeID      string
	priv        ed25519.PrivateKey
	cert        tls.Certificate
	configured  bool
	forceRelay  bool
	sock        *sharedSock
	tr          *quic.Transport
	listener    *quic.Listener
	udp         *net.UDPConn
	peers       map[string]*peerState
	peersByPub  map[string]*peerState // b64 pubkey → peer
	relays      map[string]*relayClient
	routes      map[string]*routeState // service_id → route
	hosted      map[string]*hostedService
	internal    map[string]uint32 // name → target port
	forwards    map[string]*internalForward
	startedAt   time.Time
	events      *eventBus
	ctx         context.Context
	cancel      context.CancelFunc
	metrics     *metrics
	connInbound sync.Map // *quic.Conn → bool (was inbound)
}

func NewNode(log *slog.Logger, timings Timings, reg *promReg) *Node {
	n := &Node{
		log:        log,
		timings:    timings,
		peers:      map[string]*peerState{},
		peersByPub: map[string]*peerState{},
		relays:     map[string]*relayClient{},
		routes:     map[string]*routeState{},
		hosted:     map[string]*hostedService{},
		internal:   map[string]uint32{},
		forwards:   map[string]*internalForward{},
		events:     newEventBus(),
		startedAt:  time.Now(),
	}
	n.metrics = newMetrics(reg, n)
	return n
}

// Configure binds the UDP socket, transport and listener on first call; later
// calls update node_id-independent settings (relays, force_relay) without
// dropping peer connections.
func (n *Node) Configure(req *meshv1.ConfigureRequest) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if !n.configured {
		priv, err := identity.LoadPrivateKey(req.GetIdentityKeyPath())
		if err != nil {
			return fmt.Errorf("load identity: %w", err)
		}
		cert, err := selfSignedCert(priv)
		if err != nil {
			return err
		}
		port := int(req.GetListenPort())
		addr := &net.UDPAddr{IP: net.IPv6unspecified, Port: port}
		udp, err := net.ListenUDP("udp", addr)
		if err != nil && port != 0 {
			// fallback to ephemeral per spec
			addr.Port = 0
			udp, err = net.ListenUDP("udp", addr)
		}
		if err != nil {
			return fmt.Errorf("udp listen: %w", err)
		}
		n.priv = priv
		n.cert = cert
		n.nodeID = req.GetNodeId()
		n.udp = udp
		n.sock = &sharedSock{udp: udp, relayAddrs: map[string]bool{}}
		n.ctx, n.cancel = context.WithCancel(context.Background())
		n.tr = &quic.Transport{Conn: n.sock}
		ln, err := n.tr.Listen(n.tlsConfigFor(""), quicConfig())
		if err != nil {
			return fmt.Errorf("quic listen: %w", err)
		}
		n.listener = ln
		n.configured = true
		go n.acceptLoop()
		go n.relayDemux()
	}
	n.nodeID = req.GetNodeId()
	n.forceRelay = req.GetForceRelay()

	// reconcile relay set (must not disturb existing peer conns)
	want := map[string]*meshv1.Relay{}
	for _, r := range req.GetRelays() {
		want[r.GetRelayId()] = r
	}
	var removed []*relayClient
	for id, rc := range n.relays {
		if w, ok := want[id]; ok && w.GetAddr() == rc.addr.String() {
			rc.setToken(w.GetToken())
			delete(want, id)
			continue
		}
		removed = append(removed, rc)
		delete(n.relays, id)
		n.sock.setRelay(rc.addr.String(), false)
	}
	for id, r := range want {
		rc, err := n.newRelayClientLocked(r.GetRelayId(), r.GetAddr(), r.GetToken())
		if err != nil {
			n.log.Error("relay client", "id", id, "error", err)
			continue
		}
		n.relays[id] = rc
		n.sock.setRelay(rc.addr.String(), true)
	}
	n.mu.Unlock()
	for _, rc := range removed {
		rc.stop()
	}
	n.mu.Lock()
	return nil
}

func quicConfig() *quic.Config {
	return &quic.Config{
		MaxIdleTimeout:       30 * time.Second,
		KeepAlivePeriod:      10 * time.Second,
		HandshakeIdleTimeout: 5 * time.Second,
		EnableDatagrams:      true,
	}
}

func (n *Node) Close() {
	if n.cancel != nil {
		n.cancel()
	}
	// snapshot under the lock, stop after releasing it: peer.stop waits on
	// goroutines that take n.mu.RLock (e.g. pickRelay) — holding the lock
	// here would deadlock.
	n.mu.Lock()
	peers := make([]*peerState, 0, len(n.peers))
	for _, p := range n.peers {
		peers = append(peers, p)
	}
	relays := make([]*relayClient, 0, len(n.relays))
	for _, r := range n.relays {
		relays = append(relays, r)
	}
	routes := make([]*routeState, 0, len(n.routes))
	for _, rt := range n.routes {
		routes = append(routes, rt)
	}
	forwards := make([]*internalForward, 0, len(n.forwards))
	for _, f := range n.forwards {
		forwards = append(forwards, f)
	}
	n.mu.Unlock()

	for _, p := range peers {
		p.stop()
	}
	for _, r := range relays {
		r.stop()
	}
	for _, rt := range routes {
		rt.closeAll()
	}
	for _, f := range forwards {
		f.close()
	}
	if n.listener != nil {
		_ = n.listener.Close()
	}
	if n.tr != nil {
		_ = n.tr.Close()
	}
	if n.udp != nil {
		_ = n.udp.Close()
	}
}

// localEndpoints lists candidate listen endpoints: interface addresses,
// skipping loopback and link-local v6.
func (n *Node) localEndpoints() []string {
	port := n.udp.LocalAddr().(*net.UDPAddr).Port
	out := []string{}
	ifs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range ifs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		default:
			continue
		}
		if ip.IsLoopback() {
			continue
		}
		if ip4 := ip.To4(); ip4 != nil {
			out = append(out, fmt.Sprintf("%s:%d", ip4, port))
		} else if !ip.IsLinkLocalUnicast() {
			out = append(out, fmt.Sprintf("[%s]:%d", ip, port))
		}
	}
	return out
}

// status fills MeshStatus.
func (n *Node) status() *meshv1.MeshStatus {
	n.mu.RLock()
	defer n.mu.RUnlock()
	st := &meshv1.MeshStatus{
		NodeId:          n.nodeID,
		Version:         version,
		Configured:      n.configured,
		StartedAtUnixMs: n.startedAt.UnixMilli(),
	}
	if n.udp != nil {
		st.ListenAddrs = append(n.localEndpoints(), n.udp.LocalAddr().String())
	}
	var observed []string
	var relays []*meshv1.RelayState
	for _, r := range n.relays {
		rs, obs := r.state()
		relays = append(relays, rs)
		observed = append(observed, obs...)
	}
	st.Relays = relays
	st.ObservedEndpoints = dedupe(observed)
	return st
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ---- inbound connection handling ----

func (n *Node) acceptLoop() {
	for {
		conn, err := n.listener.Accept(n.ctx)
		if err != nil {
			return
		}
		go n.registerConn(conn, meshv1.PathKind_PATH_KIND_DIRECT)
	}
}

// registerConn identifies the peer from its certificate and installs the
// connection, applying the simultaneous-dial dedupe rule.
func (n *Node) registerConn(conn *quic.Conn, kind meshv1.PathKind) {
	<-conn.HandshakeComplete()
	b64, _, err := peerPub(conn.ConnectionState().TLS.PeerCertificates)
	if err != nil {
		_ = conn.CloseWithError(1, "no cert")
		return
	}
	n.mu.RLock()
	p := n.peersByPub[b64]
	n.mu.RUnlock()
	if p == nil {
		_ = conn.CloseWithError(2, "unknown peer")
		return
	}
	p.installConn(conn, kind, true)
}

// ---- non-QUIC packet demux (relay frames on the shared socket) ----

func (n *Node) relayDemux() {
	buf := make([]byte, 64*1024)
	for {
		bn, addr, err := n.tr.ReadNonQUICPacket(n.ctx, buf)
		if err != nil {
			return
		}
		data := append([]byte(nil), buf[:bn]...)
		n.handleRelayFrame(addr, data)
	}
}

func (n *Node) handleRelayFrame(addr net.Addr, data []byte) {
	if len(data) == 0 {
		return
	}
	n.mu.RLock()
	var rc *relayClient
	for _, r := range n.relays {
		if r.addr.String() == addr.String() {
			rc = r
			break
		}
	}
	n.mu.RUnlock()
	if rc == nil {
		return // not from a known relay
	}
	rc.handleFrame(data)
}

// resolveUDP resolves "ip:port"; tolerates bare loopback strings.
func resolveUDP(s string) (*net.UDPAddr, error) {
	return net.ResolveUDPAddr("udp", s)
}

func (n *Node) emitRouteRejected(serviceID, peerID, reason string) {
	n.events.emit(&meshv1.MeshEvent{
		AtUnixMs: time.Now().UnixMilli(),
		Event: &meshv1.MeshEvent_RouteRejected{RouteRejected: &meshv1.RouteRejected{
			ServiceId: serviceID, PeerNodeId: peerID, Reason: reason,
		}},
	})
}

var _ = strings.TrimSpace // keep import for helpers
