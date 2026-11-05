package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	meshv1 "github.com/grendalaget/varde/go/gen/mesh/v1"
)

func TestGatewayTunnelReplacementAndIPC(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	node := NewNode(log, DefaultTimings(), nil)
	defer node.Close()
	server := newMeshServer(log, node)
	cfg := &meshv1.GatewayTunnel{ServiceId: "http", GatewayUrl: "wss://127.0.0.1:1/v1/tunnel", Token: "opaque", TargetPort: 3069}
	set := func(cfgs ...*meshv1.GatewayTunnel) error {
		_, err := server.SetGatewayTunnels(context.Background(), &meshv1.SetGatewayTunnelsRequest{Tunnels: cfgs})
		return err
	}
	if err := set(cfg); err != nil {
		t.Fatal(err)
	}
	original := node.gateways["http"]
	cfg.Token = "refreshed"
	if err := set(cfg); err != nil || node.gateways["http"] != original {
		t.Fatal("token update replaced client")
	}
	if err := set(&meshv1.GatewayTunnel{ServiceId: "bad"}); err == nil || node.gateways["http"] != original {
		t.Fatal("invalid replacement was not atomic")
	}
	if err := set(cfg, cfg); err == nil {
		t.Fatal("duplicate service accepted")
	}
	if err := set(&meshv1.GatewayTunnel{ServiceId: "bad", GatewayUrl: "ws://127.0.0.1:1/v1/tunnel", Token: "token", TargetPort: 1}); err == nil {
		t.Fatal("insecure IPC tunnel accepted")
	}
	cfg.TargetPort = 3070
	if err := set(cfg); err != nil || node.gateways["http"] == original {
		t.Fatal("port change did not replace client")
	}
	select {
	case <-original.done:
	default:
		t.Fatal("old client still running")
	}
	current := node.gateways["http"]
	if err := set(); err != nil || len(node.gateways) != 0 {
		t.Fatal("empty replacement did not clear tunnels")
	}
	select {
	case <-current.done:
	default:
		t.Fatal("removed client still running")
	}
	if err := set(cfg); err != nil {
		t.Fatal(err)
	}
	current = node.gateways["http"]
	node.Close()
	select {
	case <-current.done:
	default:
		t.Fatal("node shutdown left client running")
	}
	if err := set(cfg); err == nil {
		t.Fatal("closed node accepted tunnel")
	}
}
