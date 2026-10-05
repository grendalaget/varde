package identity

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestVectorFile validates the canonical signing implementation against the
// committed cross-language test vector (Rust agent tests against the same
// file). If you change the signing format, regenerate this file.
func TestVectorFile(t *testing.T) {
	var v struct {
		SeedHex      string `json:"private_key_pkcs8_pem_b64_seed_hex"`
		PublicKeyB64 string `json:"public_key_base64"`
		Request      struct {
			Method    string `json:"method"`
			Path      string `json:"path"`
			Timestamp int64  `json:"timestamp_unix_ms"`
			Body      string `json:"body"`
		} `json:"request"`
		BodySHA256    string `json:"body_sha256_hex"`
		SigningString string `json:"signing_string"`
		Signature     string `json:"expected_signature_base64"`
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "architecture", "testdata", "agent-signature.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	seed, _ := hex.DecodeString(v.SeedHex)
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	if got := base64.StdEncoding.EncodeToString(pub); got != v.PublicKeyB64 {
		t.Fatalf("public key mismatch: %s", got)
	}
	ss := SigningString(v.Request.Method, v.Request.Path, v.Request.Timestamp, []byte(v.Request.Body))
	if ss != v.SigningString {
		t.Fatalf("signing string mismatch:\n got %q\nwant %q", ss, v.SigningString)
	}
	sig := SignRequest(priv, v.Request.Method, v.Request.Path, v.Request.Timestamp, []byte(v.Request.Body))
	if sig != v.Signature {
		t.Fatalf("signature mismatch:\n got %s\nwant %s", sig, v.Signature)
	}
	if err := VerifyRequest(pub, v.Request.Method, v.Request.Path, v.Request.Timestamp, []byte(v.Request.Body), sig); err != nil {
		t.Fatalf("VerifyRequest rejected valid signature: %v", err)
	}
	if err := VerifyRequest(pub, v.Request.Method, v.Request.Path, v.Request.Timestamp+1, []byte(v.Request.Body), sig); err == nil {
		t.Fatal("VerifyRequest accepted signature for wrong timestamp")
	}
}
