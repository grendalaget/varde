package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	meshv1 "github.com/grendalaget/varde/go/gen/mesh/v1"
)

func TestUDPOversizeDropsPreserveFlow(t *testing.T) {
	a, b, client := datagramEcho(t)
	echo := func(payload []byte) {
		t.Helper()
		if !echoDatagram(client, payload) {
			t.Fatalf("UDP echo(%d bytes) failed", len(payload))
		}
	}
	echo([]byte("before"))
	a.n.mu.RLock()
	route := a.n.routes[testSvcID]
	a.n.mu.RUnlock()
	flow := func() *udpFlow {
		route.mu.Lock()
		defer route.mu.Unlock()
		for _, f := range route.flows {
			return f
		}
		return nil
	}
	original := flow()
	if original == nil {
		t.Fatal("echo created no UDP flow")
	}
	if _, err := client.Write(make([]byte, 2000)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "client oversize counter = 1", time.Second, func() bool { return datagramDrops(t, a) == 1 })
	echo([]byte("after-client-drop"))
	if _, err := client.Write([]byte("oversize-reply")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "host oversize counter = 1", time.Second, func() bool { return datagramDrops(t, b) == 1 })
	echo([]byte("after-host-drop"))
	echo(nil)
	if flow() != original || original.closed.Load() {
		t.Fatal("oversize drop replaced or closed the UDP flow")
	}
}

func TestDirectDatagramPathMTUDiscovery(t *testing.T) {
	_, _, client := datagramEcho(t)
	payload := bytes.Repeat([]byte("x"), 1300)
	waitFor(t, "direct PMTU permits a 1300-byte echo", 10*time.Second, func() bool {
		return echoDatagram(client, payload)
	})
	config := quicConfig()
	if config.DisablePathMTUDiscovery || config.InitialPacketSize != 0 {
		t.Fatalf("quicConfig() overrides safe discovery defaults: %+v", config)
	}
}

func datagramEcho(t *testing.T) (*testNode, *testNode, *net.UDPConn) {
	t.Helper()
	a, b := newTestNode(t, "node_a"), newTestNode(t, "node_b")
	a.configure(t, nil, false)
	b.configure(t, nil, false)
	a.setPeers(t, peerOf(b))
	b.setPeers(t, peerOf(a))
	waitFor(t, "direct UDP peers", 10*time.Second, func() bool {
		return pathOf(t, a, b.id) == meshv1.PathKind_PATH_KIND_DIRECT
	})
	target, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Close() })
	go func() {
		buf := make([]byte, 4096)
		for {
			n, addr, err := target.ReadFromUDP(buf)
			if err != nil {
				return
			}
			reply := buf[:n]
			if string(reply) == "oversize-reply" {
				reply = make([]byte, 2000)
			}
			_, _ = target.WriteToUDP(reply, addr)
		}
	}()
	b.n.SetHostedServices(&meshv1.SetHostedServicesRequest{Services: []*meshv1.HostedService{
		hostedFor(testSvcID, 1, 0, 0, testUDP, uint32(target.LocalAddr().(*net.UDPAddr).Port)),
	}})
	response, err := a.cli.SetRoutes(context.Background(), &meshv1.SetRoutesRequest{Routes: []*meshv1.ServiceRoute{
		routeFor(testSvcID, testIP, 0, b.id, 1, testUDP),
	}})
	if err != nil || len(response.GetErrors()) != 0 {
		t.Fatalf("SetRoutes() = %v, %v", response, err)
	}
	dst, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", testIP, testUDP))
	if err != nil {
		t.Fatal(err)
	}
	client, err := net.DialUDP("udp", nil, dst)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return a, b, client
}

func echoDatagram(client *net.UDPConn, payload []byte) bool {
	_ = client.SetDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := client.Write(payload); err != nil {
		return false
	}
	buf := make([]byte, 4096)
	n, err := client.Read(buf)
	return err == nil && bytes.Equal(buf[:n], payload)
}

func datagramDrops(t *testing.T, node *testNode) float64 {
	t.Helper()
	metrics, err := node.n.metrics.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, metric := range metrics {
		if metric.GetName() == "mesh_datagrams_dropped_total" {
			return metric.GetMetric()[0].GetCounter().GetValue()
		}
	}
	t.Fatal("mesh_datagrams_dropped_total missing")
	return 0
}
