package auth

import (
	"testing"
	"time"
)

func TestSetWakesAWaitingSnapshot(t *testing.T) {
	live := NewLive(Claims{Capabilities: []string{CapSpeak}})
	claims, changed := live.Snapshot()
	if !claims.Can(CapSpeak) {
		t.Fatal("the token's claims were not the starting point")
	}

	go live.Set(Claims{})
	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("Set did not wake the channel an earlier Snapshot handed out")
	}
	if claims, _ := live.Snapshot(); claims.Can(CapSpeak) {
		t.Fatal("the new claims did not replace the old")
	}
}

// Each Set closes one channel and hands out a fresh one, so a second change wakes again.
func TestEverySetWakesAgain(t *testing.T) {
	live := NewLive(Claims{})
	for i := 0; i < 3; i++ {
		_, changed := live.Snapshot()
		live.Set(Claims{})
		select {
		case <-changed:
		default:
			t.Fatalf("change %d did not wake its snapshot", i+1)
		}
	}
}
