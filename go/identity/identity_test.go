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

func TestNodeID(t *testing.T) {
	pub, _, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	id := NodeID(pub)
	if len(id) != len("node_")+16 {
		t.Fatalf("node id %q has unexpected length", id)
	}
}
