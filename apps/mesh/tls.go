package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// alpn is our application protocol tag.
const alpn = "varde/1"

// selfSignedCert builds a self-signed X.509 certificate carrying the node's
// ed25519 key. Peers authenticate by pinning the key, not the chain.
func selfSignedCert(priv ed25519.PrivateKey) (tls.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "varde-node"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, priv.Public(), priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: mustParse(der)}, nil
}

func mustParse(der []byte) *x509.Certificate {
	c, err := x509.ParseCertificate(der)
	if err != nil {
		panic(err)
	}
	return c
}

// peerPub extracts the ed25519 public key (b64) from a verified peer cert.
func peerPub(certs []*x509.Certificate) (string, ed25519.PublicKey, error) {
	if len(certs) == 0 {
		return "", nil, errors.New("no peer certificate")
	}
	pub, ok := certs[0].PublicKey.(ed25519.PublicKey)
	if !ok || len(pub) != ed25519.PublicKeySize {
		return "", nil, errors.New("peer certificate does not carry an ed25519 key")
	}
	return base64.StdEncoding.EncodeToString(pub), pub, nil
}

// tlsConfigFor builds the base TLS config: our cert, no chain verification —
// pinning happens in VerifyPeerCertificate against the SetPeers key list.
func (n *Node) tlsConfigFor(expectPeer string) *tls.Config {
	return &tls.Config{
		Certificates:       []tls.Certificate{n.cert},
		NextProtos:         []string{alpn},
		InsecureSkipVerify: true,                     //nolint:gosec // pinning done manually below
		ClientAuth:         tls.RequireAnyClientCert, // mTLS: always demand a client cert
		MinVersion:         tls.VersionTLS13,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("peer presented no certificate")
			}
			cert, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return fmt.Errorf("bad peer certificate: %w", err)
			}
			pub, ok := cert.PublicKey.(ed25519.PublicKey)
			if !ok {
				return errors.New("peer certificate is not ed25519")
			}
			return n.verifyPeerKey(expectPeer, pub)
		},
	}
}

// verifyPeerKey pins the presented key: when expectPeer is set (outbound
// dials) the key must match that exact peer; for inbound accepts the key must
// belong to some configured peer.
func (n *Node) verifyPeerKey(expectPeer string, pub ed25519.PublicKey) error {
	n.mu.RLock()
	defer n.mu.RUnlock()
	b64 := base64.StdEncoding.EncodeToString(pub)
	if expectPeer != "" {
		p, ok := n.peers[expectPeer]
		if !ok {
			return fmt.Errorf("unknown peer %q", expectPeer)
		}
		if p.pubB64 != b64 {
			return fmt.Errorf("peer %s key mismatch", expectPeer)
		}
		return nil
	}
	if _, ok := n.peersByPub[b64]; ok {
		return nil
	}
	n.log.Debug("rejecting unknown peer key", "key", b64[:16], "peers", len(n.peersByPub))
	return fmt.Errorf("unknown peer key %s", b64[:16])
}
