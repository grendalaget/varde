package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/grendalaget/varde/apps/controlplane/internal/api"
	"github.com/grendalaget/varde/go/gateway"
	"github.com/grendalaget/varde/go/gatewaytoken"
)

type gatewayOptions struct {
	external                string
	embedded                bool
	domain, addr, cert, key string
}

func (g *gatewayOptions) flags(fs *flag.FlagSet) {
	fs.StringVar(&g.external, "gateway", envOr("VARDE_GATEWAY", ""), "public gateway url=wss://host/v1/tunnel,domain=example.site")
	fs.BoolVar(&g.embedded, "embedded-gateway", envOr("VARDE_EMBEDDED_GATEWAY", "false") == "true", "run a TLS public gateway in this process")
	fs.StringVar(&g.domain, "gateway-domain", envOr("VARDE_GATEWAY_DOMAIN", ""), "wildcard hostname domain for embedded gateway")
	fs.StringVar(&g.addr, "gateway-addr", envOr("VARDE_GATEWAY_ADDR", ":443"), "embedded gateway HTTPS listen address")
	fs.StringVar(&g.cert, "gateway-tls-cert", envOr("VARDE_GATEWAY_TLS_CERT", ""), "embedded gateway wildcard TLS certificate")
	fs.StringVar(&g.key, "gateway-tls-key", envOr("VARDE_GATEWAY_TLS_KEY", ""), "embedded gateway TLS private key")
}

func parseGateway(v string) (*api.GatewayConf, error) {
	if v == "" {
		return nil, nil
	}
	c := &api.GatewayConf{}
	seen := map[string]bool{}
	for _, kv := range strings.Split(v, ",") {
		k, value, ok := strings.Cut(kv, "=")
		if !ok || seen[k] {
			return nil, fmt.Errorf("bad --gateway: want url=wss://host/v1/tunnel,domain=example.site")
		}
		seen[k] = true
		switch k {
		case "url":
			c.URL = value
		case "domain":
			c.Domain = value
		default:
			return nil, fmt.Errorf("unknown --gateway field %q", k)
		}
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Scheme != "wss" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/v1/tunnel" || !gatewaytoken.ValidDomain(c.Domain) {
		return nil, errors.New("gateway requires a wss://host/v1/tunnel URL and a valid hostname domain")
	}
	return c, nil
}

func (g gatewayOptions) start(ctx context.Context, key ed25519.PrivateKey, publicURL string, log *slog.Logger) (*api.GatewayConf, error) {
	c, err := parseGateway(g.external)
	if err != nil || !g.embedded {
		return c, err
	}
	if !gatewaytoken.ValidDomain(g.domain) {
		return nil, errors.New("embedded gateway requires --gateway-domain")
	}
	if c != nil && c.Domain != g.domain {
		return nil, errors.New("gateway domains must match")
	}
	certificate, err := tls.LoadX509KeyPair(g.cert, g.key)
	if err != nil {
		return nil, fmt.Errorf("embedded gateway certificate: %w", err)
	}
	handler, err := gateway.New(gateway.Config{Domain: g.domain, PublicKey: key.Public().(ed25519.PublicKey)})
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", g.addr)
	if err != nil {
		handler.Close()
		return nil, err
	}
	if c == nil {
		u, err := url.Parse(publicURL)
		if err != nil || u.Hostname() == "" {
			_ = ln.Close()
			handler.Close()
			return nil, errors.New("embedded gateway requires a valid --public-url")
		}
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		c = &api.GatewayConf{Domain: g.domain, URL: "wss://" + net.JoinHostPort(u.Hostname(), port) + "/v1/tunnel"}
	}
	ln = gateway.WithReadIdleTimeout(ln, 90*time.Second)
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 64 << 10, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}}
	go func() {
		if err := server.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("embedded gateway failed", "error", err)
		}
	}()
	go func() {
		<-ctx.Done()
		handler.Close()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		_ = server.Close()
	}()
	log.Info("embedded gateway listening", "addr", ln.Addr(), "domain", g.domain)
	return c, nil
}
