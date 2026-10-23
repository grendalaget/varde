package reconciler

import (
	"log/slog"
	"testing"
	"time"
)

func TestNotifyGroupClosesOnlyItsGeneration(t *testing.T) {
	r := New(nil, Timings{}, slog.Default())
	firstA := r.Changed("a")
	firstB := r.Changed("b")
	r.Wake()

	select {
	case <-firstA:
		t.Fatal("Wake closed group a's change channel")
	case <-firstB:
		t.Fatal("Wake closed group b's change channel")
	default:
	}

	r.NotifyGroup("a")
	select {
	case <-firstA:
	case <-time.After(time.Second):
		t.Fatal("NotifyGroup did not close group a's current channel")
	}
	select {
	case <-firstB:
		t.Fatal("NotifyGroup for a closed group b's channel")
	default:
	}

	secondA := r.Changed("a")
	if firstA == secondA {
		t.Fatal("Changed returned group a's closed generation")
	}
	r.NotifyGroup("a")
	select {
	case <-secondA:
	case <-time.After(time.Second):
		t.Fatal("NotifyGroup did not close group a's fresh channel")
	}
	select {
	case <-firstB:
		t.Fatal("group b's channel was closed")
	default:
	}
}
