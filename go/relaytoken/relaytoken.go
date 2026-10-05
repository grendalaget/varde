// Package relaytoken implements the CP-signed relay token format shared by
// the control plane (signer) and relays (verifier).
//
// Token = base64url(json claims) + "." + base64url(ed25519 sig over the json).
package relaytoken

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Claims carried in the token.
type Claims struct {
	NodeID    string `json:"node_id"`
	PublicKey string `json:"pk"` // base64 node public key
	GroupID   string `json:"group_id"`
	RelayID   string `json:"relay_id"`
	ExpUnixMs int64  `json:"exp_unix_ms"`
}

// Sign creates a token for nodeID valid until expUnixMs.
func Sign(priv ed25519.PrivateKey, c Claims) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(priv, payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(sig), nil
}

// Verify checks signature, expiry (against nowMs) and relay id.
func Verify(pub ed25519.PublicKey, token string, wantRelayID string, nowMs int64) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return nil, errors.New("relaytoken: malformed token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("relaytoken: payload: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("relaytoken: signature: %w", err)
	}
	if !ed25519.Verify(pub, payload, sig) {
		return nil, errors.New("relaytoken: bad signature")
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, fmt.Errorf("relaytoken: claims: %w", err)
	}
	if c.ExpUnixMs <= nowMs {
		return nil, errors.New("relaytoken: expired")
	}
	if c.RelayID != wantRelayID {
		return nil, fmt.Errorf("relaytoken: relay_id %q != %q", c.RelayID, wantRelayID)
	}
	return &c, nil
}
