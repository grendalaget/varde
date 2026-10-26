package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	meshv1 "github.com/grendalaget/varde/go/gen/mesh/v1"
)

// udpRoundTrip sends msg from uc until it is echoed back or attempts run out.
func udpRoundTrip(t *testing.T, uc *net.UDPConn, msg string) bool {
	t.Helper()
	for attempt := 0; attempt < 20; attempt++ {
		if _, err := uc.Write([]byte(msg)); err != nil {
			t.Fatalf("write: %v", err)
		}
		_ = uc.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		buf := make([]byte, 64*1024)
		nr, err := uc.Read(buf)
		if err == nil && string(buf[:nr]) == msg {
			return true
		}
	}
	return false
}

// routedPair connects a→b directly, hosts the test service on b with the
// given UDP target and routes it on a.
func routedPair(t *testing.T, udpTarget int) (*testNode, *testNode) {
	t.Helper()
	a := newTestNode(t, "node_a")
	b := newTestNode(t, "node_b")
	a.configure(t, nil, false)
	b.configure(t, nil, false)
	a.setPeers(t, peerOf(b))
	b.setPeers(t, peerOf(a))
	waitFor(t, "direct", 10*time.Second, func() bool {
		return pathOf(t, a, "node_b") == meshv1.PathKind_PATH_KIND_DIRECT
	})
	if _, err := b.cli.SetHostedServices(context.Background(), &meshv1.SetHostedServicesRequest{
		Services: []*meshv1.HostedService{hostedFor(testSvcID, 1, testTCP, 0, testUDP, uint32(udpTarget))},
	}); err != nil {
		t.Fatal(err)
	}
	if rr, err := a.cli.SetRoutes(context.Background(), &meshv1.SetRoutesRequest{
		Routes: []*meshv1.ServiceRoute{routeFor(testSvcID, testIP, testTCP, "node_b", 1, testUDP)},
	}); err != nil || len(rr.GetErrors()) > 0 {
		t.Fatalf("SetRoutes: %v %v", err, rr.GetErrors())
	}
	return a, b
}

// flowFor returns the client-side flow for uc's source address, or nil.
func flowFor(tn *testNode, uc *net.UDPConn) *udpFlow {
	tn.n.mu.RLock()
	rt := tn.n.routes[testSvcID]
	tn.n.mu.RUnlock()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.flows[udpFlowKey{testUDP, uc.LocalAddr().(*net.UDPAddr).AddrPort()}]
}

func droppedDatagrams(t *testing.T, tn *testNode) float64 {
	t.Helper()
	mfs, err := tn.n.metrics.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "mesh_datagrams_dropped_total" {
			return mf.GetMetric()[0].GetCounter().GetValue()
		}
	}
	return 0
}

// Regression: openFlow held rt.mu while taking n.mu.RLock (peer lookup)
// and SetRoutes holds n.mu while taking rt.mu — a new client's first packet
// racing a SetRoutes deadlocked the node. A route removed mid-open also
// wrote into closeAll's nil flows map.
func TestOpenFlowRacesSetRoutes(t *testing.T) {
	a, _ := routedPair(t, startUDPEcho(t))
	routes := [][]*meshv1.ServiceRoute{
		{routeFor(testSvcID, testIP, testTCP, "node_b", 1, testUDP)},
		{routeFor(testSvcID, testIP, testTCP, "node_b", 2, testUDP)},
		nil, // route removed: closeAll while flows may be opening
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dst := &net.UDPAddr{IP: net.ParseIP(testIP), Port: testUDP}
			for {
				select {
				case <-stop:
					return
				default:
				}
				uc, err := net.DialUDP("udp", nil, dst)
				if err != nil {
					continue
				}
				_, _ = uc.Write([]byte("hi"))
				_ = uc.Close()
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 300; i++ {
			a.n.SetRoutes(&meshv1.SetRoutesRequest{Routes: routes[i%len(routes)]})
		}
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("SetRoutes deadlocked against openFlow")
	}
	close(stop)
	wg.Wait()
}

// Regression: a flow closed by anything but the idle reaper (here: its
// stream dying with the conn, as on a relay→direct upgrade) stayed in
// rt.flows, so the client kept sending under a flow id the host had
// already forgotten and was black-holed.
func TestClosedFlowIsReopened(t *testing.T) {
	a, _ := routedPair(t, startUDPEcho(t))
	uc, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP(testIP), Port: testUDP})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = uc.Close() }()
	if !udpRoundTrip(t, uc, "before") {
		t.Fatal("no echo before conn loss")
	}
	f := flowFor(a, uc)
	if f == nil {
		t.Fatal("no flow")
	}
	_ = f.peer.current().CloseWithError(0, "test: path lost")
	waitFor(t, "closed flow removed", 5*time.Second, func() bool { return flowFor(a, uc) != f })
	waitFor(t, "reconnect", 10*time.Second, func() bool {
		return pathOf(t, a, "node_b") == meshv1.PathKind_PATH_KIND_DIRECT
	})
	if !udpRoundTrip(t, uc, "after") {
		t.Fatal("client black-holed after its flow closed")
	}
	if nf := flowFor(a, uc); nf == nil || nf == f || nf.closed.Load() {
		t.Fatalf("expected a fresh live flow, got %p (old %p)", nf, f)
	}
}

// Oversize datagrams must be dropped and counted in both directions with
// the flow left alive. errors.Is against a zero DatagramTooLargeError
// never matched, so the client flow closed and the host's reply pump died.
func TestOversizeDatagramDropsKeepFlow(t *testing.T) {
	big := make([]byte, 4000) // over any QUIC datagram payload on loopback
	target, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Close() })
	go func() {
		buf := make([]byte, 64*1024)
		for {
			nr, src, err := target.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if string(buf[:nr]) == "big" {
				_, _ = target.WriteToUDP(big, src)
				continue
			}
			_, _ = target.WriteToUDP(buf[:nr], src)
		}
	}()
	a, b := routedPair(t, target.LocalAddr().(*net.UDPAddr).Port)
	uc, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP(testIP), Port: testUDP})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = uc.Close() }()
	if !udpRoundTrip(t, uc, "hi") {
		t.Fatal("no echo")
	}
	f := flowFor(a, uc)

	// client → host oversize
	if _, err := uc.Write(big); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "client-side drop counted", 3*time.Second, func() bool { return droppedDatagrams(t, a) >= 1 })
	if flowFor(a, uc) != f || f.closed.Load() {
		t.Fatal("oversize datagram closed the client flow")
	}

	// host → client oversize reply
	if _, err := uc.Write([]byte("big")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "host-side drop counted", 3*time.Second, func() bool { return droppedDatagrams(t, b) >= 1 })
	if !udpRoundTrip(t, uc, "still-there") {
		t.Fatal("host reply pump died on an oversize reply")
	}
	if flowFor(a, uc) != f {
		t.Fatal("flow replaced")
	}
}

func TestIsDatagramTooLarge(t *testing.T) {
	e := &quic.DatagramTooLargeError{MaxDatagramPayloadSize: 1200}
	if !isDatagramTooLarge(e) || !isDatagramTooLarge(fmt.Errorf("send: %w", e)) {
		t.Fatal("DatagramTooLargeError not matched")
	}
	if got := datagramTooLarge(fmt.Errorf("send: %w", e)); got != e {
		t.Fatalf("oversize metadata = %v, want %v", got, e)
	}
	if isDatagramTooLarge(io.EOF) {
		t.Fatal("unrelated error matched")
	}
}

// Regression: the relay entry compared the configured addr (a hostname)
// with the resolved IP, so every Configure — one per heartbeat, each with
// a fresh token — tore down the relay client and its relayed conns.
func TestRelayHostnameSurvivesReconfigure(t *testing.T) {
	rel := newTestRelay(t, "r1")
	_, port, err := net.SplitHostPort(rel.addr)
	if err != nil {
		t.Fatal(err)
	}
	a := newTestNode(t, "node_a")
	r := rel.relayFor(a, "grp_1")
	r.Addr = net.JoinHostPort("localhost", port)
	a.configure(t, []*meshv1.Relay{r}, false)
	a.n.mu.RLock()
	rc := a.n.relays["r1"]
	a.n.mu.RUnlock()
	if rc == nil {
		t.Fatal("relay client not created")
	}
	waitFor(t, "registered", 5*time.Second, rc.registered)

	time.Sleep(2 * time.Millisecond) // distinct token expiry → distinct token
	r2 := rel.relayFor(a, "grp_1")
	r2.Addr = r.Addr
	a.configure(t, []*meshv1.Relay{r2}, false)
	a.n.mu.RLock()
	got := a.n.relays["r1"]
	a.n.mu.RUnlock()
	if got != rc || rc.stopped.Load() {
		t.Fatal("relay client recreated on reconfigure with the same hostname addr")
	}
	rc.mu.RLock()
	tok := rc.token
	rc.mu.RUnlock()
	if tok != r2.GetToken() {
		t.Fatal("token not updated")
	}
	if !a.n.sock.isRelay(rc.addr) {
		t.Fatal("relay addr not marked on the shared socket")
	}
}

func TestInternalForwardCannotExposePeerChunks(t *testing.T) {
	a := newTestNode(t, "node_a")
	a.n.mu.Lock()
	a.n.peers["node_b"] = newPeerState(a.n, &meshv1.Peer{NodeId: "node_b"})
	a.n.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	forward, err := a.cli.BindInternalForward(ctx, &meshv1.BindInternalForwardRequest{
		PeerNodeId: "node_b", Name: "agent.chunks",
	})
	if status.Code(err) != codes.Unimplemented || forward != nil {
		t.Fatalf("legacy bind exposed an internal forward: response=%v error=%v", forward, err)
	}
}

func TestRouteSessionCounts(t *testing.T) {
	rt := &routeState{n: &Node{}, flows: map[udpFlowKey]*udpFlow{}}
	id := rt.openSession("node_b")
	now := time.Now()
	if c := rt.sessionCounts(now); len(c) != 0 {
		t.Fatalf("a fresh connection is not a play session yet: %v", c)
	}
	playing := &udpFlow{hostNodeID: "node_a", opened: now.Add(-time.Minute)}
	playing.lastUsed.Store(now.Add(sessionMinAge).UnixNano())
	idle := &udpFlow{hostNodeID: "node_a", opened: now.Add(-time.Minute)}
	idle.lastUsed.Store(now.Add(-time.Minute).UnixNano())
	rt.flows[udpFlowKey{port: 1}] = playing
	rt.flows[udpFlowKey{port: 2}] = idle
	later := now.Add(sessionMinAge)
	c := rt.sessionCounts(later.Add(-time.Second))
	if c["node_b"] != 0 {
		t.Fatalf("tcp session counted before %s: %v", sessionMinAge, c)
	}
	c = rt.sessionCounts(later)
	if c["node_b"] != 1 || c["node_a"] != 1 || len(c) != 2 {
		t.Fatalf("by host: %v", c)
	}
	rt.closeSession(id)
	if c := rt.sessionCounts(later); c["node_b"] != 0 {
		t.Fatalf("closed tcp session still counted: %v", c)
	}
}
