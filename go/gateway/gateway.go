// Package gateway serves hostname-routed HTTP over CP-authorized outbound tunnels.
package gateway

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/grendalaget/varde/go/gatewaytoken"
	"github.com/grendalaget/varde/go/gatewaytunnel"
	"github.com/hashicorp/yamux"
	"github.com/prometheus/client_golang/prometheus"
)

type Config struct {
	Domain       string
	PublicKey    ed25519.PublicKey
	MaxStreams   int
	MaxBodyBytes int64
	Metrics      prometheus.Registerer
}

type lease struct {
	claims gatewaytoken.Claims
	active *tunnel
}
type tunnel struct {
	session   *yamux.Session
	transport *http.Transport
	proxy     *httputil.ReverseProxy
	closeOnce sync.Once
}

func (t *tunnel) close() {
	t.closeOnce.Do(func() { _ = t.session.Close(); t.transport.CloseIdleConnections() })
}

type Server struct {
	cfg      Config
	mu       sync.Mutex
	leases   map[string]lease
	closed   bool
	tunnels  prometheus.Gauge
	requests prometheus.Counter
	rejected prometheus.Counter
	streams  prometheus.Gauge
}

func New(cfg Config) (*Server, error) {
	if !gatewaytoken.ValidDomain(cfg.Domain) || len(cfg.PublicKey) != ed25519.PublicKeySize {
		return nil, errors.New("gateway: valid domain and CP public key required")
	}
	if cfg.MaxStreams <= 0 {
		cfg.MaxStreams = 128
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 2 << 30
	}
	s := &Server{cfg: cfg, leases: make(map[string]lease), tunnels: prometheus.NewGauge(prometheus.GaugeOpts{Name: "varde_gateway_tunnels", Help: "Active gateway tunnels."}), requests: prometheus.NewCounter(prometheus.CounterOpts{Name: "varde_gateway_requests_total", Help: "Proxied public requests."}), rejected: prometheus.NewCounter(prometheus.CounterOpts{Name: "varde_gateway_tunnel_rejections_total", Help: "Rejected tunnel credentials or epochs."}), streams: prometheus.NewGauge(prometheus.GaugeOpts{Name: "varde_gateway_streams", Help: "Open upstream streams."})}
	if cfg.Metrics != nil {
		for _, c := range []prometheus.Collector{s.tunnels, s.requests, s.rejected, s.streams} {
			if err := cfg.Metrics.Register(c); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	var active []*tunnel
	for _, l := range s.leases {
		if l.active != nil {
			active = append(active, l.active)
		}
	}
	s.mu.Unlock()
	for _, t := range active {
		t.close()
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/tunnel" {
		s.accept(w, r)
		return
	}
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	label, ok := strings.CutSuffix(host, "."+s.cfg.Domain)
	if !ok || !gatewaytoken.ValidLabel(label) {
		http.Error(w, "No public server at this hostname.", http.StatusNotFound)
		return
	}
	s.mu.Lock()
	l, ok := s.leases[label]
	s.mu.Unlock()
	if !ok || l.active == nil || l.claims.ExpUnixMs <= time.Now().UnixMilli() {
		http.Error(w, "No public server at this hostname.", http.StatusNotFound)
		return
	}
	if r.ContentLength > s.cfg.MaxBodyBytes {
		http.Error(w, "Request body too large.", http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	s.requests.Inc()
	l.active.proxy.ServeHTTP(w, r)
}

func (s *Server) allowed(c gatewaytoken.Claims) bool {
	old, ok := s.leases[c.Label]
	return !s.closed && (!ok || c.Epoch > old.claims.Epoch || c.Epoch == old.claims.Epoch && c.GroupID == old.claims.GroupID && c.ServerID == old.claims.ServerID)
}

func (s *Server) accept(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	claims, err := gatewaytoken.Verify(s.cfg.PublicKey, token, s.cfg.Domain, time.Now().UnixMilli())
	if !ok || err != nil {
		s.rejected.Inc()
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	allowed := s.allowed(*claims)
	s.mu.Unlock()
	if !allowed {
		s.rejected.Inc()
		http.Error(w, "Stale tunnel epoch", http.StatusConflict)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer func() { _ = ws.CloseNow() }()
	session, err := yamux.Client(websocket.NetConn(r.Context(), ws, websocket.MessageBinary), gatewaytunnel.YamuxConfig())
	if err != nil {
		return
	}
	t := s.newTunnel(session)
	defer t.close()
	s.mu.Lock()
	if !s.allowed(*claims) {
		s.mu.Unlock()
		s.rejected.Inc()
		return
	}
	old := s.leases[claims.Label].active
	s.leases[claims.Label] = lease{claims: *claims, active: t}
	s.mu.Unlock()
	if old != nil {
		old.close()
	}
	s.tunnels.Inc()
	defer s.tunnels.Dec()
	defer func() {
		s.mu.Lock()
		l := s.leases[claims.Label]
		if l.active == t {
			l.active = nil
			s.leases[claims.Label] = l
		}
		s.mu.Unlock()
	}()
	timer := time.NewTimer(time.Until(time.UnixMilli(claims.ExpUnixMs)))
	defer timer.Stop()
	select {
	case <-r.Context().Done():
	case <-session.CloseChan():
	case <-timer.C:
	}
}

type countedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *countedConn) Close() error { err := c.Conn.Close(); c.once.Do(c.release); return err }

var errStreams = errors.New("gateway: stream limit reached")

func (s *Server) newTunnel(session *yamux.Session) *tunnel {
	slots := make(chan struct{}, s.cfg.MaxStreams)
	transport := &http.Transport{MaxIdleConnsPerHost: s.cfg.MaxStreams, IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: 30 * time.Second, MaxResponseHeaderBytes: 64 << 10, ExpectContinueTimeout: time.Second, DisableCompression: true}
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		select {
		case slots <- struct{}{}:
		default:
			transport.CloseIdleConnections()
			select {
			case slots <- struct{}{}:
			default:
				return nil, errStreams
			}
		}
		// OpenStream has a bounded StreamOpenTimeout. Closing the session also unblocks it.
		stream, err := session.OpenStream()
		if err != nil {
			<-slots
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			_ = stream.Close()
			<-slots
			return nil, err
		}
		s.streams.Inc()
		return &countedConn{Conn: stream, release: func() { <-slots; s.streams.Dec() }}, nil
	}
	proxy := &httputil.ReverseProxy{Transport: transport, Rewrite: func(pr *httputil.ProxyRequest) {
		pr.Out.URL.Scheme = "http"
		pr.Out.URL.Host = "tunnel"
		pr.Out.Host = pr.In.Host
		stripForwardingHeaders(pr.Out.Header)
		pr.SetXForwarded()
	}, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			http.Error(w, "Request body too large.", http.StatusRequestEntityTooLarge)
		} else if errors.Is(err, errStreams) {
			http.Error(w, "Public server busy.", http.StatusServiceUnavailable)
		} else {
			http.Error(w, "Public server unavailable.", http.StatusBadGateway)
		}
	}, ErrorLog: discardLog()}
	return &tunnel{session: session, transport: transport, proxy: proxy}
}

func stripForwardingHeaders(headers http.Header) {
	for key := range headers {
		name := strings.ToLower(strings.ReplaceAll(key, "_", "-"))
		forwarded := strings.Contains(name, "forwarded")
		clientIP := name == "client-ip" || strings.HasSuffix(name, "-client-ip") || strings.HasSuffix(name, "-real-ip")
		switch name {
		case "via", "cf-connecting-ip", "cf-connecting-ipv6", "cf-pseudo-ipv4", "true-client-ip", "x-proxyuser-ip", "x-envoy-external-address", "remote-addr", "x-remote-addr", "x-original-remote-addr", "x-azure-clientip", "x-appengine-user-ip", "front-end-https", "x-url-scheme", "x-original-host", "x-original-proto":
			delete(headers, key)
		default:
			if forwarded || clientIP {
				delete(headers, key)
			}
		}
	}
}

// ReverseProxy errors must never log request credentials or upstream details.
func discardLog() *log.Logger { return log.New(io.Discard, "", 0) }
