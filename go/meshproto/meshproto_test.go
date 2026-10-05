package meshproto

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	meshv1 "github.com/arnemolland/p2pgames/go/gen/mesh/v1"
)

func FuzzReadFrame(f *testing.F) {
	f.Add([]byte{0x03, 0x08, 0x01, 0x00})
	f.Add([]byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		var acc meshv1.StreamAccept
		_ = ReadFrame(bytes.NewReader(data), &acc)
	})
}

func FuzzDecodeData(f *testing.F) {
	f.Add([]byte{0x03, 0x04, 'n', 'o', 'd', 'e', 0x01})
	f.Add([]byte{0x03})
	f.Add([]byte{0x03, 0xff, 0x01})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _ = DecodeData(data)
	})
}

func FuzzDecodeFlowDatagram(f *testing.F) {
	f.Add([]byte{0x81, 0x22, 0xAA})
	f.Add([]byte{0x80})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _ = DecodeFlowDatagram(data)
	})
}

func FuzzDecodeNonce(f *testing.F) {
	f.Add(EncodePing(42))
	f.Add(EncodePong(1))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _ = DecodeNonce(data)
	})
}

func FuzzDecodeProtoDgram(f *testing.F) {
	f.Add([]byte{0x02, 0x0a, 0x03, 'a', 'b', 'c'})
	f.Fuzz(func(t *testing.T, data []byte) {
		var rr meshv1.RelayRegistered
		_, _ = DecodeProtoDgram(data, &rr)
	})
}

func TestFrameRoundTrip(t *testing.T) {
	in := &meshv1.StreamAccept{Ok: true, Reason: meshv1.RejectReason_REJECT_REASON_STALE_EPOCH, Message: "m"}
	var buf bytes.Buffer
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatal(err)
	}
	var out meshv1.StreamAccept
	if err := ReadFrame(&buf, &out); err != nil {
		t.Fatal(err)
	}
	if !out.Ok || out.Reason != in.Reason || out.Message != "m" {
		t.Fatalf("round trip: %+v", &out)
	}
}

func TestDataRoundTrip(t *testing.T) {
	d := EncodeDataBytes("node_abc", []byte{1, 2, 3})
	id, pl, err := DecodeData(d)
	if err != nil || id != "node_abc" || !bytes.Equal(pl, []byte{1, 2, 3}) {
		t.Fatalf("id=%q pl=%v err=%v", id, pl, err)
	}
	if _, _, err := DecodeData([]byte{0x02, 0x00}); err == nil {
		t.Fatal("accepted non-DATA")
	}
}

func TestNonceRoundTrip(t *testing.T) {
	typ, n, err := DecodeNonce(EncodePong(0xdeadbeef))
	if err != nil || typ != DgramPong || n != 0xdeadbeef {
		t.Fatal(err)
	}
}

func TestFlowDatagramRoundTrip(t *testing.T) {
	d := EncodeFlowDatagram(1<<40+7, []byte("hello"))
	id, pl, err := DecodeFlowDatagram(d)
	if err != nil || id != 1<<40+7 || string(pl) != "hello" {
		t.Fatalf("id=%d pl=%q err=%v", id, pl, err)
	}
}

func TestRegisterSignature(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sig := SignRegister(priv, "eu-1", "node_x", 1234)
	if !VerifyRegister(pub, "eu-1", "node_x", 1234, sig) {
		t.Fatal("verify failed")
	}
	if VerifyRegister(pub, "eu-2", "node_x", 1234, sig) {
		t.Fatal("wrong relay accepted")
	}
	if got := RegisterSigningString("eu-1", "node_x", 1234); got != "p2pgames-relay-register|eu-1|node_x|1234" {
		t.Fatalf("signing string %q", got)
	}
}
