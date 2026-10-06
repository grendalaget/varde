package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"google.golang.org/grpc"

	meshv1 "github.com/grendalaget/varde/go/gen/mesh/v1"
	"github.com/grendalaget/varde/go/identity"
	"github.com/grendalaget/varde/go/relay"
	"github.com/grendalaget/varde/go/relaytoken"
)

var testTimings = Timings{
	DirectDialInterval: 50 * time.Millisecond,
	DirectDialRounds:   30,
	RelayedRetryDirect: 400 * time.Millisecond,
	ControlPingEvery:   150 * time.Millisecond,
	RelayRegisterEvery: 150 * time.Millisecond,
	RelayPingEvery:     100 * time.Millisecond,
	UDPFlowIdle:        60 * time.Second,
}

func testLog() *slog.Logger {
	if os.Getenv("MESH_TEST_VERBOSE") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testNode is a mesh instance with its gRPC API over a temp UDS.
type testNode struct {
	n    *Node
	id   string
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
	cli  meshv1.MeshServiceClient
	grpc *grpc.Server
	conn *grpc.ClientConn
}

func newTestNode(t *testing.T, id string) *testNode {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "node.key")
	pem, err := identity.MarshalPrivateKeyPEM(priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem, 0o600); err != nil {
		t.Fatal(err)
	}
	n := NewNode(testLog(), testTimings, nil)
	sock := ipcSockPath(dir)
	lis, err := listenIPC(sock)
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	meshv1.RegisterMeshServiceServer(gs, newMeshServer(testLog(), n))
	go func() { _ = gs.Serve(lis) }()
	cc, err := grpcIPCClient(sock)
	if err != nil {
		t.Fatal(err)
	}
	tn := &testNode{n: n, id: id, priv: priv, pub: pub, cli: meshv1.NewMeshServiceClient(cc), grpc: gs, conn: cc}
	t.Cleanup(func() { _ = cc.Close(); gs.Stop(); n.Close() })
	return tn
}

func (tn *testNode) configure(t *testing.T, relays []*meshv1.Relay, forceRelay bool) {
	t.Helper()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "node.key")
	pem, _ := identity.MarshalPrivateKeyPEM(tn.priv)
	if err := os.WriteFile(keyPath, pem, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := tn.cli.Configure(context.Background(), &meshv1.ConfigureRequest{
		NodeId: tn.id, IdentityKeyPath: keyPath, ListenPort: 0,
		Relays: relays, ForceRelay: forceRelay,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (tn *testNode) listenAddr(t *testing.T) string {
	t.Helper()
	st, err := tn.cli.GetStatus(context.Background(), &meshv1.GetStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.GetListenAddrs()) == 0 {
		t.Fatal("no listen addrs")
	}
	return st.GetListenAddrs()[len(st.GetListenAddrs())-1] // last is the raw bound addr
}

var _ = (*testNode).listenAddr // helper kept for tests that need the bound addr

func (tn *testNode) setPeers(t *testing.T, peers ...*meshv1.Peer) {
	t.Helper()
	if _, err := tn.cli.SetPeers(context.Background(), &meshv1.SetPeersRequest{Peers: peers}); err != nil {
		t.Fatal(err)
	}
}

func peerOf(tn *testNode, relayIDs ...string) *meshv1.Peer {
	port := tn.n.udp.LocalAddr().(*net.UDPAddr).Port
	return &meshv1.Peer{
		NodeId:    tn.id,
		PublicKey: tn.pub,
		Endpoints: []string{fmt.Sprintf("127.0.0.1:%d", port)},
		RelayIds:  relayIDs,
	}
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func pathOf(t *testing.T, tn *testNode, peerID string) meshv1.PathKind {
	t.Helper()
	resp, err := tn.cli.ListPeers(context.Background(), &meshv1.ListPeersRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range resp.GetPeers() {
		if p.GetNodeId() == peerID {
			return p.GetPath()
		}
	}
	return meshv1.PathKind_PATH_KIND_NONE
}

func newPeerPair(t *testing.T) (*testNode, *testNode, *peerState, *peerState) {
	t.Helper()
	a := newTestNode(t, "node_a")
	b := newTestNode(t, "node_b")
	a.configure(t, nil, false)
	b.configure(t, nil, false)
	pa := newPeerState(a.n, peerOf(b))
	pb := newPeerState(b.n, peerOf(a))
	for _, item := range []struct {
		node *testNode
		peer *peerState
	}{{a, pa}, {b, pb}} {
		item.node.n.mu.Lock()
		item.node.n.peers[item.peer.id] = item.peer
		item.node.n.peersByPub[item.peer.pubB64] = item.peer
		item.node.n.mu.Unlock()
	}
	t.Cleanup(func() {
		pa.stop()
		pb.stop()
	})
	return a, b, pa, pb
}

func dialPeerForTest(t *testing.T, from, to *testNode, localPeer *peerState) *quic.Conn {
	t.Helper()
	addr, err := net.ResolveUDPAddr("udp", peerOf(to).GetEndpoints()[0])
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := from.n.tr.Dial(ctx, addr, from.n.tlsConfigFor(to.id), quicConfig())
	if err != nil {
		t.Fatal(err)
	}
	localPeer.installConn(conn, meshv1.PathKind_PATH_KIND_DIRECT, false)
	remotePeer := to.n.peer(from.id)
	waitFor(t, "inbound test connection", 5*time.Second, func() bool {
		return remotePeer != nil && remotePeer.current() != nil
	})
	return conn
}

func setPeerConnInstalledAt(peer *peerState, installedAt time.Time) {
	peer.mu.Lock()
	peer.connInstalledAt = installedAt
	peer.mu.Unlock()
}

// testRelay is a running go/relay server + its CP keypair.
type testRelay struct {
	srv  *relay.Server
	id   string
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	addr string
	seen chan []byte
}

func newTestRelay(t *testing.T, id string) *testRelay {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	tr := &testRelay{id: id, pub: pub, priv: priv, seen: make(chan []byte, 4096)}
	s, err := relay.New(relay.Config{
		ID: id, PublicKey: pub, Conn: conn, Log: testLog(),
		OnForward: func(p []byte) {
			select {
			case tr.seen <- append([]byte(nil), p...):
			default:
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	tr.srv = s
	tr.addr = conn.LocalAddr().String()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Serve(ctx) }()
	t.Cleanup(func() { cancel() })
	return tr
}

func (tr *testRelay) relayFor(tn *testNode, groupID string) *meshv1.Relay {
	tok, err := relaytoken.Sign(tr.priv, relaytoken.Claims{
		NodeID: tn.id, GroupID: groupID, RelayID: tr.id,
		PublicKey: base64.StdEncoding.EncodeToString(tn.pub),
		ExpUnixMs: time.Now().Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		panic(err)
	}
	return &meshv1.Relay{RelayId: tr.id, Addr: tr.addr, Token: tok}
}

// ---- echo servers ----

func startTCPEcho(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

func startUDPEcho(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		buf := make([]byte, 64*1024)
		for {
			nr, src, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = c.WriteToUDP(buf[:nr], src)
		}
	}()
	t.Cleanup(func() { _ = c.Close() })
	return c.LocalAddr().(*net.UDPAddr).Port
}

func routeFor(svcID, ip string, port uint32, host string, epoch uint64, udpPort uint32) *meshv1.ServiceRoute {
	ports := []*meshv1.PortSpec{{Port: port, Protocol: meshv1.Protocol_PROTOCOL_TCP}}
	if udpPort != 0 {
		ports = append(ports, &meshv1.PortSpec{Port: udpPort, Protocol: meshv1.Protocol_PROTOCOL_UDP})
	}
	return &meshv1.ServiceRoute{
		ServiceId: svcID, LoopbackIp: ip, Ports: ports,
		HostNodeId: host, Epoch: epoch,
	}
}

func hostedFor(svcID string, epoch uint64, tcpPort, tcpTarget, udpPort, udpTarget uint32) *meshv1.HostedService {
	ports := []*meshv1.HostedPort{{Port: tcpPort, Protocol: meshv1.Protocol_PROTOCOL_TCP, TargetPort: tcpTarget}}
	if udpPort != 0 {
		ports = append(ports, &meshv1.HostedPort{Port: udpPort, Protocol: meshv1.Protocol_PROTOCOL_UDP, TargetPort: udpTarget})
	}
	return &meshv1.HostedService{ServiceId: svcID, Epoch: epoch, Ports: ports}
}

// ===================== tests =====================

func TestDirectConnectAndAuth(t *testing.T) {
	a := newTestNode(t, "node_a")
	b := newTestNode(t, "node_b")
	a.configure(t, nil, false)
	b.configure(t, nil, false)
	a.setPeers(t, peerOf(b))
	b.setPeers(t, peerOf(a))

	waitFor(t, "direct path", 10*time.Second, func() bool {
		return pathOf(t, a, "node_b") == meshv1.PathKind_PATH_KIND_DIRECT &&
			pathOf(t, b, "node_a") == meshv1.PathKind_PATH_KIND_DIRECT
	})
	// RTT measured via control stream
	waitFor(t, "rtt", 5*time.Second, func() bool {
		resp, _ := a.cli.ListPeers(context.Background(), &meshv1.ListPeersRequest{})
		for _, p := range resp.GetPeers() {
			if p.GetNodeId() == "node_b" {
				return p.GetRttUs() > 0
			}
		}
		return false
	})

	// unknown key rejected in both directions: c isn't in either peer set.
	// The security invariant is on the accept side: a must NEVER install a
	// conn for c. (c's side may transiently hold a zombie conn — the client
	// learns of the rejection only via the server's close — and a's own
	// client-side rejection is covered by the mismatched-key case below.)
	c := newTestNode(t, "node_c")
	c.configure(t, nil, false)
	c.setPeers(t, peerOf(a)) // c tries to join a's mesh
	for i := 0; i < 20; i++ {
		if p := pathOf(t, a, "node_c"); p != meshv1.PathKind_PATH_KIND_NONE {
			t.Fatalf("a accepted unknown peer: %v", p)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// mismatched key: d believes a has a different public key
	d := newTestNode(t, "node_d")
	d.configure(t, nil, false)
	wrongPub, _, _ := ed25519.GenerateKey(rand.Reader)
	wrong := peerOf(a)
	wrong.PublicKey = wrongPub
	d.setPeers(t, wrong)
	a.setPeers(t, peerOf(b), peerOf(d)) // a knows d's real key
	directSeen := 0
	for i := 0; i < 20; i++ {
		if p := pathOf(t, a, "node_d"); p == meshv1.PathKind_PATH_KIND_DIRECT {
			// d's own key is correct so a may accept the conn; the peer set is
			// legit — that's fine (auth is by key, and d's key IS in a's set)
			break
		}
		if pathOf(t, d, "node_a") != meshv1.PathKind_PATH_KIND_NONE {
			directSeen++
		}
		time.Sleep(100 * time.Millisecond)
	}
	if directSeen > 4 {
		t.Fatalf("d held a connection despite wrong expected key (%d/20)", directSeen)
	}
}

func TestDedupeRacePrefersLexicographicallySmallerInitiator(t *testing.T) {
	a, b, pa, pb := newPeerPair(t)
	oldA := dialPeerForTest(t, a, b, pa)
	oldB := pb.current()
	if oldB == nil {
		t.Fatal("remote side did not install the first connection")
	}
	setPeerConnInstalledAt(pa, time.Now())
	setPeerConnInstalledAt(pb, time.Now())

	newB := dialPeerForTest(t, b, a, pb)
	waitFor(t, "duplicate connection close", time.Second, func() bool {
		return newB.Context().Err() != nil
	})
	if pa.current() != oldA || pb.current() != oldB {
		t.Fatal("fresh simultaneous-dial race did not retain the lower-id initiator")
	}
}

func TestOldConnectionOutsideDedupeWindowIsReplaced(t *testing.T) {
	a, b, pa, pb := newPeerPair(t)
	oldA := dialPeerForTest(t, a, b, pa)
	oldB := pb.current()
	installedAt := time.Now().Add(-dedupeRaceWindow - time.Second)
	setPeerConnInstalledAt(pa, installedAt)
	setPeerConnInstalledAt(pb, installedAt)

	newB := dialPeerForTest(t, b, a, pb)
	waitFor(t, "stale connection replacement", time.Second, func() bool {
		return pb.current() == newB && pa.current() != oldA && oldA.Context().Err() != nil
	})
	if oldB.Context().Err() == nil {
		t.Fatal("superseded stale connection remained open")
	}
}

func TestClosedConnectionIsReplaced(t *testing.T) {
	a, b, pa, pb := newPeerPair(t)
	oldA := dialPeerForTest(t, a, b, pa)
	oldB := pb.current()
	_ = oldB.CloseWithError(0, "test closed old connection")
	waitFor(t, "old connection close", time.Second, func() bool {
		return oldA.Context().Err() != nil
	})

	newB := dialPeerForTest(t, b, a, pb)
	waitFor(t, "closed connection replacement", time.Second, func() bool {
		return pb.current() == newB && pa.current() != oldA
	})
	if pa.current() == oldA || oldB.Context().Err() == nil {
		t.Fatal("closed old connection was not replaced")
	}
}

const (
	testSvcID = "svc_test"
	testIP    = "127.77.0.42"
	testTCP   = 27001
	testUDP   = 27002
)

func TestTCPEchoAndUDPEcho(t *testing.T) {
	a := newTestNode(t, "node_a")
	b := newTestNode(t, "node_b")
	a.configure(t, nil, false)
	b.configure(t, nil, false)
	a.setPeers(t, peerOf(b))
	b.setPeers(t, peerOf(a))
	waitFor(t, "direct", 10*time.Second, func() bool {
		return pathOf(t, a, "node_b") == meshv1.PathKind_PATH_KIND_DIRECT
	})

	tcpPort, udpPort := startTCPEcho(t), startUDPEcho(t)
	if _, err := b.cli.SetHostedServices(context.Background(), &meshv1.SetHostedServicesRequest{
		Services: []*meshv1.HostedService{
			hostedFor(testSvcID, 1, testTCP, uint32(tcpPort), testUDP, uint32(udpPort)),
		},
	}); err != nil {
		t.Fatal(err)
	}
	rr, err := a.cli.SetRoutes(context.Background(), &meshv1.SetRoutesRequest{
		Routes: []*meshv1.ServiceRoute{routeFor(testSvcID, testIP, testTCP, "node_b", 1, testUDP)},
	})
	if err != nil || len(rr.GetErrors()) > 0 {
		t.Fatalf("SetRoutes: %v %v", err, rr.GetErrors())
	}

	// 1 MiB TCP transfer with integrity check
	c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", testIP, testTCP), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 1<<20)
	_, _ = rand.Read(payload)
	want := sha256.Sum256(payload)
	go func() {
		_, _ = c.Write(payload)
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	got, err := io.ReadAll(io.LimitReader(c, int64(len(payload))))
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) != want {
		t.Fatalf("TCP integrity check failed: got %d bytes", len(got))
	}
	_ = c.Close()

	// concurrent UDP flows: 4 sockets, distinct flows
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			uc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = uc.Close() }()
			dst, _ := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", testIP, testUDP))
			msg := []byte(fmt.Sprintf("ping-%d", i))
			for attempt := 0; attempt < 20; attempt++ {
				if _, err := uc.WriteToUDP(msg, dst); err != nil {
					t.Error(err)
					return
				}
				_ = uc.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				buf := make([]byte, 2048)
				nr, _, err := uc.ReadFromUDP(buf)
				if err == nil && string(buf[:nr]) == string(msg) {
					return
				}
			}
			t.Errorf("udp flow %d never echoed", i)
		}(i)
	}
	wg.Wait()
}

func TestForceRelayEcho(t *testing.T) {
	rel := newTestRelay(t, "r1")
	a := newTestNode(t, "node_a")
	b := newTestNode(t, "node_b")
	a.configure(t, []*meshv1.Relay{rel.relayFor(a, "grp_1")}, true)
	b.configure(t, []*meshv1.Relay{rel.relayFor(b, "grp_1")}, true)
	a.setPeers(t, peerOf(b, "r1"))
	b.setPeers(t, peerOf(a, "r1"))

	waitFor(t, "relayed path", 15*time.Second, func() bool {
		return pathOf(t, a, "node_b") == meshv1.PathKind_PATH_KIND_RELAYED
	})
	// observed endpoints recorded
	waitFor(t, "observed endpoints", 5*time.Second, func() bool {
		st, _ := a.cli.GetStatus(context.Background(), &meshv1.GetStatusRequest{})
		return len(st.GetObservedEndpoints()) > 0 && len(st.GetRelays()) == 1 && st.GetRelays()[0].GetRegistered()
	})

	tcpPort := startTCPEcho(t)
	if _, err := b.cli.SetHostedServices(context.Background(), &meshv1.SetHostedServicesRequest{
		Services: []*meshv1.HostedService{hostedFor(testSvcID, 1, testTCP, uint32(tcpPort), 0, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	marker := "PLAINTEXT-MARKER-SENTINEL"
	markerBytes := []byte(marker)

	// UDP echo through relay
	udpPort := startUDPEcho(t)
	if _, err := b.cli.SetHostedServices(context.Background(), &meshv1.SetHostedServicesRequest{
		Services: []*meshv1.HostedService{hostedFor(testSvcID, 1, testTCP, uint32(tcpPort), testUDP, uint32(udpPort))},
	}); err != nil {
		t.Fatal(err)
	}
	if rr, err := a.cli.SetRoutes(context.Background(), &meshv1.SetRoutesRequest{
		Routes: []*meshv1.ServiceRoute{routeFor(testSvcID, testIP, testTCP, "node_b", 1, testUDP)},
	}); err != nil || len(rr.GetErrors()) > 0 {
		t.Fatalf("SetRoutes: %v %v", err, rr.GetErrors())
	}

	// TCP echo with the marker plaintext
	c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", testIP, testTCP), 8*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(markerBytes); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(markerBytes))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(buf), marker) {
		t.Fatal("echo lost")
	}
	_ = c.Close()

	// UDP echo through relay
	uc, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	defer func() { _ = uc.Close() }()
	dst, _ := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", testIP, testUDP))
	udpOK := false
	for i := 0; i < 30 && !udpOK; i++ {
		if _, err := uc.WriteToUDP(markerBytes, dst); err != nil {
			t.Fatal(err)
		}
		_ = uc.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		nb := make([]byte, 2048)
		nr, _, err := uc.ReadFromUDP(nb)
		if err == nil && strings.Contains(string(nb[:nr]), marker) {
			udpOK = true
		}
	}
	if !udpOK {
		t.Fatal("UDP echo through relay failed")
	}

	// relay saw only ciphertext: the marker must never appear in payloads
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case p := <-rel.seen:
			if bytes := string(p); strings.Contains(bytes, marker) {
				t.Fatal("relay observed plaintext marker")
			}
		case <-deadline:
			return
		}
	}
}

func TestRouteMoveAndStaleEpoch(t *testing.T) {
	a := newTestNode(t, "node_a")
	b := newTestNode(t, "node_b")
	cc := newTestNode(t, "node_c")
	for _, tn := range []*testNode{a, b, cc} {
		tn.configure(t, nil, false)
	}
	a.setPeers(t, peerOf(b), peerOf(cc))
	b.setPeers(t, peerOf(a), peerOf(cc))
	cc.setPeers(t, peerOf(a), peerOf(b))
	waitFor(t, "mesh", 10*time.Second, func() bool {
		return pathOf(t, a, "node_b") == meshv1.PathKind_PATH_KIND_DIRECT &&
			pathOf(t, a, "node_c") == meshv1.PathKind_PATH_KIND_DIRECT
	})

	echoB := startTCPEcho(t)
	echoC := startTCPEcho(t)
	if _, err := b.cli.SetHostedServices(context.Background(), &meshv1.SetHostedServicesRequest{
		Services: []*meshv1.HostedService{hostedFor(testSvcID, 1, testTCP, uint32(echoB), 0, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := cc.cli.SetHostedServices(context.Background(), &meshv1.SetHostedServicesRequest{
		Services: []*meshv1.HostedService{hostedFor(testSvcID, 2, testTCP, uint32(echoC), 0, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	if rr, err := a.cli.SetRoutes(context.Background(), &meshv1.SetRoutesRequest{
		Routes: []*meshv1.ServiceRoute{routeFor(testSvcID, testIP, testTCP, "node_b", 1, 0)},
	}); err != nil || len(rr.GetErrors()) > 0 {
		t.Fatalf("routes: %v", rr.GetErrors())
	}

	ping := func(wantAlive bool) {
		// a just-moved route can still be opening its upstream stream when
		// the first client connects; tolerate a few resets/timeouts on the
		// live-path checks (Windows surfaces them as forced closes)
		var err error
		for i := 0; ; i++ {
			var c net.Conn
			c, err = net.DialTimeout("tcp", fmt.Sprintf("%s:%d", testIP, testTCP), 8*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			_, _ = c.Write([]byte("hi"))
			buf := make([]byte, 2)
			_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
			_, err = io.ReadFull(c, buf)
			_ = c.Close()
			if !wantAlive || err == nil || i >= 5 {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
		if wantAlive && err != nil {
			t.Fatalf("expected echo, got %v", err)
		}
	}
	ping(true)

	// move route to host c, epoch 2 — same client address keeps working
	if rr, err := a.cli.SetRoutes(context.Background(), &meshv1.SetRoutesRequest{
		Routes: []*meshv1.ServiceRoute{routeFor(testSvcID, testIP, testTCP, "node_c", 2, 0)},
	}); err != nil || len(rr.GetErrors()) > 0 {
		t.Fatalf("routes2: %v", rr.GetErrors())
	}
	ping(true)

	// b only knows epoch 1: a stream at epoch 2 must be rejected
	pb := a.n.peer("node_b")
	st, err := pb.openStream(context.Background(), &meshv1.StreamOpen{Kind: &meshv1.StreamOpen_Service{
		Service: &meshv1.ServiceStream{ServiceId: testSvcID, Epoch: 2, Port: testTCP, Protocol: meshv1.Protocol_PROTOCOL_TCP},
	}})
	if err == nil {
		_ = st.Close()
		t.Fatal("b accepted epoch-2 stream")
	}
	if !strings.Contains(err.Error(), "STALE_EPOCH") {
		t.Fatalf("expected STALE_EPOCH, got %v", err)
	}

	// c only knows epoch 2: a stale epoch-1 stream must be rejected
	pc := a.n.peer("node_c")
	st, err = pc.openStream(context.Background(), &meshv1.StreamOpen{Kind: &meshv1.StreamOpen_Service{
		Service: &meshv1.ServiceStream{ServiceId: testSvcID, Epoch: 1, Port: testTCP, Protocol: meshv1.Protocol_PROTOCOL_TCP},
	}})
	if err == nil {
		_ = st.Close()
		t.Fatal("c accepted stale epoch-1 stream")
	}
	if !strings.Contains(err.Error(), "STALE_EPOCH") {
		t.Fatalf("expected STALE_EPOCH, got %v", err)
	}
}

func TestInternalForward(t *testing.T) {
	a := newTestNode(t, "node_a")
	b := newTestNode(t, "node_b")
	a.configure(t, nil, false)
	b.configure(t, nil, false)
	a.setPeers(t, peerOf(b))
	b.setPeers(t, peerOf(a))
	waitFor(t, "direct", 10*time.Second, func() bool {
		return pathOf(t, a, "node_b") == meshv1.PathKind_PATH_KIND_DIRECT
	})

	echo := startTCPEcho(t)
	if _, err := b.cli.SetInternalServices(context.Background(), &meshv1.SetInternalServicesRequest{
		Services: []*meshv1.InternalService{{Name: "agent.chunks", TargetPort: uint32(echo)}},
	}); err != nil {
		t.Fatal(err)
	}
	fr, err := a.cli.BindInternalForward(context.Background(), &meshv1.BindInternalForwardRequest{
		PeerNodeId: "node_b", Name: "agent.chunks",
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.DialTimeout("tcp", fr.GetLocalAddr(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("chunk-request")
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != string(msg) {
		t.Fatal("bad echo")
	}
	_ = c.Close()
	if _, err := a.cli.UnbindInternalForward(context.Background(), &meshv1.UnbindInternalForwardRequest{
		ForwardId: fr.GetForwardId(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRelayToDirectUpgrade(t *testing.T) {
	rel := newTestRelay(t, "r1")
	a := newTestNode(t, "node_a")
	b := newTestNode(t, "node_b")

	// direct blocked on both sides → relayed path
	// (set after Configure so sock exists)
	a.configure(t, []*meshv1.Relay{rel.relayFor(a, "grp_1")}, false)
	b.configure(t, []*meshv1.Relay{rel.relayFor(b, "grp_1")}, false)
	a.n.sock.blockDirect.Store(true)
	b.n.sock.blockDirect.Store(true)

	a.setPeers(t, peerOf(b, "r1"))
	b.setPeers(t, peerOf(a, "r1"))
	waitFor(t, "relayed path", 15*time.Second, func() bool {
		return pathOf(t, a, "node_b") == meshv1.PathKind_PATH_KIND_RELAYED
	})

	// unblock → upgrade within the retry window
	a.n.sock.blockDirect.Store(false)
	b.n.sock.blockDirect.Store(false)
	waitFor(t, "direct upgrade", 5*time.Second, func() bool {
		return pathOf(t, a, "node_b") == meshv1.PathKind_PATH_KIND_DIRECT
	})
}

func TestWatchEventsEmitsPathChanges(t *testing.T) {
	a := newTestNode(t, "node_a")
	b := newTestNode(t, "node_b")
	a.configure(t, nil, false)
	b.configure(t, nil, false)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := a.cli.WatchEvents(ctx, &meshv1.WatchEventsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	a.setPeers(t, peerOf(b))
	b.setPeers(t, peerOf(a))
	for {
		ev, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if pc := ev.GetPeerPathChanged(); pc != nil &&
			pc.GetNodeId() == "node_b" && pc.GetPath() == meshv1.PathKind_PATH_KIND_DIRECT {
			return
		}
	}
}

// Regression: retryDirectLoop evaluated p.current().Context() each select
// iteration, so a nil conn (peer not yet connected, or relayed conn torn
// down) panicked instead of returning.
func TestRetryDirectLoopWithoutConn(t *testing.T) {
	n := NewNode(testLog(), testTimings, nil)
	t.Cleanup(func() { n.Close() })
	p := &peerState{n: n, id: "node_b", stopCh: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.retryDirectLoop()
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retryDirectLoop did not return within 1s")
	}
}

var _ = atomic.Bool{}
