// Command varde-control-plane serves the control-plane HTTP API,
// embeds the web UI, and runs the reconciler.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"net"
	"net/http"
	"net/url"

	"github.com/grendalaget/varde/go/identity"
	"github.com/grendalaget/varde/go/relay"

	"github.com/grendalaget/varde/apps/controlplane/internal/api"
	"github.com/grendalaget/varde/apps/controlplane/internal/auth"
	"github.com/grendalaget/varde/apps/controlplane/internal/reconciler"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
	"github.com/grendalaget/varde/apps/controlplane/internal/webui"
)

// Set by -ldflags "-X main.version=...".
var version = "dev"

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// defaultDBURL keeps the sqlite db in ProgramData on Windows, where a
// service-context install expects it, and /var/lib elsewhere.
func defaultDBURL() string {
	if runtime.GOOS == "windows" {
		if pd := os.Getenv("ProgramData"); pd != "" {
			return "sqlite:///" + filepath.ToSlash(filepath.Join(pd, "VardeCP", "cp.db"))
		}
	}
	return "sqlite:///var/lib/varde-cp/cp.db"
}

// sqlitePath extracts the file path from a sqlite:/// db url, or "" for other
// schemes. Query params are stripped.
func sqlitePath(url string) string {
	if url != "" && !strings.HasPrefix(url, "sqlite://") {
		return ""
	}
	p := strings.TrimPrefix(url, "sqlite://")
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	// The Windows drive form sqlite:///C:/dir/db leaves /C:/dir/db after the
	// authority slash — drop it so os.MkdirAll sees a real drive path.
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' &&
		(p[1] >= 'A' && p[1] <= 'Z' || p[1] >= 'a' && p[1] <= 'z') {
		p = p[1:]
	}
	return p
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

type serveCfg struct {
	listen         string
	dbURL          string
	publicURL      string
	signup         string
	embeddedRelay  string
	embeddedAddr   string
	relays         relayFlags
	heartbeatMs    int64
	leaseTTLMs     int64
	startGraceMs   int64
	suspectAfterMs int64
	offlineAfterMs int64
	logLevel       string
}

// serveFlags returns a FlagSet bound to a fresh serveCfg; both interactive
// serving and `service run` parse the same flags (env vars still apply as
// defaults).
func serveFlags() (*flag.FlagSet, *serveCfg) {
	cfg := &serveCfg{}
	fs := flag.NewFlagSet("varde-control-plane", flag.ExitOnError)
	fs.StringVar(&cfg.listen, "listen", envOr("VARDE_LISTEN", ":8080"), "HTTP listen address")
	fs.StringVar(&cfg.dbURL, "db", envOr("VARDE_DB", defaultDBURL()), "database URL (sqlite:///path | postgres://…)")
	fs.StringVar(&cfg.publicURL, "public-url", envOr("VARDE_PUBLIC_URL", "http://localhost:8080"), "external base URL for device-link verification")
	fs.StringVar(&cfg.signup, "signup", envOr("VARDE_SIGNUP", ""), "signup policy: open|invite|closed (empty = open until first user, then invite)")
	fs.StringVar(&cfg.embeddedRelay, "embedded-relay", envOr("VARDE_EMBEDDED_RELAY", ""), "run an in-process relay on this UDP addr (e.g. :3478)")
	fs.StringVar(&cfg.embeddedAddr, "embedded-relay-addr", envOr("VARDE_EMBEDDED_RELAY_ADDR", ""), "public addr of the embedded relay (default: --public-url host + embedded-relay port)")
	fs.Int64Var(&cfg.heartbeatMs, "heartbeat-interval-ms", envInt("VARDE_HEARTBEAT_INTERVAL_MS", 5000), "agent heartbeat interval")
	fs.Int64Var(&cfg.leaseTTLMs, "lease-ttl-ms", envInt("VARDE_LEASE_TTL_MS", 20000), "execution lease TTL")
	fs.Int64Var(&cfg.startGraceMs, "start-grace-ms", envInt("VARDE_START_GRACE_MS", 30000), "extra lease while preparing/restoring")
	fs.Int64Var(&cfg.suspectAfterMs, "suspect-after-ms", envInt("VARDE_SUSPECT_AFTER_MS", 15000), "node suspect threshold")
	fs.Int64Var(&cfg.offlineAfterMs, "offline-after-ms", envInt("VARDE_OFFLINE_AFTER_MS", 30000), "node offline threshold")
	fs.StringVar(&cfg.logLevel, "log-level", envOr("VARDE_LOG_LEVEL", "info"), "slog level")
	fs.Var(&cfg.relays, "relay", "declared relay id=…,addr=… (repeatable)")
	return fs, cfg
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "service" {
		os.Exit(serviceCmd(os.Args[2:]))
	}
	fs, cfg := serveFlags()
	_ = fs.Parse(os.Args[1:])

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, cfg, nil); err != nil {
		slog.Error("control-plane failed", "error", err)
		os.Exit(1)
	}
}

// serve runs the control plane until ctx is canceled or the server fails.
// logOut overrides the JSON log destination (the Windows service logs to a
// file); nil means stdout.
func serve(ctx context.Context, cfg *serveCfg, logOut io.Writer) error {
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.logLevel))
	if logOut == nil {
		logOut = os.Stdout
	}
	log := slog.New(slog.NewJSONHandler(logOut, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	if p := sqlitePath(cfg.dbURL); p != "" {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return fmt.Errorf("create db dir: %w", err)
		}
	}
	st, err := store.Open(cfg.dbURL, store.RealClock)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	// CP signing key for relay tokens, persisted in kv.
	relayKey, err := loadOrCreateRelayKey(context.Background(), st)
	if err != nil {
		return fmt.Errorf("relay key: %w", err)
	}

	timings := reconciler.DefaultTimings()
	timings.HeartbeatIntervalMs = cfg.heartbeatMs
	timings.LeaseTTLMs = cfg.leaseTTLMs
	timings.StartGraceMs = cfg.startGraceMs
	timings.SuspectAfterMs = cfg.suspectAfterMs
	timings.OfflineAfterMs = cfg.offlineAfterMs

	recon := reconciler.New(st, timings, log)

	if cfg.embeddedRelay != "" {
		rconn, err := net.ListenUDP("udp", mustResolveUDPAddr(cfg.embeddedRelay))
		if err != nil {
			return fmt.Errorf("embedded relay listen %s: %w", cfg.embeddedRelay, err)
		}
		rsrv, err := relay.New(relay.Config{
			ID: "embedded", PublicKey: relayKey.Public().(ed25519.PublicKey),
			Conn: rconn, Log: log,
		})
		if err != nil {
			return fmt.Errorf("embedded relay: %w", err)
		}
		go func() {
			if err := rsrv.Serve(ctx); err != nil {
				log.Error("embedded relay failed", "error", err)
			}
		}()
		addr := cfg.embeddedAddr
		if addr == "" {
			host := "localhost"
			if u, err := url.Parse(cfg.publicURL); err == nil && u.Hostname() != "" {
				host = u.Hostname()
			}
			addr = net.JoinHostPort(host, fmt.Sprint(rconn.LocalAddr().(*net.UDPAddr).Port))
		}
		cfg.relays = append(cfg.relays, api.RelayConf{ID: "embedded", Addr: addr})
		log.Info("embedded relay listening", "addr", addr)
	}

	srv := &api.Server{
		Store:    st,
		Auth:     &auth.Local{Store: st, Policy: auth.SignupPolicy(cfg.signup)},
		Cfg:      api.Config{PublicURL: cfg.publicURL, Timings: timings, Relays: cfg.relays, Version: version},
		Recon:    recon,
		RelayKey: relayKey,
		Log:      log,
	}

	handler := api.NewHandler(srv, webui.Handler())

	httpSrv := &http.Server{
		Addr:              cfg.listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go recon.Run(ctx)

	errCh := make(chan error, 1)
	go func() {
		log.Info("control-plane listening", "addr", cfg.listen, "db", cfg.dbURL, "version", version)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("server failed: %w", err)
		}
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
	}
	return nil
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
