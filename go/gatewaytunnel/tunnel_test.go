package gatewaytunnel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestConfigValidation(t *testing.T) {
	good := Config{GatewayURL: "wss://gateway.example.com/v1/tunnel", Token: "token", TargetAddr: "127.0.0.1:3000"}
	if _, err := New(good); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Config{{}, {GatewayURL: "ws://gateway.example.com/v1/tunnel", Token: "token", TargetAddr: good.TargetAddr, AllowInsecureHTTP: true}, {GatewayURL: good.GatewayURL, Token: "token", TargetAddr: "192.0.2.1:80"}, {GatewayURL: good.GatewayURL, Token: "token", TargetAddr: "127.0.0.1:0"}, {GatewayURL: good.GatewayURL + "?token=secret", Token: "token", TargetAddr: good.TargetAddr}} {
		if _, err := New(bad); err == nil {
			t.Fatal("bad config accepted")
		}
	}
}

func TestReconnectAndTokenRefresh(t *testing.T) {
	var attempts atomic.Int32
	refreshed := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if r.Header.Get("Authorization") == "Bearer new" {
			select {
			case refreshed <- struct{}{}:
			default:
			}
		}
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()
	client, err := New(Config{GatewayURL: strings.Replace(srv.URL, "http", "ws", 1) + "/v1/tunnel", Token: "old", TargetAddr: "127.0.0.1:3000", AllowInsecureHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); client.Run(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for attempts.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if attempts.Load() < 2 {
		t.Fatal("did not reconnect")
	}
	client.UpdateToken("new")
	select {
	case <-refreshed:
	case <-time.After(time.Second):
		t.Fatal("refresh did not reconnect immediately")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not stop retries")
	}
}
