package room

import (
	"strings"
	"testing"
	"time"

	"sfu-v2/internal/auth"
)

const (
	testServer   = "server-1"
	testPassword = "server-1-shared-secret"
	testRoom     = "room-1"
	testUser     = "user-1"
)

func managerWithServer(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(false)
	if err := m.RegisterServer(testServer, testPassword, testRoom); err != nil {
		t.Fatalf("RegisterServer: %v", err)
	}
	return m
}

func signed(userID, roomID string) string {
	return auth.Sign(testPassword, userID, roomID, "n", time.Now().Add(time.Minute))
}

func TestJoinWithASignedTokenIsAccepted(t *testing.T) {
	m := managerWithServer(t)
	if _, err := m.ValidateClientJoin(testRoom, testServer, "", signed(testUser, testRoom), testUser); err != nil {
		t.Fatalf("ValidateClientJoin = %v, want nil", err)
	}
}

// The point of the change. Holding the shared password used to be enough to
// enter any room, and every browser was handed it.
func TestJoinWithAStolenPasswordAndABadTokenIsRefused(t *testing.T) {
	m := managerWithServer(t)
	forged := auth.Sign("not-the-secret", testUser, testRoom, "n", time.Now().Add(time.Minute))

	_, err := m.ValidateClientJoin(testRoom, testServer, testPassword, forged, testUser)
	if err == nil {
		t.Fatal("ValidateClientJoin = nil, want a refusal: a presented token must be verified, not fallen back on")
	}
	if !strings.Contains(err.Error(), "client token rejected") {
		t.Fatalf("ValidateClientJoin = %v, want the token rejection", err)
	}
}

// A token for one room must not open another, even signed by the right server.
func TestATokenForAnotherRoomDoesNotOpenThisOne(t *testing.T) {
	m := managerWithServer(t)
	if _, err := m.ValidateClientJoin(testRoom, testServer, "", signed(testUser, "some-other-room"), testUser); err == nil {
		t.Fatal("ValidateClientJoin = nil, want a refusal for a token minted for a different room")
	}
}

// Deployable before the servers that mint tokens; removable once none rely on it.
func TestJoinWithoutATokenStillFallsBackToThePassword(t *testing.T) {
	m := managerWithServer(t)
	if _, err := m.ValidateClientJoin(testRoom, testServer, testPassword, "", testUser); err != nil {
		t.Fatalf("ValidateClientJoin = %v, want nil for the legacy path", err)
	}
}

func TestJoinWithNeitherIsRefused(t *testing.T) {
	m := managerWithServer(t)
	if _, err := m.ValidateClientJoin(testRoom, testServer, "wrong-password", "", testUser); err == nil {
		t.Fatal("ValidateClientJoin = nil, want a refusal when neither a token nor the password is valid")
	}
}

func TestJoinAgainstAnUnregisteredServerIsRefused(t *testing.T) {
	m := managerWithServer(t)
	if _, err := m.ValidateClientJoin(testRoom, "server-nobody-registered", "", signed(testUser, testRoom), testUser); err == nil {
		t.Fatal("ValidateClientJoin = nil, want a refusal for an unregistered server")
	}
}

// With the flag on, knowing the old shared password buys nothing. Until it is on, it still
// does — which is why the flag exists rather than a comment saying to remove the fallback.
func TestRequireClientTokenClosesTheLegacyPath(t *testing.T) {
	m := managerWithServer(t)
	m.SetRequireClientToken(true)

	if _, err := m.ValidateClientJoin(testRoom, testServer, testPassword, "", testUser); err == nil {
		t.Fatal("ValidateClientJoin = nil, want a refusal once client tokens are required")
	}
	if _, err := m.ValidateClientJoin(testRoom, testServer, "", signed(testUser, testRoom), testUser); err != nil {
		t.Fatalf("ValidateClientJoin = %v, want nil: a signed token must still work", err)
	}
}

// ── What the join says the client may do ─────────────────────────────

func TestAV1TokenJoinMaySpeak(t *testing.T) {
	m := managerWithServer(t)
	claims, err := m.ValidateClientJoin(testRoom, testServer, "", signed(testUser, testRoom), testUser)
	if err != nil {
		t.Fatalf("ValidateClientJoin = %v, want nil", err)
	}
	if !claims.Can(auth.CapSpeak) {
		t.Fatalf("a v1 token must still grant %q, got %v", auth.CapSpeak, claims.Capabilities)
	}
}

func TestAV2TokenWithoutSpeakSaysSo(t *testing.T) {
	m := managerWithServer(t)
	tok := auth.SignV2(testPassword, testUser, testRoom, "n", time.Now().Add(time.Minute), nil)
	claims, err := m.ValidateClientJoin(testRoom, testServer, "", tok, testUser)
	if err != nil {
		t.Fatalf("ValidateClientJoin = %v, want nil", err)
	}
	if claims.Can(auth.CapSpeak) {
		t.Fatalf("want %q withheld, got %v", auth.CapSpeak, claims.Capabilities)
	}
}

// The deprecated password path. A server old enough to use it has no `speak` permission to
// express, so refusing the microphone there would break working deployments.
func TestThePasswordPathMaySpeak(t *testing.T) {
	m := managerWithServer(t)
	claims, err := m.ValidateClientJoin(testRoom, testServer, testPassword, "", testUser)
	if err != nil {
		t.Fatalf("ValidateClientJoin = %v, want nil", err)
	}
	if !claims.Can(auth.CapSpeak) {
		t.Fatalf("the password path must grant %q, got %v", auth.CapSpeak, claims.Capabilities)
	}
}

// A rejected join hands back nothing, so a caller that ignores the error cannot
// accidentally read a capability out of it.
func TestARejectedJoinGrantsNothing(t *testing.T) {
	m := managerWithServer(t)
	claims, err := m.ValidateClientJoin(testRoom, testServer, "wrong-password", "", testUser)
	if err == nil {
		t.Fatal("ValidateClientJoin = nil, want an error")
	}
	if claims.Can(auth.CapSpeak) {
		t.Fatalf("a refused join granted %q", auth.CapSpeak)
	}
}
