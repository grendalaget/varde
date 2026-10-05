// Package relay implements the p2pgames UDP relay: stateless apart from
// 60 s registrations, it forwards opaque QUIC ciphertext between registered
// nodes of the same control-plane group. See proto/mesh/v1/relay.proto.
package relay

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	meshv1 "github.com/arnemolland/p2pgames/go/gen/mesh/v1"
	"github.com/arnemolland/p2pgames/go/meshproto"
	"github.com/arnemolland/p2pgames/go/relaytoken"
)

// Defaults from networking.md.
const (
	DefaultRegisterTTLMs = 60_000
	MaxTimestampSkewMs   = 60_000
	sweepInterval        = 5 * time.Second
)

// Config for a relay server.
type Config struct {
	// ID is this relay's id; tokens must carry relay_id == ID.
	ID string
	// PublicKey is the control-plane ed25519 key that signs relay tokens.
	PublicKey ed25519.PublicKey
	// Conn is the UDP socket to serve on (caller binds; :0 is fine for tests).
	Conn *net.UDPConn
	// GroupRateMbps rate-limits forwarded DATA bytes per group (0 = unlimited).
	GroupRateMbps float64
	// RegisterTTLMs overrides the 60 s registration lifetime (tests).
	RegisterTTLMs int64
	// Now returns unix ms; nil = real clock.
	Now func() int64
	// Metrics registers Prometheus collectors into this registry (nil = off).
	Metrics *prometheus.Registry
	// OnForward, if set, is invoked with each forwarded DATA payload (the raw
	// QUIC ciphertext). Used by tests to prove the relay only ever sees
	// ciphertext.
	OnForward func(payload []byte)
	Log       *slog.Logger
}

type registration struct {
	nodeID  string
	groupID string
	addr    *net.UDPAddr
	expires int64
}

type bucket struct {
	tokens float64
	last   int64
}

// Server is a running relay.
type Server struct {
	cfg   Config
	conn  *net.UDPConn
	log   *slog.Logger
	nowMs func() int64

	mu      sync.Mutex
	regs    map[string]*registration // node_id → registration
	byAddr  map[string]*registration // "ip:port" → registration
	buckets map[string]*bucket       // group_id → token bucket

	pktsForwarded  prometheus.Counter
	bytesForwarded *prometheus.CounterVec
	errors         *prometheus.CounterVec
	rateLimited    *prometheus.CounterVec
	regsActive     prometheus.Gauge
}

// New validates cfg and returns a server (call Serve to run it).
func New(cfg Config) (*Server, error) {
	if cfg.Conn == nil {
		return nil, errors.New("relay: Config.Conn required")
	}
	if cfg.ID == "" {
		return nil, errors.New("relay: Config.ID required")
	}
	if len(cfg.PublicKey) != ed25519.PublicKeySize {
		return nil, errors.New("relay: Config.PublicKey required")
	}
	if cfg.RegisterTTLMs <= 0 {
		cfg.RegisterTTLMs = DefaultRegisterTTLMs
	}
	if cfg.Now == nil {
		cfg.Now = func() int64 { return time.Now().UnixMilli() }
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		cfg: cfg, conn: cfg.Conn, log: log, nowMs: cfg.Now,
		regs: map[string]*registration{}, byAddr: map[string]*registration{},
		buckets: map[string]*bucket{},
	}
	if cfg.Metrics != nil {
		s.pktsForwarded = prometheus.NewCounter(prometheus.CounterOpts{
			Name: "relay_packets_forwarded_total"})
		s.bytesForwarded = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relay_bytes_forwarded_total"}, []string{"group_id"})
		s.errors = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relay_errors_total"}, []string{"code"})
		s.rateLimited = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relay_rate_limited_total"}, []string{"group_id"})
		s.regsActive = prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "relay_registrations_active"})
		cfg.Metrics.MustRegister(s.pktsForwarded, s.bytesForwarded, s.errors, s.rateLimited, s.regsActive)
	}
	return s, nil
}

// Addr is the bound UDP address.
func (s *Server) Addr() string { return s.conn.LocalAddr().String() }

// Serve runs the read loop until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		t := time.NewTicker(sweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.sweep()
			}
		}
	}()
	go func() {
		<-ctx.Done()
		_ = s.conn.Close()
	}()

	buf := make([]byte, 64*1024)
	for {
		n, src, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		s.handle(src, buf[:n])
	}
}

func (s *Server) handle(src *net.UDPAddr, dgram []byte) {
	if len(dgram) == 0 {
		return
	}
	switch dgram[0] {
	case meshproto.DgramRegister:
		s.handleRegister(src, dgram)
	case meshproto.DgramData:
		s.handleData(src, dgram)
	case meshproto.DgramPing:
		if _, nonce, err := meshproto.DecodeNonce(dgram); err == nil {
			_, _ = s.conn.WriteToUDP(meshproto.EncodePong(nonce), src)
		}
	default:
		// QUIC packets or garbage on a relay socket: drop.
	}
}

func (s *Server) handleRegister(src *net.UDPAddr, dgram []byte) {
	var reg meshv1.RelayRegister
	if _, err := meshproto.DecodeProtoDgram(dgram, &reg); err != nil {
		s.sendError(src, meshv1.RelayError_CODE_UNAUTHENTICATED, "malformed register", "")
		return
	}
	now := s.nowMs()
	fail := func(code meshv1.RelayError_Code, msg string) {
		s.countError(code)
		s.sendError(src, code, msg, reg.GetNodeId())
	}
	if reg.GetRelayId() != s.cfg.ID {
		fail(meshv1.RelayError_CODE_UNAUTHENTICATED, "wrong relay_id")
		return
	}
	if d := reg.GetTimestampUnixMs() - now; d > MaxTimestampSkewMs || d < -MaxTimestampSkewMs {
		fail(meshv1.RelayError_CODE_UNAUTHENTICATED, "stale timestamp")
		return
	}
	claims, err := relaytoken.Verify(s.cfg.PublicKey, reg.GetToken(), s.cfg.ID, now)
	if err != nil {
		fail(meshv1.RelayError_CODE_UNAUTHENTICATED, "bad token")
		return
	}
	if claims.NodeID != reg.GetNodeId() {
		fail(meshv1.RelayError_CODE_UNAUTHENTICATED, "token/node mismatch")
		return
	}
	pkBytes, err := base64.StdEncoding.DecodeString(claims.PublicKey)
	if err != nil || len(pkBytes) != ed25519.PublicKeySize {
		fail(meshv1.RelayError_CODE_UNAUTHENTICATED, "bad public key in token")
		return
	}
	if !meshproto.VerifyRegister(ed25519.PublicKey(pkBytes), s.cfg.ID, claims.NodeID,
		reg.GetTimestampUnixMs(), reg.GetSignature()) {
		fail(meshv1.RelayError_CODE_UNAUTHENTICATED, "bad node signature")
		return
	}
	expires := now + s.cfg.RegisterTTLMs
	r := &registration{nodeID: claims.NodeID, groupID: claims.GroupID, addr: src, expires: expires}
	s.mu.Lock()
	if old := s.regs[r.nodeID]; old != nil {
		delete(s.byAddr, old.addr.String())
	}
	s.regs[r.nodeID] = r
	s.byAddr[src.String()] = r
	if s.regsActive != nil {
		s.regsActive.Set(float64(len(s.regs)))
	}
	s.mu.Unlock()

	resp, err := meshproto.EncodeProtoDgram(meshproto.DgramRegistered, &meshv1.RelayRegistered{
		ObservedAddr:  src.String(),
		ExpiresUnixMs: expires,
	})
	if err == nil {
		_, _ = s.conn.WriteToUDP(resp, src)
	}
	s.log.Debug("registered", "node", r.nodeID, "group", r.groupID, "addr", src)
}

func (s *Server) handleData(src *net.UDPAddr, dgram []byte) {
	dstID, payload, err := meshproto.DecodeData(dgram)
	if err != nil {
		return
	}
	s.mu.Lock()
	srcReg := s.byAddr[src.String()]
	dstReg := s.regs[dstID]
	s.mu.Unlock()
	now := s.nowMs()
	fail := func(code meshv1.RelayError_Code, msg string) {
		s.countError(code)
		var nid string
		if srcReg != nil {
			nid = srcReg.nodeID
		}
		s.sendError(src, code, msg, nid)
	}
	if srcReg == nil || srcReg.expires <= now {
		fail(meshv1.RelayError_CODE_UNAUTHENTICATED, "not registered")
		return
	}
	if dstReg == nil || dstReg.expires <= now {
		fail(meshv1.RelayError_CODE_UNKNOWN_DESTINATION, "unknown destination")
		return
	}
	if dstReg.groupID != srcReg.groupID {
		fail(meshv1.RelayError_CODE_FORBIDDEN, "different group")
		return
	}
	if !s.allow(dstReg.groupID, len(payload)) {
		if s.rateLimited != nil {
			s.rateLimited.WithLabelValues(dstReg.groupID).Inc()
		}
		fail(meshv1.RelayError_CODE_RATE_LIMITED, "rate limited")
		return
	}
	out := meshproto.EncodeDataBytes(srcReg.nodeID, payload)
	if s.cfg.OnForward != nil {
		s.cfg.OnForward(payload)
	}
	if _, err := s.conn.WriteToUDP(out, dstReg.addr); err == nil {
		if s.pktsForwarded != nil {
			s.pktsForwarded.Inc()
			s.bytesForwarded.WithLabelValues(dstReg.groupID).Add(float64(len(payload)))
		}
	}
}

// allow implements a per-group token bucket refilled at GroupRateMbps.
func (s *Server) allow(groupID string, n int) bool {
	rate := s.cfg.GroupRateMbps
	if rate <= 0 {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buckets[groupID]
	now := s.nowMs()
	if b == nil {
		// initial burst = one second of rate
		b = &bucket{tokens: rate * 1e6 / 8, last: now}
		s.buckets[groupID] = b
	}
	b.tokens += float64(now-b.last) / 1000 * rate * 1e6 / 8
	b.last = now
	if max := rate * 1e6 / 8; b.tokens > max {
		b.tokens = max
	}
	if b.tokens < float64(n) {
		return false
	}
	b.tokens -= float64(n)
	return true
}

func (s *Server) sweep() {
	now := s.nowMs()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, r := range s.regs {
		if r.expires <= now {
			delete(s.regs, id)
			delete(s.byAddr, r.addr.String())
		}
	}
	if s.regsActive != nil {
		s.regsActive.Set(float64(len(s.regs)))
	}
}

func (s *Server) sendError(dst *net.UDPAddr, code meshv1.RelayError_Code, msg, nodeID string) {
	d, err := meshproto.EncodeProtoDgram(meshproto.DgramError, &meshv1.RelayError{
		Code: code, Message: msg, NodeId: nodeID,
	})
	if err == nil {
		_, _ = s.conn.WriteToUDP(d, dst)
	}
}

func (s *Server) countError(code meshv1.RelayError_Code) {
	if s.errors != nil {
		s.errors.WithLabelValues(code.String()).Inc()
	}
}
