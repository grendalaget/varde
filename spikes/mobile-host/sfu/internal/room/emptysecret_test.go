package room

import (
	"strings"
	"testing"
	"time"

	"sfu-v2/internal/auth"
)

// GRYT-786. SERVER_PASSWORD defaulted to empty and is the HMAC key tokens are verified
// against, so an ordinary deployment verified against a key anybody can guess.
func TestRegisterServerRefusesAnEmptySecret(t *testing.T) {
	m := NewManager(false)

	if err := m.RegisterServer("srv-1", "", "room-42"); err == nil {
		t.Fatal("registration with an empty secret was accepted")
	}

	// And nothing was remembered, so a later join cannot find an entry to
	// verify against.
	if _, exists := m.registeredServers["srv-1"]; exists {
		t.Fatal("an empty secret was stored despite being refused")
	}
}

func TestForgedTokenIsRefusedWhenTheSecretIsEmpty(t *testing.T) {
	m := NewManager(false)

	// The attack this closes, walked end to end. Before the fix this entered
	// room-42 as somebody else, holding speak.
	_ = m.RegisterServer("srv-1", "", "room-42")
	forged := auth.SignV2("", "somebody-elses-user", "room-42", "n", time.Now().Add(time.Minute), []string{auth.CapSpeak})

	if _, err := m.ValidateClientJoin("room-42", "srv-1", "", forged, "somebody-elses-user"); err == nil {
		t.Fatal("a token signed with an empty secret was accepted")
	}
}

func TestARealSecretStillWorks(t *testing.T) {
	m := NewManager(false)
	const secret = "a-real-secret-that-nobody-can-guess"

	if err := m.RegisterServer("srv-1", secret, "room-42"); err != nil {
		t.Fatalf("registration with a real secret refused: %v", err)
	}

	token := auth.SignV2(secret, "user-1", "room-42", "n", time.Now().Add(time.Minute), []string{auth.CapSpeak})
	claims, err := m.ValidateClientJoin("room-42", "srv-1", secret, token, "user-1")
	if err != nil {
		t.Fatalf("a legitimate join was refused: %v", err)
	}
	if !claims.Can(auth.CapSpeak) {
		t.Fatal("speak was not carried through")
	}
}

func TestAChangedSecretSaysToRestartTheSFU(t *testing.T) {
	// The message matters more than it looks. Nothing removes a registration, so a server
	// that rotated its key cannot get back in until this process restarts.
	m := NewManager(false)
	_ = m.RegisterServer("srv-1", "first-secret", "room-42")

	err := m.RegisterServer("srv-1", "second-secret", "room-42")
	if err == nil {
		t.Fatal("a changed secret was accepted, which would let anybody replace the key")
	}
	if !strings.Contains(err.Error(), "restart") {
		t.Fatalf("the error does not say what to do about it: %v", err)
	}
}
