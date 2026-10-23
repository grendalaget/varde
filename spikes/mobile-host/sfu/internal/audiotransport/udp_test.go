package audiotransport

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

// The same inputs through @gryt/voice's native/core/seal.c gave these bytes, so Go and C seal alike.
func TestSealMatchesTheNativeEngine(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	aead, _ := chacha20poly1305.New(key)
	opus := []byte("opus-bytes")
	for _, c := range []struct {
		dir     byte
		counter uint32
		clear   string
		want    string
	}{
		{dirToSFU, 5, "0102030405060708", "01020304000000050102030405060708a3b8966f0ffde047ccfdc15fc608cb7d42ee1146daf42a5e9964"},
		{dirFromSFU, 9, "000000070102030405060708", "010203040000000900000007010203040506070808b927d4b10d2e2494c787421cf73624f3b7f78f1086a9385c89"},
	} {
		clear, _ := hex.DecodeString(c.clear)
		if got := hex.EncodeToString(seal(aead, c.dir, 0x01020304, c.counter, clear, opus)); got != c.want {
			t.Fatalf("dir %d: Go sealed %s, C sealed %s", c.dir, got, c.want)
		}
	}
}

func TestOpenRefusesForgeriesReplaysAndTheWrongDirection(t *testing.T) {
	aead, _ := chacha20poly1305.New(make([]byte, 32))
	clear := []byte{1, 0, 0, 1, 0, 0, 0, 0}
	b := seal(aead, dirToSFU, 1, 100, clear, []byte{9, 9, 9})
	var r replay
	if _, _, ok := open(aead, dirFromSFU, b, 8, &r); ok {
		t.Fatal("opened under the wrong direction")
	}
	bad := append([]byte{}, b...)
	bad[len(bad)-1] ^= 1
	if _, _, ok := open(aead, dirToSFU, bad, 8, &r); ok {
		t.Fatal("opened a tampered packet")
	}
	if _, secret, ok := open(aead, dirToSFU, b, 8, &r); !ok || !bytes.Equal(secret, []byte{9, 9, 9}) {
		t.Fatal("refused a genuine packet")
	}
	if _, _, ok := open(aead, dirToSFU, b, 8, &r); ok {
		t.Fatal("accepted a replay")
	}
	if _, _, ok := open(aead, dirToSFU, seal(aead, dirToSFU, 1, 30, clear, []byte{1}), 8, &r); ok {
		t.Fatal("accepted a counter more than 64 behind")
	}
}

func dialOffer(t *testing.T, o *UDPOffer) (*net.UDPConn, *net.UDPAddr, []byte) {
	t.Helper()
	key, _ := base64.StdEncoding.DecodeString(o.Key)
	sfu, err := net.ResolveUDPAddr("udp", o.Addrs[0])
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, sfu, key
}

// Two clients in a room over real UDP: A's packet reaches B with A's sender id, sealed for B; nothing else does.
func TestUDPForwardsSealedForEachReceiver(t *testing.T) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	u := &udpListener{conn: conn, hosts: []string{"127.0.0.1"}, port: conn.LocalAddr().(*net.UDPAddr).Port, hub: newHub(), sessions: map[uint32]*udpSession{}}
	go func() { _ = u.serve() }()

	oa, _ := u.offer("a", "room", time.Now())
	ob, _ := u.offer("b", "room", time.Now())
	ca, sfu, keyA := dialOffer(t, oa)
	cb, _, keyB := dialOffer(t, ob)
	aeadA, _ := chacha20poly1305.New(keyA)
	aeadB, _ := chacha20poly1305.New(keyB)

	// B says something first, so the SFU knows where B is; it goes nowhere, A hasn't joined.
	hello := packet(10)
	if _, err := cb.WriteToUDP(seal(aeadB, dirToSFU, ob.Session, 1, hello[:headerBytes], hello[headerBytes:]), sfu); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	// Garbage, an unknown session and a forged packet are dropped unread.
	_, _ = ca.WriteToUDP([]byte("not a packet at all, but long enough"), sfu)
	forged := seal(aeadB, dirToSFU, oa.Session, 1, hello[:headerBytes], hello[headerBytes:])
	_, _ = ca.WriteToUDP(forged, sfu)

	pkt := packet(20)
	if _, err := ca.WriteToUDP(seal(aeadA, dirToSFU, oa.Session, 1, pkt[:headerBytes], pkt[headerBytes:]), sfu); err != nil {
		t.Fatal(err)
	}
	_ = cb.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2048)
	n, _, err := cb.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("B heard nothing: %v", err)
	}
	var r replay
	clear, secret, ok := open(aeadB, dirFromSFU, buf[:n], senderBytes+headerBytes, &r)
	if !ok {
		t.Fatal("B couldn't open what the SFU sent it")
	}
	if binary.BigEndian.Uint32(buf) != ob.Session {
		t.Fatal("sealed under the wrong session")
	}
	if !bytes.Equal(clear[senderBytes:], pkt[:headerBytes]) || !bytes.Equal(secret, pkt[headerBytes:]) {
		t.Fatal("the packet changed on the way")
	}
	if binary.BigEndian.Uint32(clear) == 0 {
		t.Fatal("no sender id in front")
	}
	u.mu.Lock()
	unknown, forgedCount := u.dropped.unknown, u.dropped.forged
	u.mu.Unlock()
	if unknown != 1 || forgedCount != 1 {
		t.Fatalf("dropped %d unknown and %d forged, want 1 and 1", unknown, forgedCount)
	}

	// After a drop, A's session is gone: its packets aren't forwarded.
	u.hub.drop("a")
	u.forget("a")
	if _, err := ca.WriteToUDP(seal(aeadA, dirToSFU, oa.Session, 2, pkt[:headerBytes], pkt[headerBytes:]), sfu); err != nil {
		t.Fatal(err)
	}
	_ = cb.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, _, err := cb.ReadFromUDP(buf); err == nil {
		t.Fatal("a dropped peer's packet still reached B")
	}
}
