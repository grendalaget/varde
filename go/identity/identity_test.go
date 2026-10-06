package identity

import (
	"crypto/ed25519"
	"path/filepath"
	"testing"
)

func TestRoundTripPEM(t *testing.T) {
	pub, priv, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	pemBytes, err := MarshalPrivateKeyPEM(priv)
	if err != nil {
		t.Fatalf("MarshalPrivateKeyPEM: %v", err)
	}
	loaded, err := ParsePrivateKeyPEM(pemBytes)
	if err != nil {
		t.Fatalf("ParsePrivateKeyPEM: %v", err)
	}
	if !ed25519.PublicKey(pub).Equal(loaded.Public().(ed25519.PublicKey)) {
		t.Fatal("public key mismatch after PEM round-trip")
	}
}

func TestSaveLoad(t *testing.T) {
	_, priv, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	path := filepath.Join(t.TempDir(), "node.key")
	if err := SavePrivateKey(path, priv); err != nil {
		t.Fatalf("SavePrivateKey: %v", err)
	}
	if _, err := LoadPrivateKey(path); err != nil {
		t.Fatalf("LoadPrivateKey: %v", err)
	}
}

func TestFingerprint(t *testing.T) {
	pub, _, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	id := Fingerprint(pub)
	if len(id) != len("node_")+16 {
		t.Fatalf("fingerprint %q has unexpected length", id)
	}
}

func TestSignVerify(t *testing.T) {
	pub, priv, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	body := []byte(`{"a":1}`)
	sig := SignRequest(priv, "POST", "/v1/agent/heartbeat", 1790000000000, body)
	if err := VerifyRequest(pub, "POST", "/v1/agent/heartbeat", 1790000000000, body, sig); err != nil {
		t.Fatalf("VerifyRequest: %v", err)
	}
	if err := VerifyRequest(pub, "POST", "/v1/agent/heartbeat", 1790000000001, body, sig); err == nil {
		t.Fatal("expected failure on timestamp mismatch")
	}
	if err := VerifyRequest(pub, "POST", "/v1/agent/heartbeat", 1790000000000, []byte("{}"), sig); err == nil {
		t.Fatal("expected failure on body mismatch")
	}
}
