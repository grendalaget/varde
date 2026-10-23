package audiotransport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"sync"
	"time"
)

// Browsers trust a self-signed certificate by its hash (serverCertificateHashes) only if it is ECDSA
// and valid for at most 14 days. Its hash reaches the client over the authenticated WebSocket.
const (
	certValidity = 13 * 24 * time.Hour
	certRotate   = 6 * 24 * time.Hour
)

type certStore struct {
	mu   sync.Mutex
	ips  []net.IP
	cert *tls.Certificate
	hash string
	made time.Time
}

func newCertStore(ips []net.IP) *certStore { return &certStore{ips: ips} }

// current returns the certificate and its base64 SHA-256, making a new pair when the old one is due.
func (c *certStore) current(now time.Time) (*tls.Certificate, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cert != nil && now.Sub(c.made) < certRotate {
		return c.cert, c.hash, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return nil, "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "gryt-sfu-audio"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(certValidity),
		IPAddresses:  c.ips,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(der)
	c.cert = &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	c.hash = base64.StdEncoding.EncodeToString(sum[:])
	c.made = now
	return c.cert, c.hash, nil
}
