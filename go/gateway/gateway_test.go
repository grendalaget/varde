package gateway_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/grendalaget/varde/go/gateway"
	"github.com/grendalaget/varde/go/gatewaytoken"
	"github.com/grendalaget/varde/go/gatewaytunnel"
	"github.com/prometheus/client_golang/prometheus"
)

const domain = "servers.example.com"
const publicHost = "at." + domain

func upstream(t *testing.T, name string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" {
			ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			defer func() { _ = ws.CloseNow() }()
			for {
				kind, b, err := ws.Read(r.Context())
				if err != nil {
					return
				}
				if ws.Write(r.Context(), kind, b) != nil {
					return
				}
			}
		}
		if r.Method == http.MethodPost {
			_, _ = io.Copy(io.Discard, r.Body)
		}
		w.Header().Set("X-Saw-Proto", r.Header.Get("X-Forwarded-Proto"))
		w.Header().Set("X-Saw-Host", r.Header.Get("X-Forwarded-Host"))
		w.Header().Set("X-Saw-For", r.Header.Get("X-Forwarded-For"))
		_, _ = fmt.Fprint(w, name)
	}))
	t.Cleanup(s.Close)
	return s
}

func capability(t *testing.T, priv ed25519.PrivateKey, epoch uint64, server string, expiry time.Time) string {
	t.Helper()
	tok, err := gatewaytoken.Sign(priv, gatewaytoken.Claims{Audience: gatewaytoken.Audience, Domain: domain, Label: "at", GroupID: "group", ServerID: server, Epoch: epoch, ExpUnixMs: expiry.UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func startClient(t *testing.T, url, token, target string, httpClient *http.Client) {
	t.Helper()
	client, err := gatewaytunnel.New(gatewaytunnel.Config{GatewayURL: strings.Replace(url, "http", "ws", 1) + "/v1/tunnel", Token: token, TargetAddr: strings.TrimPrefix(target, "http://"), HTTPClient: httpClient, AllowInsecureHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); client.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("tunnel did not stop")
		}
	})
}

func request(t *testing.T, cli *http.Client, url, host, method string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	req.Header.Set("X-Forwarded-For", "203.0.113.99")
	req.Header.Set("X-Forwarded-Proto", "spoofed")
	req.Header.Set("X-Forwarded-Host", "spoofed")
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func awaitBody(t *testing.T, cli *http.Client, url, want string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		resp := request(t, cli, url, publicHost, http.MethodGet, nil)
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == 200 && string(b) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("gateway did not route to %q", want)
}

func TestHTTPWebsocketAndEpochFailover(t *testing.T) {
	for _, tls := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls=%v", tls), func(t *testing.T) {
			pub, priv, _ := ed25519.GenerateKey(rand.Reader)
			handler, err := gateway.New(gateway.Config{Domain: domain, PublicKey: pub, MaxBodyBytes: 16, MaxStreams: 4, Metrics: prometheus.NewRegistry()})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(handler.Close)
			var srv *httptest.Server
			if tls {
				srv = httptest.NewTLSServer(handler)
			} else {
				srv = httptest.NewServer(handler)
			}
			t.Cleanup(srv.Close)
			cli := srv.Client()
			cli.Timeout = 5 * time.Second
			a, b := upstream(t, "first"), upstream(t, "second")
			first := capability(t, priv, 1, "first", time.Now().Add(time.Minute))
			startClient(t, srv.URL, first, a.URL, cli)
			awaitBody(t, cli, srv.URL, "first")
			resp := request(t, cli, srv.URL, publicHost, http.MethodGet, nil)
			expectedProto := "http"
			if tls {
				expectedProto = "https"
			}
			if resp.Header.Get("X-Saw-Proto") != expectedProto || resp.Header.Get("X-Saw-Host") != publicHost || resp.Header.Get("X-Saw-For") != "127.0.0.1" {
				t.Error("forwarded headers incorrect or spoofable")
			}
			_ = resp.Body.Close()
			ws, _, err := websocket.Dial(context.Background(), strings.Replace(srv.URL, "http", "ws", 1)+"/ws", &websocket.DialOptions{Host: publicHost, HTTPClient: cli})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = ws.CloseNow() }()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := ws.Write(ctx, websocket.MessageText, []byte("echo")); err != nil {
				t.Fatal(err)
			}
			_, echo, err := ws.Read(ctx)
			if err != nil || string(echo) != "echo" {
				t.Fatal("websocket echo failed")
			}
			for _, host := range []string{"unknown." + domain, "nested.at." + domain, "at.evil.example.com"} {
				r := request(t, cli, srv.URL, host, http.MethodGet, nil)
				if r.StatusCode != 404 {
					t.Error("unknown hostname accepted")
				}
				_ = r.Body.Close()
			}
			for _, body := range []io.Reader{bytes.NewBufferString(strings.Repeat("x", 17)), struct{ io.Reader }{strings.NewReader(strings.Repeat("x", 32))}} {
				r := request(t, cli, srv.URL, publicHost, http.MethodPost, body)
				if r.StatusCode != 413 {
					t.Errorf("body cap: %d", r.StatusCode)
				}
				_ = r.Body.Close()
			}
			second := capability(t, priv, 2, "second", time.Now().Add(time.Minute))
			startClient(t, srv.URL, second, b.URL, cli)
			awaitBody(t, cli, srv.URL, "second")
			if _, _, err := ws.Read(ctx); err == nil {
				t.Error("old upgraded stream survived failover")
			}
			for _, tok := range []string{first, capability(t, priv, 2, "other-server", time.Now().Add(time.Minute))} {
				rejected, response, err := websocket.Dial(ctx, strings.Replace(srv.URL, "http", "ws", 1)+"/v1/tunnel", &websocket.DialOptions{HTTPClient: cli, HTTPHeader: http.Header{"Authorization": []string{"Bearer " + tok}}})
				if rejected != nil {
					_ = rejected.CloseNow()
				}
				if err == nil || response == nil || response.StatusCode != 409 {
					t.Error("stale/equal foreign epoch not rejected")
				}
			}
		})
	}
}

func TestAuthExpiryAndStreamLimit(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	handler, _ := gateway.New(gateway.Config{Domain: domain, PublicKey: pub, MaxStreams: 1})
	defer handler.Close()
	srv := httptest.NewServer(handler)
	defer srv.Close()
	cli := srv.Client()
	cli.Timeout = 5 * time.Second
	resp := request(t, cli, srv.URL+"/v1/tunnel", publicHost, http.MethodGet, nil)
	if resp.StatusCode != 401 {
		t.Fatal("missing token accepted")
	}
	_ = resp.Body.Close()
	target := upstream(t, "alive")
	tok := capability(t, priv, 7, "s", time.Now().Add(2*time.Second))
	startClient(t, srv.URL, tok, target.URL, cli)
	awaitBody(t, cli, srv.URL, "alive")
	cli.CloseIdleConnections()
	ws, _, err := websocket.Dial(context.Background(), strings.Replace(srv.URL, "http", "ws", 1)+"/ws", &websocket.DialOptions{Host: publicHost, HTTPClient: cli})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	resp = request(t, cli, srv.URL, publicHost, http.MethodGet, nil)
	if resp.StatusCode != 503 {
		t.Errorf("stream cap: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if _, _, err := ws.Read(ctx); err == nil {
		t.Fatal("expired upgraded connection survived")
	}
	// The disconnected high-water mark must still fence older, otherwise-valid credentials.
	old := capability(t, priv, 6, "s", time.Now().Add(time.Minute))
	_, response, err := websocket.Dial(ctx, strings.Replace(srv.URL, "http", "ws", 1)+"/v1/tunnel", &websocket.DialOptions{HTTPClient: cli, HTTPHeader: http.Header{"Authorization": []string{"Bearer " + old}}})
	if err == nil || response == nil || response.StatusCode != 409 {
		t.Fatal("disconnected epoch fence lost")
	}
}
