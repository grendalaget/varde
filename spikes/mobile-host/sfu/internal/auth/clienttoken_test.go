package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	secret = "shared-server-secret"
	user   = "user-abc"
	room   = "room-xyz"
)

func validToken(t *testing.T) string {
	t.Helper()
	return Sign(secret, user, room, "nonce-1", time.Now().Add(time.Minute))
}

func TestARoundTripVerifies(t *testing.T) {
	if _, err := Verify(secret, validToken(t), room, user, time.Now()); err != nil {
		t.Fatalf("verify = %v, want nil", err)
	}
}

// The whole point: the token is only worth anything to somebody holding the
// shared secret, which the browser no longer does.
func TestAnotherSecretDoesNotVerify(t *testing.T) {
	if _, err := Verify("some-other-secret", validToken(t), room, user, time.Now()); !errors.Is(err, ErrSignature) {
		t.Fatalf("verify = %v, want ErrSignature", err)
	}
}

// A token is a statement about one user in one room. Without this check the signature alone
// would let anybody with a valid token walk into any room.
func TestATokenForAnotherRoomIsRefused(t *testing.T) {
	if _, err := Verify(secret, validToken(t), "some-other-room", user, time.Now()); !errors.Is(err, ErrMismatch) {
		t.Fatalf("verify = %v, want ErrMismatch", err)
	}
}

func TestATokenForAnotherUserIsRefused(t *testing.T) {
	if _, err := Verify(secret, validToken(t), room, "someone-else", time.Now()); !errors.Is(err, ErrMismatch) {
		t.Fatalf("verify = %v, want ErrMismatch", err)
	}
}

func TestAnExpiredTokenIsRefused(t *testing.T) {
	tok := Sign(secret, user, room, "nonce-1", time.Now().Add(-time.Second))
	if _, err := Verify(secret, tok, room, user, time.Now()); !errors.Is(err, ErrExpired) {
		t.Fatalf("verify = %v, want ErrExpired", err)
	}
}

// Editing the payload invalidates the signature, so the claims inside it cannot
// be rewritten by whoever is holding the token.
func TestATamperedPayloadIsRefused(t *testing.T) {
	tok := validToken(t)
	parts := strings.Split(tok, ".")
	forged := Sign(secret, "attacker", room, "nonce-1", time.Now().Add(time.Minute))
	tampered := parts[0] + "." + strings.Split(forged, ".")[1] + "." + parts[2]

	if _, err := Verify(secret, tampered, room, "attacker", time.Now()); !errors.Is(err, ErrSignature) {
		t.Fatalf("verify = %v, want ErrSignature", err)
	}
}

func TestRubbishIsRefusedRatherThanPanicking(t *testing.T) {
	for _, tok := range []string{
		"", ".", "..", "v1", "v1.", "v1.a", "v1.a.b.c",
		"v2." + strings.Split(validToken(t), ".")[1] + ".sig",
		"v1.!!!not-base64!!!.also-not",
	} {
		if _, err := Verify(secret, tok, room, user, time.Now()); err == nil {
			t.Fatalf("verify(%q) = nil, want an error", tok)
		}
	}
}

// An empty secret must not become a universal key: a token signed with one is
// still only valid against that same empty secret, never against a real one.
func TestAnEmptySecretIsNotAMasterKey(t *testing.T) {
	tok := Sign("", user, room, "nonce-1", time.Now().Add(time.Minute))
	if _, err := Verify(secret, tok, room, user, time.Now()); !errors.Is(err, ErrSignature) {
		t.Fatalf("verify = %v, want ErrSignature", err)
	}
}

// ── Capabilities (v2) ────────────────────────────────────────────────

// The compatibility case: this SFU is released separately from the server, so it will run
// against servers still minting v1. Reading those as "no capabilities" would mute them.
func TestAV1TokenMaySpeak(t *testing.T) {
	claims, err := Verify(secret, validToken(t), room, user, time.Now())
	if err != nil {
		t.Fatalf("verify = %v, want nil", err)
	}
	if !claims.Can(CapSpeak) {
		t.Fatalf("a v1 token must grant %q, got %v", CapSpeak, claims.Capabilities)
	}
}

func TestAV2TokenGrantsWhatItCarries(t *testing.T) {
	tok := SignV2(secret, user, room, "n", time.Now().Add(time.Minute), []string{CapSpeak})
	claims, err := Verify(secret, tok, room, user, time.Now())
	if err != nil {
		t.Fatalf("verify = %v, want nil", err)
	}
	if !claims.Can(CapSpeak) {
		t.Fatalf("want %q granted, got %v", CapSpeak, claims.Capabilities)
	}
}

func TestAV2TokenWithoutSpeakDoesNotGrantIt(t *testing.T) {
	tok := SignV2(secret, user, room, "n", time.Now().Add(time.Minute), nil)
	claims, err := Verify(secret, tok, room, user, time.Now())
	if err != nil {
		t.Fatalf("verify = %v, want nil", err)
	}
	if claims.Can(CapSpeak) {
		t.Fatalf("want %q withheld, got %v", CapSpeak, claims.Capabilities)
	}
}

// The whole point of putting capabilities inside the signed payload. A client
// that edits its own token to add `speak` must be refused, not believed.
func TestAddingACapabilityBreaksTheSignature(t *testing.T) {
	tok := SignV2(secret, user, room, "n", time.Now().Add(time.Minute), nil)
	parts := strings.Split(tok, ".")
	forged := SignV2("some-other-secret", user, room, "n", time.Now().Add(time.Minute), []string{CapSpeak})
	// Keep the real signature, swap in a payload that grants speak.
	tampered := parts[0] + "." + strings.Split(forged, ".")[1] + "." + parts[2]
	if _, err := Verify(secret, tampered, room, user, time.Now()); !errors.Is(err, ErrSignature) {
		t.Fatalf("verify = %v, want ErrSignature", err)
	}
}

// A v2 token still has to be for this user and this room. The capability field
// is extra, not a replacement for what v1 already checked.
func TestAV2TokenIsStillBoundToItsRoom(t *testing.T) {
	tok := SignV2(secret, user, room, "n", time.Now().Add(time.Minute), []string{CapSpeak})
	if _, err := Verify(secret, tok, "some-other-room", user, time.Now()); !errors.Is(err, ErrMismatch) {
		t.Fatalf("verify = %v, want ErrMismatch", err)
	}
}

// A v1 token carrying a fifth field, or a v2 carrying four, is malformed rather
// than read as far as it parses.
func TestAVersionMustMatchItsFieldCount(t *testing.T) {
	v2Payload := strings.Split(SignV2(secret, user, room, "n", time.Now().Add(time.Minute), nil), ".")[1]
	v1Sig := strings.Split(Sign(secret, user, room, "n", time.Now().Add(time.Minute)), ".")[2]
	if _, err := Verify(secret, TokenVersion+"."+v2Payload+"."+v1Sig, room, user, time.Now()); err == nil {
		t.Fatal("a v1 token with five fields verified, want an error")
	}
}

// ── The vectors the server pins too ──────────────────────────────────
// `src/sfu/clientToken.test.ts` asserts these exact strings. The server signs and this
// verifies, so the two agreeing byte for byte is the whole contract.

const (
	vectorSecret = "shared-server-secret"
	vectorUser   = "user-abc"
	vectorRoom   = "room-xyz"
	vectorNonce  = "nonce-1"
	vectorExpiry = 1788000000000
)

func TestTheV1VectorTheServerPins(t *testing.T) {
	got := Sign(vectorSecret, vectorUser, vectorRoom, vectorNonce, time.UnixMilli(vectorExpiry))
	want := "v1.dXNlci1hYmN8cm9vbS14eXp8MTc4ODAwMDAwMDAwMHxub25jZS0x.sRfyhPQhUzcu-oYapTqoxWvli-5pT1f1OTVl80vPE8c"
	if got != want {
		t.Fatalf("v1 vector drifted:\n got %s\nwant %s", got, want)
	}
}

func TestTheV2VectorTheServerPins(t *testing.T) {
	got := SignV2(vectorSecret, vectorUser, vectorRoom, vectorNonce, time.UnixMilli(vectorExpiry), []string{CapSpeak})
	want := "v2.dXNlci1hYmN8cm9vbS14eXp8MTc4ODAwMDAwMDAwMHxub25jZS0xfHNwZWFr.Sl_XerGqvdjvr6PFkUIBTtaf_zBFWetug06e8elPPzk"
	if got != want {
		t.Fatalf("v2 vector drifted:\n got %s\nwant %s", got, want)
	}
}

// The empty capability list is its own vector. It is the shape a denied member
// actually gets, and the trailing separator is easy to drop on one side.
func TestTheV2NoCapabilitiesVectorTheServerPins(t *testing.T) {
	got := SignV2(vectorSecret, vectorUser, vectorRoom, vectorNonce, time.UnixMilli(vectorExpiry), nil)
	want := "v2.dXNlci1hYmN8cm9vbS14eXp8MTc4ODAwMDAwMDAwMHxub25jZS0xfA.caTW8CLQDzyjZJbUzMtPb_OAsKzfH6lPOO2G9kBEeWE"
	if got != want {
		t.Fatalf("v2 empty-capability vector drifted:\n got %s\nwant %s", got, want)
	}
}

// ── Video (GRYT-1417) ────────────────────────────────────────────────

// An old server's v2 token has no marker, and a v1 token has nothing at all. Both keep the
// video they had, or a new SFU would turn off every camera behind a server that lagged.
func TestATokenWithoutTheMarkerMayShareVideo(t *testing.T) {
	v2 := SignV2(secret, user, room, "n", time.Now().Add(time.Minute), []string{CapSpeak})
	for name, tok := range map[string]string{"v1": validToken(t), "v2": v2} {
		claims, err := Verify(secret, tok, room, user, time.Now())
		if err != nil {
			t.Fatalf("%s: verify = %v, want nil", name, err)
		}
		if !claims.MayShare(CapShareVideo) || !claims.MayShare(CapShareScreen) {
			t.Fatalf("%s: a token without %q must allow video, got %v", name, CapVideoChecked, claims.Capabilities)
		}
	}
}

func TestTheMarkerMakesAMissingVideoCapabilityADenial(t *testing.T) {
	tok := SignV2(secret, user, room, "n", time.Now().Add(time.Minute), []string{CapSpeak, CapVideoChecked, CapShareScreen})
	claims, err := Verify(secret, tok, room, user, time.Now())
	if err != nil {
		t.Fatalf("verify = %v, want nil", err)
	}
	if claims.MayShare(CapShareVideo) {
		t.Fatalf("want %q withheld, got %v", CapShareVideo, claims.Capabilities)
	}
	if !claims.MayShare(CapShareScreen) {
		t.Fatalf("want %q granted, got %v", CapShareScreen, claims.Capabilities)
	}
}

// What a member allowed everything gets from a GRYT-1417 server, in the server's order.
func TestTheV2VideoVectorTheServerPins(t *testing.T) {
	caps := []string{CapSpeak, CapVideoChecked, CapShareVideo, CapShareScreen}
	got := SignV2(vectorSecret, vectorUser, vectorRoom, vectorNonce, time.UnixMilli(vectorExpiry), caps)
	want := "v2.dXNlci1hYmN8cm9vbS14eXp8MTc4ODAwMDAwMDAwMHxub25jZS0xfHNwZWFrLHZpZGVvX2NoZWNrZWQsc2hhcmVfdmlkZW8sc2hhcmVfc2NyZWVu.Qx-j1KxaPI1WbjnTkvxx3duSBx4WMK3Jg03pQIZZZ2g"
	if got != want {
		t.Fatalf("v2 video vector drifted:\n got %s\nwant %s", got, want)
	}
}
