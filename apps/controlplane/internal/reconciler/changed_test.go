package reconciler

import (
	"log/slog"
	"testing"
	"time"
)

func TestWakeBroadcastsChangeGenerations(t *testing.T) {
	r := New(nil, Timings{}, slog.Default())
	first := r.Changed()
	r.Wake()

	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("Wake did not close the current change channel")
	}

	second := r.Changed()
	if first == second {
		t.Fatal("Changed returned the closed generation")
	}
	r.Wake()
	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("second Wake did not close the replacement channel")
	}
}
