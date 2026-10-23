package websocket

import (
	"errors"
	"fmt"
	"testing"
	"time"

	gorilla "github.com/gorilla/websocket"
)

// The whole point of the change these cover: a peer can tell what happened. Before, every
// one of these was 1006 with no reason, which is what a snapped connection looks like.

func TestCloseWithReasonSendsACloseFrame(t *testing.T) {
	server, client := newTestSocketPair(t)

	go func() {
		_ = server.CloseWithReason(gorilla.CloseTryAgainLater, "server full")
	}()

	if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}

	_, _, err := client.ReadMessage()
	if err == nil {
		t.Fatal("expected the read to end with a close error")
	}

	var closeErr *gorilla.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("read error is %T (%v), want *websocket.CloseError", err, err)
	}
	if closeErr.Code == gorilla.CloseAbnormalClosure {
		t.Fatal("close code is 1006 — no frame arrived, which is the bug this is here for")
	}
	if closeErr.Code != gorilla.CloseTryAgainLater {
		t.Fatalf("close code = %d, want %d", closeErr.Code, gorilla.CloseTryAgainLater)
	}
	if closeErr.Text != "server full" {
		t.Fatalf("close reason = %q, want %q", closeErr.Text, "server full")
	}
}

// A close frame's payload is capped at 125 bytes, two of them the status code. Gorilla
// rejects a longer one rather than truncating, so the peer would land back on 1006.
func TestCloseWithReasonTruncatesAnOverlongReason(t *testing.T) {
	server, client := newTestSocketPair(t)

	long := ""
	for len(long) < 400 {
		long += "reason-"
	}

	go func() {
		_ = server.CloseWithReason(gorilla.CloseInternalServerErr, long)
	}()

	if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}

	_, _, err := client.ReadMessage()

	var closeErr *gorilla.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("read error is %T (%v), want *websocket.CloseError", err, err)
	}
	if closeErr.Code != gorilla.CloseInternalServerErr {
		t.Fatalf("close code = %d, want %d", closeErr.Code, gorilla.CloseInternalServerErr)
	}
	if len(closeErr.Text) != maxCloseReason {
		t.Fatalf("reason is %d bytes, want it truncated to %d", len(closeErr.Text), maxCloseReason)
	}
}

func TestCloseStatusForMapsTheCasesApart(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantCode   int
		wantReason string
	}{
		{
			name:       "a connection that ended on its own",
			err:        nil,
			wantCode:   gorilla.CloseNormalClosure,
			wantReason: "",
		},
		{
			// Wrapped, because handler_join.go adds the counts to it.
			name:       "capacity, however it was wrapped",
			err:        fmt.Errorf("%w: 200/200", ErrServerFull),
			wantCode:   gorilla.CloseTryAgainLater,
			wantReason: "server full",
		},
		{
			name:       "anything else",
			err:        errors.New("read tcp: connection reset by peer"),
			wantCode:   gorilla.CloseInternalServerErr,
			wantReason: "sfu error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, reason := closeStatusFor(tc.err)
			if code != tc.wantCode {
				t.Errorf("code = %d, want %d", code, tc.wantCode)
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
			if code == gorilla.CloseAbnormalClosure {
				t.Error("1006 is never something to send — it is what the peer sees when nothing was sent")
			}
		})
	}
}

// The error text is deliberately not passed through: it carries token and
// validation detail, and it travels to whoever is connected.
func TestCloseStatusForDoesNotLeakTheErrorText(t *testing.T) {
	_, reason := closeStatusFor(errors.New("Join validation failed: bad signature for user 4f2c"))
	if reason != "sfu error" {
		t.Fatalf("reason = %q, want it to say nothing about the error", reason)
	}
}
