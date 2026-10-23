package websocket

import (
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	gorilla "github.com/gorilla/websocket"
)

// What these cover: before this, a socket whose other end had gone without a FIN read
// forever, and nothing travelled server to client while a call was quiet.

func TestKeepAlivePingsAQuietConnection(t *testing.T) {
	server, client := newTestSocketPair(t)

	pings := make(chan struct{}, 4)
	client.SetPingHandler(func(string) error {
		select {
		case pings <- struct{}{}:
		default:
		}
		return nil
	})

	k := StartKeepAlive(server, 50*time.Millisecond, 5*time.Second)
	defer k.Stop()

	// Control frames are only surfaced while a read is in progress, so the client has to be
	// reading for its ping handler to run. This read never completes, which is the case.
	go func() {
		_, _, _ = client.ReadMessage()
	}()

	select {
	case <-pings:
	case <-time.After(3 * time.Second):
		t.Fatal("no ping arrived — a quiet connection sends nothing server to client, which is half the bug")
	}
}

func TestReadDeadlineFiresWhenNothingComesBack(t *testing.T) {
	server, client := newTestSocketPair(t)

	// A peer that is there but does not answer. Gorilla's default ping handler replies with
	// a pong, so it has to be replaced to get a socket that is open and silent.
	client.SetPingHandler(func(string) error { return nil })

	k := StartKeepAlive(server, 50*time.Millisecond, 200*time.Millisecond)
	defer k.Stop()

	done := make(chan error, 1)
	go func() {
		_, _, err := server.ReadMessage()
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("read returned no error — the deadline never fired")
		}
		if !isReadDeadline(err) {
			t.Fatalf("read error is %v, want a deadline", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the read never ended — this is the goroutine that used to be held open forever")
	}
}

func TestAnyMessageKeepsTheDeadlineAway(t *testing.T) {
	server, client := newTestSocketPair(t)

	// Same silent peer as above, except this one is sending. It answers no
	// pings, so only the message traffic can be holding the deadline off.
	client.SetPingHandler(func(string) error { return nil })

	k := StartKeepAlive(server, 50*time.Millisecond, 300*time.Millisecond)
	defer k.Stop()

	stop := make(chan struct{})
	defer close(stop)

	go func() {
		ticker := time.NewTicker(75 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if err := client.WriteMessage(gorilla.TextMessage, []byte("{}")); err != nil {
					return
				}
			}
		}
	}()

	// Well past the 300ms deadline, so a read that only extended on pong would
	// have given up several times over by now.
	deadline := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, _, err := server.ReadMessage(); err != nil {
			t.Fatalf("read failed while the peer was actively sending: %v", err)
		}
	}
}

func TestKeepAliveOffLeavesNoDeadline(t *testing.T) {
	server, client := newTestSocketPair(t)
	client.SetPingHandler(func(string) error { return nil })

	k := StartKeepAlive(server, 0, 200*time.Millisecond)
	defer k.Stop()

	if server.readTimeout != 0 {
		t.Fatalf("readTimeout = %s, want it left unset — an interval of 0 is the switch that turns all of this off", server.readTimeout)
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := server.ReadMessage()
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("the read ended (%v) — with liveness off it should have kept waiting", err)
	case <-time.After(600 * time.Millisecond):
	}
}

// Stop is called from the connection handler's defer, right before the close.
// It must not sit waiting for a ping to a dead socket to finish.
func TestStopDoesNotWaitOnThePinger(t *testing.T) {
	server, _ := newTestSocketPair(t)

	k := StartKeepAlive(server, 50*time.Millisecond, 5*time.Second)

	returned := make(chan struct{})
	go func() {
		k.Stop()
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("Stop blocked")
	}
}

func TestStopIsSafeWhenLivenessIsOff(t *testing.T) {
	server, _ := newTestSocketPair(t)

	// Both of these have to be no-ops rather than a nil dereference: the
	// handler defers Stop without knowing whether pinging was switched on.
	StartKeepAlive(server, 0, 0).Stop()
	(*KeepAlive)(nil).Stop()
}

func TestIsReadDeadlineTellsTimeoutsApartFromEverythingElse(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nothing went wrong", nil, false},
		{"the deadline", os.ErrDeadlineExceeded, true},
		{"the deadline, wrapped", fmt.Errorf("read client message: %w", os.ErrDeadlineExceeded), true},
		{"a timeout that does not wrap the sentinel", &timeoutError{}, true},
		{"the peer closed", &gorilla.CloseError{Code: gorilla.CloseNormalClosure}, false},
		{"the connection broke", errors.New("read tcp: connection reset by peer"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isReadDeadline(tc.err); got != tc.want {
				t.Errorf("isReadDeadline(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// A peer given up on gets its own close code, so the SFU's log can tell that
// apart from a bug on this side.
func TestCloseStatusForSaysPingTimeout(t *testing.T) {
	code, reason := closeStatusFor(fmt.Errorf("%w after 1m30s", ErrPeerUnresponsive))

	if code != closeCodePingTimeout {
		t.Errorf("code = %d, want %d", code, closeCodePingTimeout)
	}
	if reason != "ping timeout" {
		t.Errorf("reason = %q, want %q", reason, "ping timeout")
	}
	if code == gorilla.CloseInternalServerErr {
		t.Error("reported as an SFU error, which is what this case exists to stop being")
	}
}

type timeoutError struct{}

func (*timeoutError) Error() string   { return "i/o timeout" }
func (*timeoutError) Timeout() bool   { return true }
func (*timeoutError) Temporary() bool { return true }

var _ net.Error = (*timeoutError)(nil)
