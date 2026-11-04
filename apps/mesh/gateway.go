package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/grendalaget/varde/go/gatewaytunnel"
	meshv1 "github.com/grendalaget/varde/go/gen/mesh/v1"
)

type hostedGateway struct {
	url    string
	port   uint32
	client *gatewaytunnel.Client
	cancel context.CancelFunc
	done   chan struct{}
}

// SetGatewayTunnels atomically validates a full replacement. A token-only update
// reconnects the existing client; removed services close all active streams.
func (n *Node) SetGatewayTunnels(req *meshv1.SetGatewayTunnelsRequest) error {
	want := map[string]*meshv1.GatewayTunnel{}
	clients := map[string]*gatewaytunnel.Client{}
	for _, cfg := range req.GetTunnels() {
		id := cfg.GetServiceId()
		if id == "" || cfg.GetTargetPort() == 0 || cfg.GetTargetPort() > 65535 {
			return errors.New("gateway tunnel requires service_id and a valid target_port")
		}
		if _, exists := want[id]; exists {
			return errors.New("duplicate gateway service_id")
		}
		client, err := gatewaytunnel.New(gatewaytunnel.Config{GatewayURL: cfg.GetGatewayUrl(), Token: cfg.GetToken(), TargetAddr: "127.0.0.1:" + strconv.Itoa(int(cfg.GetTargetPort()))})
		if err != nil {
			return fmt.Errorf("gateway tunnel %q: %w", id, err)
		}
		want[id] = cfg
		clients[id] = client
	}
	n.gatewayMu.Lock()
	defer n.gatewayMu.Unlock()
	if n.gateClosed {
		return errors.New("mesh is closed")
	}
	for id, old := range n.gateways {
		cfg := want[id]
		if cfg == nil || cfg.GetGatewayUrl() != old.url || cfg.GetTargetPort() != old.port {
			old.cancel()
			<-old.done
			delete(n.gateways, id)
		}
	}
	for id, cfg := range want {
		if old := n.gateways[id]; old != nil {
			old.client.UpdateToken(cfg.GetToken())
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		hosted := &hostedGateway{url: cfg.GetGatewayUrl(), port: cfg.GetTargetPort(), client: clients[id], cancel: cancel, done: make(chan struct{})}
		n.gateways[id] = hosted
		go func() { defer close(hosted.done); hosted.client.Run(ctx) }()
	}
	return nil
}

func (n *Node) closeGateways() {
	n.gatewayMu.Lock()
	defer n.gatewayMu.Unlock()
	n.gateClosed = true
	for id, hosted := range n.gateways {
		hosted.cancel()
		<-hosted.done
		delete(n.gateways, id)
	}
}
