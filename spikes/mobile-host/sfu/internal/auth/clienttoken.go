// Package auth verifies that a client connecting to the SFU was sent here by the server that
// owns the room. The shared password reached every browser, so it was never a secret.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Token versions prefix every token so the format can change. v1 is still accepted and means
// every capability: reading it as "may not speak" would mute a server on an older build.
const (
	TokenVersion  = "v1"
	TokenVersion2 = "v2"
)

// CapSpeak is the capability to publish microphone audio, and gates the microphone only —
// screen-share audio is `share_screen` on the server and a different transceiver.
const CapSpeak = "speak"

// CapShareVideo gates the camera and CapShareScreen a share's video and audio. Each is
// only read from a token that also carries CapVideoChecked — see MayShare.
const (
	CapShareVideo  = "share_video"
	CapShareScreen = "share_screen"
)

// CapVideoChecked says the server decided both video capabilities. A server that predates
// them never sends it, and its members keep the video they had.
const CapVideoChecked = "video_checked"

var (
	ErrMalformed = errors.New("client token is malformed")
	ErrSignature = errors.New("client token signature does not verify")
	ErrExpired   = errors.New("client token has expired")
	ErrMismatch  = errors.New("client token is for a different user or room")
)

var enc = base64.RawURLEncoding

// Claims is what a verified token says the bearer may do.
type Claims struct {
	// Capabilities the server granted. Nil or empty means none were granted,
	// which is only reachable from a v2 token — see Verify.
	Capabilities []string
}

// Can reports whether the token granted a capability.
func (c Claims) Can(capability string) bool {
	for _, got := range c.Capabilities {
		if got == capability {
			return true
		}
	}
	return false
}

// MayShare answers for CapShareVideo or CapShareScreen. Absent the marker the server never
// decided, so the answer is yes, as it was before the SFU looked.
func (c Claims) MayShare(capability string) bool {
	if !c.Can(CapVideoChecked) {
		return true
	}
	return c.Can(capability)
}

// Sign produces a v1 token binding a user to a room until expiresAt. The room and user are
// inside the signed payload, so a token minted for one room cannot be replayed into another.
func Sign(secret, userID, roomID, nonce string, expiresAt time.Time) string {
	payload := fmt.Sprintf("%s|%s|%d|%s", userID, roomID, expiresAt.UnixMilli(), nonce)
	return TokenVersion + "." + sealed(secret, payload)
}

// SignV2 produces a token that also says what the bearer may do. The capability list is
// inside the signed payload, so a client cannot add `speak` to a token minted without it.
func SignV2(secret, userID, roomID, nonce string, expiresAt time.Time, capabilities []string) string {
	payload := fmt.Sprintf("%s|%s|%d|%s|%s", userID, roomID, expiresAt.UnixMilli(), nonce, strings.Join(capabilities, ","))
	return TokenVersion2 + "." + sealed(secret, payload)
}

func sealed(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return enc.EncodeToString([]byte(payload)) + "." + enc.EncodeToString(mac.Sum(nil))
}

// Verify checks the signature, the expiry and the exact user and room, then reports what the
// bearer may do. A v1 token grants every capability — see the comment on TokenVersion.
func Verify(secret, token, roomID, userID string, now time.Time) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, ErrMalformed
	}
	version := parts[0]
	if version != TokenVersion && version != TokenVersion2 {
		return Claims{}, ErrMalformed
	}
	payload, err := enc.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	sig, err := enc.DecodeString(parts[2])
	if err != nil {
		return Claims{}, ErrMalformed
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return Claims{}, ErrSignature
	}

	fields := strings.Split(string(payload), "|")
	// v1 is four fields, v2 adds the capability list. An exact count per version rather than
	// a minimum, so a token with trailing rubbish is rejected.
	wantFields := 4
	if version == TokenVersion2 {
		wantFields = 5
	}
	if len(fields) != wantFields {
		return Claims{}, ErrMalformed
	}
	gotUser, gotRoom, expiryRaw := fields[0], fields[1], fields[2]

	expiryMs, err := strconv.ParseInt(expiryRaw, 10, 64)
	if err != nil {
		return Claims{}, ErrMalformed
	}
	if now.After(time.UnixMilli(expiryMs)) {
		return Claims{}, ErrExpired
	}

	// Compared in constant time out of habit rather than necessity: these are
	// not secrets, but they are attacker-supplied and it costs nothing.
	if !hmac.Equal([]byte(gotUser), []byte(userID)) || !hmac.Equal([]byte(gotRoom), []byte(roomID)) {
		return Claims{}, ErrMismatch
	}

	if version == TokenVersion {
		return Claims{Capabilities: []string{CapSpeak}}, nil
	}
	return Claims{Capabilities: splitCaps(fields[4])}, nil
}

// splitCaps turns the capability field into a list, without the single empty
// string strings.Split hands back for an empty input.
func splitCaps(raw string) []string {
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}
