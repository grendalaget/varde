// Package gatewaytoken defines the CP-signed public gateway tunnel capability.
// Wire format matches relaytoken: base64url(JSON) + "." + base64url(Ed25519(JSON)).
package gatewaytoken

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

const Audience = "varde.gateway.v1"

type Claims struct {
	Audience  string `json:"aud"`
	Domain    string `json:"domain"`
	Label     string `json:"label"`
	GroupID   string `json:"group_id"`
	ServerID  string `json:"server_id"`
	Epoch     uint64 `json:"epoch"`
	ExpUnixMs int64  `json:"exp_unix_ms"`
}

func ValidLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, c := range s {
		allowed := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-'
		if !allowed {
			return false
		}
	}
	return true
}

func ValidDomain(s string) bool {
	if len(s) > 253 || !strings.Contains(s, ".") {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if !ValidLabel(label) {
			return false
		}
	}
	return true
}

func Sign(priv ed25519.PrivateKey, c Claims) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", errors.New("gatewaytoken: invalid private key")
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, payload)), nil
}

func Verify(pub ed25519.PublicKey, token, domain string, nowMs int64) (*Claims, error) {
	if len(pub) != ed25519.PublicKeySize || len(token) > 8192 {
		return nil, errors.New("gatewaytoken: invalid key or token size")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return nil, errors.New("gatewaytoken: malformed token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("gatewaytoken: invalid payload")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !ed25519.Verify(pub, payload, sig) {
		return nil, errors.New("gatewaytoken: invalid signature")
	}
	var c Claims
	if json.Unmarshal(payload, &c) != nil || c.Audience != Audience || c.Domain != domain || !ValidDomain(c.Domain) || !ValidLabel(c.Label) || c.GroupID == "" || c.ServerID == "" || c.Epoch == 0 || c.ExpUnixMs <= nowMs {
		return nil, errors.New("gatewaytoken: invalid or expired claims")
	}
	return &c, nil
}
