package websocket

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// How long a close frame gets to reach the socket. Short on purpose: all this deadline does
// is stop a dead TCP connection holding the handler open.
const closeWriteWait = 2 * time.Second

// The most a close frame can say. The payload is capped at 125 bytes and the
// first two are the status code.
const maxCloseReason = 123

// ThreadSafeWriter wraps a WebSocket connection with a mutex to ensure safe concurrent access
type ThreadSafeWriter struct {
	*websocket.Conn
	sync.Mutex

	// How long the peer may say nothing before its read deadline fires; zero means none.
	// Not guarded by the mutex: written once before the read loop, read on that goroutine.
	readTimeout time.Duration
}

// ReadMessage shadows the embedded gorilla method so every message pushes the read deadline
// out. A shadow rather than a new name, because three read loops already call it.
func (t *ThreadSafeWriter) ReadMessage() (int, []byte, error) {
	messageType, payload, err := t.Conn.ReadMessage()
	if err == nil {
		t.extendReadDeadline()
	}
	return messageType, payload, err
}

// armReadDeadline sets the timeout and starts the clock.
func (t *ThreadSafeWriter) armReadDeadline(timeout time.Duration) {
	t.readTimeout = timeout
	t.extendReadDeadline()
}

// extendReadDeadline gives the peer another full timeout to say something.
func (t *ThreadSafeWriter) extendReadDeadline() {
	if t.readTimeout <= 0 {
		return
	}

	_ = t.Conn.SetReadDeadline(time.Now().Add(t.readTimeout))
}

// WriteJSON writes a JSON message to the WebSocket connection in a thread-safe manner
func (t *ThreadSafeWriter) WriteJSON(v interface{}) error {
	t.Lock()
	defer t.Unlock()
	return t.Conn.WriteJSON(v)
}

// CloseWithReason says why before hanging up: a bare Close gives every peer 1006, which is
// also what a snapped cable looks like. Best-effort — a gone peer still gets closed.
func (t *ThreadSafeWriter) CloseWithReason(code int, reason string) error {
	t.Lock()
	defer t.Unlock()

	if len(reason) > maxCloseReason {
		reason = reason[:maxCloseReason]
	}

	_ = t.Conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason),
		time.Now().Add(closeWriteWait),
	)

	return t.Conn.Close()
}

// NewThreadSafeWriter creates a new thread-safe WebSocket writer
func NewThreadSafeWriter(conn *websocket.Conn) *ThreadSafeWriter {
	return &ThreadSafeWriter{
		Conn:  conn,
		Mutex: sync.Mutex{},
	}
}
