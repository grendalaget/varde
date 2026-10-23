package websocket

import (
	"time"

	"github.com/gorilla/websocket"
)

// How long a ping frame gets to reach the socket. Without a deadline, a ping to a peer whose
// TCP window has stopped draining blocks this goroutine indefinitely.
const pingWriteWait = 10 * time.Second

// KeepAlive pings a connection on a timer and arms a read deadline: a peer that vanished
// without a FIN reads forever, and a quiet call has its NAT mapping reaped.
type KeepAlive struct {
	stop chan struct{}
}

// StartKeepAlive arms conn's read deadline and starts pinging every interval; Stop belongs in
// a defer beside it. An interval of zero turns the whole thing off — the escape hatch.
func StartKeepAlive(conn *ThreadSafeWriter, interval, timeout time.Duration) *KeepAlive {
	if interval <= 0 || timeout <= 0 {
		return &KeepAlive{}
	}

	// Arm before the first read rather than at the first tick: the read before any ping is
	// the join handshake, and a socket that says nothing used to hold a goroutine open.
	conn.armReadDeadline(timeout)

	// A pong is the peer saying it is still there. Gorilla calls this on the reading
	// goroutine, so touching the deadline here is like touching it after a message.
	conn.SetPongHandler(func(string) error {
		conn.extendReadDeadline()
		return nil
	})

	k := &KeepAlive{stop: make(chan struct{})}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-k.stop:
				return
			case <-ticker.C:
				// WriteControl is the one write that does not take the mutex; gorilla
				// documents it as concurrent-safe. A failed ping is dropped on purpose.
				_ = conn.WriteControl(
					websocket.PingMessage,
					nil,
					time.Now().Add(pingWriteWait),
				)
			}
		}
	}()

	return k
}

// Stop ends the pinger without waiting for the goroutine, so a ping can still be in flight.
// Waiting would hold the handler for a full interval on a socket that is already dead.
func (k *KeepAlive) Stop() {
	if k == nil || k.stop == nil {
		return
	}
	close(k.stop)
}
