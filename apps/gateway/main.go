// Command varde-gateway terminates public HTTPS and proxies through outbound mesh tunnels.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
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

	"github.com/grendalaget/varde/go/gateway"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	listen := flag.String("listen", envOr("VARDE_GATEWAY_LISTEN", ":8443"), "Public HTTPS listen address")
	domain := flag.String("domain", envOr("VARDE_GATEWAY_DOMAIN", ""), "Wildcard public hostname domain")
	cert := flag.String("tls-cert", envOr("VARDE_GATEWAY_TLS_CERT", ""), "Wildcard certificate PEM path")
	key := flag.String("tls-key", envOr("VARDE_GATEWAY_TLS_KEY", ""), "Certificate private key PEM path")
	cpKey := flag.String("control-plane-key", envOr("VARDE_GATEWAY_CP_KEY", ""), "CP Ed25519 public key: base64 or public-key endpoint URL")
	dev := flag.Bool("dev-http", false, "Tests only: serve plain HTTP on loopback")
	metrics := flag.String("metrics", envOr("VARDE_GATEWAY_METRICS", ""), "Private Prometheus listen address")
	maxStreams := flag.Int("max-streams", 128, "Maximum concurrent upstream connections per tunnel")
	bodyCap := flag.Int64("max-body-bytes", 2<<30, "Maximum request body bytes per public label")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(*listen, *domain, *cert, *key, *cpKey, *metrics, *dev, *maxStreams, *bodyCap); err != nil {
		slog.Error("gateway stopped", "error", err)
		os.Exit(1)
	}
}

func run(listen, domain, cert, key, cpKey, metrics string, dev bool, maxStreams int, bodyCap int64) error {
	if dev {
		host, _, err := net.SplitHostPort(listen)
		if err != nil || !net.ParseIP(host).IsLoopback() {
			return errors.New("dev-http requires a loopback listen address")
		}
	} else if cert == "" || key == "" {
		return errors.New("TLS certificate and key required")
	}
	if maxStreams <= 0 || bodyCap <= 0 {
		return errors.New("limits must be positive")
	}
	pub, err := resolveCPKey(cpKey)
	if err != nil {
		return err
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	handler, err := gateway.New(gateway.Config{Domain: domain, PublicKey: pub, MaxStreams: maxStreams, MaxBodyBytes: bodyCap, Metrics: reg})
	if err != nil {
		return err
	}
	defer handler.Close()
	server := &http.Server{Addr: listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 64 << 10, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	listener = gateway.WithReadIdleTimeout(listener, 90*time.Second)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var metricsServer *http.Server
	failures := make(chan error, 2)
	if metrics != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
		metricsServer = &http.Server{Addr: metrics, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() { failures <- metricsServer.ListenAndServe() }()
		defer func() { _ = metricsServer.Close() }()
	}
	go func() {
		if dev {
			failures <- server.Serve(listener)
		} else {
			failures <- server.ServeTLS(listener, cert, key)
		}
	}()
	slog.Info("gateway listening", "addr", listen, "domain", domain)
	var result error
	select {
	case <-ctx.Done():
	case result = <-failures:
	}
	handler.Close()
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
	_ = server.Close()
	if errors.Is(result, http.ErrServerClosed) {
		return nil
	}
	return result
}

// Same input contract as varde-relay's --control-plane-key.
func resolveCPKey(v string) (ed25519.PublicKey, error) {
	if v == "" {
		return nil, errors.New("control-plane-key required")
	}
	if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(v)
		if err != nil {
			return nil, errors.New("fetch CP public key failed")
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("CP public key status %d", resp.StatusCode)
		}
		var out struct {
			PublicKey string `json:"public_key"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
			return nil, err
		}
		v = out.PublicKey
	}
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		b, err = base64.RawURLEncoding.DecodeString(v)
	}
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("invalid CP Ed25519 public key")
	}
	return ed25519.PublicKey(b), nil
}
