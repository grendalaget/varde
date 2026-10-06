package relay_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net"
	"testing"
	"time"

	meshv1 "github.com/grendalaget/varde/go/gen/mesh/v1"
	"github.com/grendalaget/varde/go/meshproto"
	"github.com/grendalaget/varde/go/relay"
	"github.com/grendalaget/varde/go/relaytoken"
)

type harness struct {
	t       *testing.T
	cpPub   ed25519.PublicKey
	cpPriv  ed25519.PrivateKey
	relayID string
	srv     *relay.Server
	conn    *net.UDPConn
	now     int64
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	cpPub, cpPriv, _ := ed25519.GenerateKey(rand.Reader)
	lc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, cpPub: cpPub, cpPriv: cpPriv, relayID: "eu-1", conn: lc,
		now: time.Now().UnixMilli()}
	s, err := relay.New(relay.Config{
		ID: h.relayID, PublicKey: cpPub, Conn: lc,
		Now: func() int64 { return h.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	h.srv = s
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Serve(ctx) }()
	t.Cleanup(func() { cancel(); _ = lc.Close() })
	return h
}

func (h *harness) client() *net.UDPConn {
	h.t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = c.Close() })
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	return c
}

func (h *harness) send(c *net.UDPConn, dgram []byte) {
	h.t.Helper()
	raddr, _ := net.ResolveUDPAddr("udp", h.srv.Addr())
	if _, err := c.WriteToUDP(dgram, raddr); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) token(nodeID, groupID string, pub ed25519.PublicKey, exp int64, relayID string) string {
	tok, err := relaytoken.Sign(h.cpPriv, relaytoken.Claims{
		NodeID: nodeID, GroupID: groupID, RelayID: relayID,
		PublicKey: base64.StdEncoding.EncodeToString(pub), ExpUnixMs: exp,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return tok
}

func (h *harness) registerDatagram(nodeID, groupID string, pub ed25519.PublicKey,
	priv ed25519.PrivateKey, ts, exp int64, relayID, tokOverride string) []byte {
	tok := tokOverride
	if tok == "" {
		tok = h.token(nodeID, groupID, pub, exp, relayID)
	}
	reg := &meshv1.RelayRegister{
		Token: tok, NodeId: nodeID, TimestampUnixMs: ts,
		Signature: meshproto.SignRegister(priv, relayID, nodeID, ts),
		RelayId:   relayID,
	}
	d, err := meshproto.EncodeProtoDgram(meshproto.DgramRegister, reg)
	if err != nil {
		h.t.Fatal(err)
	}
	return d
}

func (h *harness) readDgram(c *net.UDPConn) []byte {
	h.t.Helper()
	buf := make([]byte, 64*1024)
	n, _, err := c.ReadFromUDP(buf)
	if err != nil {
		h.t.Fatal(err)
	}
	return buf[:n]
}

func (h *harness) mustRegister(c *net.UDPConn, nodeID, groupID string) (ed25519.PublicKey, ed25519.PrivateKey) {
	h.t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	h.send(c, h.registerDatagram(nodeID, groupID, pub, priv, h.now, h.now+3600_000, h.relayID, ""))
	d := h.readDgram(c)
	var rr meshv1.RelayRegistered
	typ, err := meshproto.DecodeProtoDgram(d, &rr)
	if err != nil || typ != meshproto.DgramRegistered {
		h.t.Fatalf("expected REGISTERED, got %x %v", d, err)
	}
	return pub, priv
}

func TestRegisterAndForward(t *testing.T) {
	h := newHarness(t)
	ca, cb := h.client(), h.client()
	h.mustRegister(ca, "node_a", "grp_1")
	h.mustRegister(cb, "node_b", "grp_1")
}

func TestDataForwardingSameGroup(t *testing.T) {
	h := newHarness(t)
	ca, cb := h.client(), h.client()
	h.mustRegister(ca, "node_a", "grp_1")
	h.mustRegister(cb, "node_b", "grp_1")

	h.send(ca, meshproto.EncodeDataBytes("node_b", []byte("QUIC ciphertext")))
	d := h.readDgram(cb)
	src, pl, err := meshproto.DecodeData(d)
	if err != nil || src != "node_a" || string(pl) != "QUIC ciphertext" {
		t.Fatalf("src=%q pl=%q err=%v", src, pl, err)
	}
}

func TestCrossGroupNoForwarding(t *testing.T) {
	h := newHarness(t)
	ca, cb := h.client(), h.client()
	h.mustRegister(ca, "node_a", "grp_1")
	h.mustRegister(cb, "node_b", "grp_2")

	h.send(ca, meshproto.EncodeDataBytes("node_b", []byte("x")))
	// cb gets nothing; ca gets ERROR forbidden
	var ce meshv1.RelayError
	typ, err := meshproto.DecodeProtoDgram(h.readDgram(ca), &ce)
	if err != nil || typ != meshproto.DgramError || ce.Code != meshv1.RelayError_CODE_FORBIDDEN {
		t.Fatalf("expected FORBIDDEN, got %v %v", typ, ce.Code)
	}
}

func TestBadToken(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	// token signed by a different key
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	badTok, _ := relaytoken.Sign(otherPriv, relaytoken.Claims{
		NodeID: "node_a", GroupID: "g", RelayID: h.relayID,
		PublicKey: base64.StdEncoding.EncodeToString(pub), ExpUnixMs: h.now + 1000,
	})
	h.send(c, h.registerDatagram("node_a", "g", pub, priv, h.now, 0, h.relayID, badTok))
	var ce meshv1.RelayError
	typ, _ := meshproto.DecodeProtoDgram(h.readDgram(c), &ce)
	if typ != meshproto.DgramError || ce.Code != meshv1.RelayError_CODE_UNAUTHENTICATED {
		t.Fatalf("expected UNAUTHENTICATED got %v", ce.Code)
	}
}

func TestExpiredToken(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tok := h.token("node_a", "g", pub, h.now-1000, h.relayID) // expired
	h.send(c, h.registerDatagram("node_a", "g", pub, priv, h.now, 0, h.relayID, tok))
	var ce meshv1.RelayError
	typ, _ := meshproto.DecodeProtoDgram(h.readDgram(c), &ce)
	if typ != meshproto.DgramError || ce.Code != meshv1.RelayError_CODE_UNAUTHENTICATED {
		t.Fatalf("expected UNAUTHENTICATED got %v", ce.Code)
	}
}

func TestBadNodeSignature(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, wrongPriv, _ := ed25519.GenerateKey(rand.Reader) // sign with a different key
	h.send(c, h.registerDatagram("node_a", "g", pub, wrongPriv, h.now, h.now+3600_000, h.relayID, ""))
	var ce meshv1.RelayError
	typ, _ := meshproto.DecodeProtoDgram(h.readDgram(c), &ce)
	if typ != meshproto.DgramError || ce.Code != meshv1.RelayError_CODE_UNAUTHENTICATED {
		t.Fatalf("expected UNAUTHENTICATED got %v", ce.Code)
	}
}

func TestReplayedOldTimestamp(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	old := h.now - 120_000 // beyond 60 s skew
	h.send(c, h.registerDatagram("node_a", "g", pub, priv, old, h.now+3600_000, h.relayID, ""))
	var ce meshv1.RelayError
	typ, _ := meshproto.DecodeProtoDgram(h.readDgram(c), &ce)
	if typ != meshproto.DgramError || ce.Code != meshv1.RelayError_CODE_UNAUTHENTICATED {
		t.Fatalf("expected UNAUTHENTICATED got %v", ce.Code)
	}
}

func TestPingPong(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	h.send(c, meshproto.EncodePing(0xCAFE))
	typ, nonce, err := meshproto.DecodeNonce(h.readDgram(c))
	if err != nil || typ != meshproto.DgramPong || nonce != 0xCAFE {
		t.Fatalf("typ=%x nonce=%x err=%v", typ, nonce, err)
	}
}
