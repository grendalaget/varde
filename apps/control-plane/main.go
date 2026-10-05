// Command p2pgames-control-plane serves the control-plane HTTP API,
// embeds the web UI, and runs the reconciler.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"net"
	"net/http"
	"net/url"

	"github.com/arnemolland/p2pgames/go/identity"
	"github.com/arnemolland/p2pgames/go/relay"

	"github.com/arnemolland/p2pgames/apps/control-plane/internal/api"
	"github.com/arnemolland/p2pgames/apps/control-plane/internal/auth"
	"github.com/arnemolland/p2pgames/apps/control-plane/internal/reconciler"
	"github.com/arnemolland/p2pgames/apps/control-plane/internal/store"
	"github.com/arnemolland/p2pgames/apps/control-plane/internal/webui"
)

// Set by -ldflags "-X main.version=...".
var version = "dev"

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// relayFlags collects repeatable --relay id=…,addr=… flags.
type relayFlags []api.RelayConf

func (r *relayFlags) String() string { return "" }
func (r *relayFlags) Set(v string) error {
	var rc api.RelayConf
	for _, kv := range strings.Split(v, ",") {
		k, val, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("bad --relay %q: want id=…,addr=…", v)
		}
		switch strings.TrimSpace(k) {
		case "id":
			rc.ID = val
		case "addr":
			rc.Addr = val
		}
	}
	if rc.ID == "" || rc.Addr == "" {
		return fmt.Errorf("bad --relay %q: need both id and addr", v)
	}
	*r = append(*r, rc)
	return nil
}

func main() {
	var (
		listen         = flag.String("listen", envOr("P2PGAMES_LISTEN", ":8080"), "HTTP listen address")
		dbURL          = flag.String("db", envOr("P2PGAMES_DB", "sqlite:///var/lib/p2pgames-cp/cp.db"), "database URL (sqlite:///path | postgres://…)")
		publicURL      = flag.String("public-url", envOr("P2PGAMES_PUBLIC_URL", "http://localhost:8080"), "external base URL for device-link verification")
		signup         = flag.String("signup", envOr("P2PGAMES_SIGNUP", ""), "signup policy: open|invite|closed (empty = open until first user, then invite)")
		embeddedRelay  = flag.String("embedded-relay", envOr("P2PGAMES_EMBEDDED_RELAY", ""), "run an in-process relay on this UDP addr (e.g. :3478)")
		embeddedAddr   = flag.String("embedded-relay-addr", envOr("P2PGAMES_EMBEDDED_RELAY_ADDR", ""), "public addr of the embedded relay (default: --public-url host + embedded-relay port)")
		relays         relayFlags
		heartbeatMs    = flag.Int64("heartbeat-interval-ms", envInt("P2PGAMES_HEARTBEAT_INTERVAL_MS", 5000), "agent heartbeat interval")
		leaseTTLMs     = flag.Int64("lease-ttl-ms", envInt("P2PGAMES_LEASE_TTL_MS", 20000), "execution lease TTL")
		startGraceMs   = flag.Int64("start-grace-ms", envInt("P2PGAMES_START_GRACE_MS", 600000), "extra lease while preparing/restoring")
		suspectAfterMs = flag.Int64("suspect-after-ms", envInt("P2PGAMES_SUSPECT_AFTER_MS", 15000), "node suspect threshold")
		offlineAfterMs = flag.Int64("offline-after-ms", envInt("P2PGAMES_OFFLINE_AFTER_MS", 30000), "node offline threshold")
		logLevel       = flag.String("log-level", envOr("P2PGAMES_LOG_LEVEL", "info"), "slog level")
	)
	flag.Var(&relays, "relay", "declared relay id=…,addr=… (repeatable)")
	flag.Parse()

	var level slog.Level
	_ = level.UnmarshalText([]byte(*logLevel))
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	st, err := store.Open(*dbURL, store.RealClock)
	if err != nil {
		log.Error("open store", "error", err)
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()

	// CP signing key for relay tokens, persisted in kv.
	relayKey, err := loadOrCreateRelayKey(context.Background(), st)
	if err != nil {
		log.Error("relay key", "error", err)
		os.Exit(1)
	}

	timings := reconciler.DefaultTimings()
	timings.HeartbeatIntervalMs = *heartbeatMs
	timings.LeaseTTLMs = *leaseTTLMs
	timings.StartGraceMs = *startGraceMs
	timings.SuspectAfterMs = *suspectAfterMs
	timings.OfflineAfterMs = *offlineAfterMs

	recon := reconciler.New(st, timings, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *embeddedRelay != "" {
		rconn, err := net.ListenUDP("udp", mustResolveUDPAddr(*embeddedRelay))
		if err != nil {
			log.Error("embedded relay listen", "addr", *embeddedRelay, "error", err)
			os.Exit(1)
		}
		rsrv, err := relay.New(relay.Config{
			ID: "embedded", PublicKey: relayKey.Public().(ed25519.PublicKey),
			Conn: rconn, Log: log,
		})
		if err != nil {
			log.Error("embedded relay", "error", err)
			os.Exit(1)
		}
		go func() {
			if err := rsrv.Serve(ctx); err != nil {
				log.Error("embedded relay failed", "error", err)
			}
		}()
		addr := *embeddedAddr
		if addr == "" {
			host := "localhost"
			if u, err := url.Parse(*publicURL); err == nil && u.Hostname() != "" {
				host = u.Hostname()
			}
			addr = net.JoinHostPort(host, fmt.Sprint(rconn.LocalAddr().(*net.UDPAddr).Port))
		}
		relays = append(relays, api.RelayConf{ID: "embedded", Addr: addr})
		log.Info("embedded relay listening", "addr", addr)
	}

	srv := &api.Server{
		Store:    st,
		Auth:     &auth.Local{Store: st, Policy: auth.SignupPolicy(*signup)},
		Cfg:      api.Config{PublicURL: *publicURL, Timings: timings, Relays: relays, Version: version},
		Recon:    recon,
		RelayKey: relayKey,
		Log:      log,
	}

	handler := api.NewHandler(srv, webui.Handler())

	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go recon.Run(ctx)

	errCh := make(chan error, 1)
	go func() {
		log.Info("control-plane listening", "addr", *listen, "db", *dbURL, "version", version)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			log.Error("server failed", "error", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			log.Error("shutdown failed", "error", err)
			os.Exit(1)
		}
	}
}

func envInt(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

func mustResolveUDPAddr(addr string) *net.UDPAddr {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		slog.Error("invalid addr", "addr", addr, "error", err)
		os.Exit(2)
	}
	return ua
}

const relayKeyKV = "relay_signing_key_pkcs8"

func loadOrCreateRelayKey(ctx context.Context, st *store.Store) (ed25519.PrivateKey, error) {
	if pemStr, err := st.KVGet(ctx, relayKeyKV); err == nil {
		return identity.ParsePrivateKeyPEM([]byte(pemStr))
	}
	_, priv, err := identity.Generate()
	if err != nil {
		return nil, err
	}
	pemBytes, err := identity.MarshalPrivateKeyPEM(priv)
	if err != nil {
		return nil, err
	}
	if err := st.KVSet(ctx, relayKeyKV, string(pemBytes)); err != nil {
		return nil, err
	}
	return priv, nil
}

// Unused references kept for clarity of wiring.
var _ = base64.StdEncoding
