// Package identity handles node ed25519 keys in PKCS#8 PEM form and the
// derived node_id helpers.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
)

const keyFilePerm = 0o600

// Generate returns a fresh ed25519 key pair.
func Generate() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate ed25519 key: %w", err)
	}
	return pub, priv, nil
}

// MarshalPrivateKeyPEM encodes an ed25519 private key as PKCS#8 PEM.
func MarshalPrivateKeyPEM(priv ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal PKCS#8: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// ParsePrivateKeyPEM decodes a PKCS#8 PEM ed25519 private key.
func ParsePrivateKeyPEM(data []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("identity: no PEM block found")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKCS#8: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("identity: unexpected key type %T", key)
	}
	return priv, nil
}

// SavePrivateKey writes the key to path as PKCS#8 PEM with mode 0600.
func SavePrivateKey(path string, priv ed25519.PrivateKey) error {
	pemBytes, err := MarshalPrivateKeyPEM(priv)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, pemBytes, keyFilePerm); err != nil {
		return fmt.Errorf("write key %s: %w", path, err)
	}
	return nil
}

// LoadPrivateKey reads a PKCS#8 PEM private key from path.
func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read key %s: %w", path, err)
	}
	return ParsePrivateKeyPEM(data)
}

// Fingerprint derives a stable, human-readable label from a public key:
// "node_" + first 16 hex chars of SHA-256(pub). It is a pre-enrollment log
// label only — the control plane assigns the real node_id (node_ + 20
// base32 chars) at enrollment and the agent uses that id everywhere after.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "node_" + hex.EncodeToString(sum[:8])
}

// AgentSigVersion prefixes the canonical agent request signing string.
const AgentSigVersion = "p2pgames-agent-v1"

// SigningString builds the canonical string an agent signs for a
// /v1/agent/* request:
//
//	"p2pgames-agent-v1\n" + METHOD + "\n" + PATH + "\n" + TIMESTAMP + "\n" + hex(sha256(body))
//
// timestamp is unix milliseconds formatted in decimal.
func SigningString(method, path string, timestampUnixMs int64, body []byte) string {
	sum := sha256.Sum256(body)
	return strings.Join([]string{
		AgentSigVersion,
		strings.ToUpper(method),
		path,
		fmt.Sprintf("%d", timestampUnixMs),
		hex.EncodeToString(sum[:]),
	}, "\n")
}

// SignRequest signs a request, returning the base64 signature to put in the
// X-P2PG-Signature header.
func SignRequest(priv ed25519.PrivateKey, method, path string, timestampUnixMs int64, body []byte) string {
	sig := ed25519.Sign(priv, []byte(SigningString(method, path, timestampUnixMs, body)))
	return base64.StdEncoding.EncodeToString(sig)
}

// VerifyRequest checks an X-P2PG-Signature value against the request.
func VerifyRequest(pub ed25519.PublicKey, method, path string, timestampUnixMs int64, body []byte, sigB64 string) error {
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("signature not base64: %w", err)
	}
	if !ed25519.Verify(pub, []byte(SigningString(method, path, timestampUnixMs, body)), sig) {
		return errors.New("invalid signature")
	}
	return nil
}
