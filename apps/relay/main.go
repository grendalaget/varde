// Command p2pgames-relay forwards opaque encrypted UDP payloads between
// mesh nodes that cannot establish a direct path. Thin wrapper over go/relay.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/arnemolland/p2pgames/go/relay"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	listen := flag.String("listen", envOr("P2PGAMES_RELAY_LISTEN", ":3478"), "UDP listen address")
	id := flag.String("id", envOr("P2PGAMES_RELAY_ID", "eu-1"), "relay id (must match CP-signed tokens)")
	cpKey := flag.String("control-plane-key", envOr("P2PGAMES_RELAY_CP_KEY", ""),
		"control-plane public key: base64, or URL of /v1/relay/public-key")
	metricsAddr := flag.String("metrics", envOr("P2PGAMES_RELAY_METRICS", ""), "Prometheus metrics listen address (e.g. :9090)")
	rate := flag.Float64("group-rate-mbps", 0, "per-group forward rate cap in Mbit/s (0 = unlimited)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	pub, err := resolveCPKey(*cpKey)
	if err != nil {
		log.Error("control-plane key", "error", err)
		os.Exit(2)
	}

	conn, err := net.ListenUDP("udp", mustResolveUDPAddr(*listen))
	if err != nil {
		log.Error("listen failed", "addr", *listen, "error", err)
		os.Exit(1)
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	srv, err := relay.New(relay.Config{
		ID: *id, PublicKey: pub, Conn: conn,
		GroupRateMbps: *rate, Metrics: reg, Log: log,
	})
	if err != nil {
		log.Error("relay config", "error", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *metricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
		go func() {
			log.Info("metrics listening", "addr", *metricsAddr)
			if err := http.ListenAndServe(*metricsAddr, mux); err != nil {
				log.Error("metrics server failed", "error", err)
			}
		}()
	}

	log.Info("relay listening", "addr", srv.Addr(), "id", *id)
	if err := srv.Serve(ctx); err != nil {
		log.Error("serve failed", "error", err)
		os.Exit(1)
	}
}

// resolveCPKey accepts a base64 ed25519 public key or an http(s) URL to a
// control plane's /v1/relay/public-key endpoint.
func resolveCPKey(v string) (ed25519.PublicKey, error) {
	if v == "" {
		return nil, fmt.Errorf("--control-plane-key is required")
	}
	if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
		cli := &http.Client{Timeout: 10 * time.Second}
		resp, err := cli.Get(v)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", v, err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("fetch %s: status %d", v, resp.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if err != nil {
			return nil, err
		}
		var out struct {
			PublicKey string `json:"public_key"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, fmt.Errorf("parse relay public key response: %w", err)
		}
		v = out.PublicKey
	}
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		// maybe the caller passed a base64url token fragment
		if b, err = base64.RawURLEncoding.DecodeString(v); err != nil {
			return nil, fmt.Errorf("decode control-plane key: %w", err)
		}
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("control-plane key is %d bytes, want %d", len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

func mustResolveUDPAddr(addr string) *net.UDPAddr {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		slog.Error("invalid addr", "addr", addr, "error", err)
		os.Exit(2)
	}
	return ua
}
