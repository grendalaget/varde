package websocket

import (
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// TestOnConnectionStateChangeReplaces pins the pion behaviour GRYT-570 rests on: it is a
// setter, so a second registration silently turns the first off. A test of someone's library.
func TestOnConnectionStateChangeReplaces(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("could not create peer connection: %v", err)
	}
	defer func() { _ = pc.Close() }()

	var mu sync.Mutex
	var called []string

	pc.OnConnectionStateChange(func(webrtc.PeerConnectionState) {
		mu.Lock()
		called = append(called, "first")
		mu.Unlock()
	})
	pc.OnConnectionStateChange(func(webrtc.PeerConnectionState) {
		mu.Lock()
		called = append(called, "second")
		mu.Unlock()
	})

	// Closing takes the connection to Closed, which fires whichever handler is
	// currently registered.
	if err := pc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		got := append([]string(nil), called...)
		mu.Unlock()

		if len(got) > 0 {
			for _, who := range got {
				if who == "first" {
					t.Fatalf("the first handler ran, so pion now appends rather than replaces — "+
						"see the note on setupWebRTCHandlers; got %v", got)
				}
			}
			return
		}

		select {
		case <-deadline:
			t.Fatal("no handler ran within two seconds")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestSharedCloseReleasesEveryWaiter is the shape the fix relies on: one channel, closed
// once, releasing every track's cleanup rather than only the last registered.
func TestSharedCloseReleasesEveryWaiter(t *testing.T) {
	closed := make(chan struct{})
	var once sync.Once
	markClosed := func() { once.Do(func() { close(closed) }) }

	const tracks = 3
	var wg sync.WaitGroup
	wg.Add(tracks)
	cleaned := make(chan int, tracks)

	for i := 0; i < tracks; i++ {
		go func(n int) {
			defer wg.Done()
			<-closed
			cleaned <- n
		}(i)
	}

	// Twice, because a peer connection can report Failed and then Closed and both call this.
	// Closing a channel twice panics; sync.Once is what stops it.
	markClosed()
	markClosed()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("only %d of %d cleanups ran", len(cleaned), tracks)
	}

	if len(cleaned) != tracks {
		t.Fatalf("expected every track to be cleaned up, got %d of %d", len(cleaned), tracks)
	}
}
