// Package identity handles node ed25519 keys in PKCS#8 PEM form and the
// derived node_id helpers.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
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

// NodeID derives a stable node identifier from a public key:
// "node_" + first 16 hex chars of SHA-256(pub).
func NodeID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "node_" + hex.EncodeToString(sum[:8])
}
