package audiotransport

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func packet(n int) []byte {
	b := make([]byte, headerBytes+n)
	b[0] = packetVersion
	return b
}

func TestValidDatagram(t *testing.T) {
	cases := map[string]struct {
		b    []byte
		want bool
	}{
		"one opus byte":    {packet(1), true},
		"opus maximum":     {packet(maxOpusBytes), true},
		"header only":      {packet(0), false},
		"over the maximum": {packet(maxOpusBytes + 1), false},
		"empty":            {nil, false},
		"another version":  {append([]byte{2}, packet(10)[1:]...), false},
	}
	for name, c := range cases {
		if got := validDatagram(c.b); got != c.want {
			t.Errorf("%s: validDatagram = %v, want %v", name, got, c.want)
		}
	}
}

func TestBucketCapsTheRate(t *testing.T) {
	var b bucket
	now := time.Unix(0, 0)
	allowed := 0
	// A second of a client sending every millisecond, 2.5x what a 2.5 ms sender needs.
	for i := 0; i < 1000; i++ {
		if b.allow(now.Add(time.Duration(i) * time.Millisecond)) {
			allowed++
		}
	}
	if allowed > ratePerSecond+rateBurst || allowed < ratePerSecond {
		t.Fatalf("allowed %d in a second, want about %d", allowed, ratePerSecond)
	}
	// A normal sender, one every 2.5 ms, is never dropped.
	var steady bucket
	for i := 0; i < 4000; i++ {
		if !steady.allow(now.Add(time.Duration(i) * 2500 * time.Microsecond)) {
			t.Fatalf("a 2.5 ms sender was dropped at packet %d", i)
		}
	}
}

func TestTicketsAreSingleUseAndExpire(t *testing.T) {
	ts := newTickets()
	now := time.Unix(1000, 0)
	tok, err := ts.issue("peer-a", "room-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) < 40 {
		t.Fatalf("ticket %q is too short to be unguessable", tok)
	}
	got, ok := ts.redeem(tok, now.Add(time.Second))
	if !ok || got.peerID != "peer-a" || got.roomID != "room-1" {
		t.Fatalf("redeem = %+v, %v", got, ok)
	}
	if _, ok := ts.redeem(tok, now.Add(2*time.Second)); ok {
		t.Fatal("a ticket worked twice")
	}
	late, _ := ts.issue("peer-b", "room-1", now)
	if _, ok := ts.redeem(late, now.Add(ticketTTL+time.Second)); ok {
		t.Fatal("an expired ticket worked")
	}
	if _, ok := ts.redeem("made-up", now); ok {
		t.Fatal("a ticket nobody issued worked")
	}
}

func TestCertIsWhatBrowsersAcceptByHash(t *testing.T) {
	ip := net.ParseIP("203.0.113.7")
	cs := newCertStore([]net.IP{ip})
	now := time.Now()
	cert, hash, err := cs.current(now)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if leaf.NotAfter.Sub(leaf.NotBefore) > 14*24*time.Hour {
		t.Fatalf("valid for %v; browsers refuse a hash-pinned cert over 14 days", leaf.NotAfter.Sub(leaf.NotBefore))
	}
	if leaf.PublicKeyAlgorithm != x509.ECDSA {
		t.Fatal("browsers only take ECDSA for a hash-pinned cert")
	}
	sum := sha256.Sum256(cert.Certificate[0])
	if hash != base64.StdEncoding.EncodeToString(sum[:]) {
		t.Fatal("the advertised hash isn't the certificate's")
	}
	if !leaf.IPAddresses[0].Equal(ip) {
		t.Fatal("the cert doesn't name the address clients connect to")
	}
	_, again, _ := cs.current(now.Add(time.Hour))
	_, rotated, _ := cs.current(now.Add(certRotate + time.Minute))
	if again != hash || rotated == hash {
		t.Fatal("the cert should stay for certRotate, then change")
	}
}

type fakeSession struct{ got [][]byte }

func (f *fakeSession) SendDatagram(b []byte) error {
	f.got = append(f.got, append([]byte(nil), b...))
	return nil
}

func TestHubForwardsToOthersWithTheSessionsID(t *testing.T) {
	h := newHub()
	a, b, c, other := &fakeSession{}, &fakeSession{}, &fakeSession{}, &fakeSession{}
	ma := h.join("room-1", "a", a, nil)
	h.join("room-1", "b", b, nil)
	h.join("room-1", "c", c, nil)
	h.join("room-2", "x", other, nil)

	pkt := packet(20)
	// Whatever a sender writes in front, the id that goes out is its session's.
	h.forward(ma, pkt, time.Now())
	if len(a.got) != 0 {
		t.Fatal("a sender heard itself")
	}
	if len(other.got) != 0 {
		t.Fatal("audio crossed into another room")
	}
	for _, s := range []*fakeSession{b, c} {
		if len(s.got) != 1 || binary.BigEndian.Uint32(s.got[0]) != ma.id || len(s.got[0]) != senderBytes+len(pkt) {
			t.Fatalf("a listener got %v", s.got)
		}
	}

	h.forward(ma, packet(0), time.Now())
	if h.dropped.malformed != 1 || len(b.got) != 1 {
		t.Fatal("a malformed datagram was forwarded")
	}
}

func TestHubRejoinClosesTheOldSessionAndDropEndsIt(t *testing.T) {
	h := newHub()
	closed := 0
	old := h.join("room-1", "a", &fakeSession{}, func() { closed++ })
	h.join("room-1", "a", &fakeSession{}, func() { closed++ })
	if closed != 1 {
		t.Fatal("a reconnect left the old session open")
	}
	h.leave(old)
	if len(h.rooms["room-1"]) != 1 {
		t.Fatal("the old session leaving removed the new one")
	}
	h.drop("a")
	if closed != 2 || len(h.rooms) != 0 {
		t.Fatal("dropping a peer didn't close and remove its session")
	}
}

func TestHubSaysWhoSendsUnderWhichIDOnJoinAndLeave(t *testing.T) {
	h := newHub()
	var got []map[string]uint32
	h.changed = func(roomID string) {
		if roomID == "r" {
			got = append(got, h.senders(roomID))
		}
	}
	a := h.join("r", "peer-a", &fakeSession{}, nil)
	b := h.join("r", "peer-b", &fakeSession{}, nil)
	h.leave(a)
	h.leave(a) // already gone: no second change
	if len(got) != 3 {
		t.Fatalf("changes = %d, want 3: %v", len(got), got)
	}
	if got[1]["peer-a"] != a.id || got[1]["peer-b"] != b.id || len(got[2]) != 1 || got[2]["peer-b"] != b.id {
		t.Fatalf("senders = %v", got)
	}
}

func TestHandlerRefusesWithoutWebTransportOrTicket(t *testing.T) {
	s := New(0, []string{"203.0.113.7"})
	get := httptest.NewRecorder()
	s.handle(get, httptest.NewRequest(http.MethodGet, "/audio", nil))
	if get.Code != http.StatusBadRequest {
		t.Fatalf("a plain GET got %d", get.Code)
	}
	req := httptest.NewRequest(http.MethodConnect, "/audio?ticket=made-up", nil)
	req.Proto = "webtransport-h3"
	rec := httptest.NewRecorder()
	s.handle(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a made-up ticket got %d", rec.Code)
	}
}

func TestOfferNamesEveryAddress(t *testing.T) {
	s := New(4433, []string{"203.0.113.7", "2001:db8::1"})
	o, err := s.Offer("a", "room-1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://203.0.113.7:4433/audio", "https://[2001:db8::1]:4433/audio"}
	if len(o.URLs) != 2 || o.URLs[0] != want[0] || o.URLs[1] != want[1] || o.CertHash == "" || o.Ticket == "" {
		t.Fatalf("offer = %+v", o)
	}
}
