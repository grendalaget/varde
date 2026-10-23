package audiotransport

import (
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

// The fallback for clients that can't use WebTransport, or whose network blocks UDP (GRYT-1831):
// the same packets as binary WebSocket messages, through the same ticket and hub.

// wsQueue is how many packets may wait for a slow connection: 50 ms of one sender. Past it they're
// dropped, because late audio is useless and TCP would otherwise queue it for seconds.
const wsQueue = 20

var wsUpgrader = websocket.Upgrader{
	// The ticket is the credential, and it only came over the authenticated signalling socket.
	CheckOrigin:     func(*http.Request) bool { return true },
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

type wsSender struct{ out chan []byte }

// SendDatagram queues a packet for the writer, or drops it when the connection is behind.
func (w *wsSender) SendDatagram(b []byte) error {
	select {
	case w.out <- append([]byte(nil), b...):
	default:
	}
	return nil
}

// ServeWebSocket is the WebSocket fallback at /audio-ws?ticket=…, on the SFU's public port.
func (s *Server) ServeWebSocket(w http.ResponseWriter, r *http.Request) {
	t, ok := s.tickets.redeem(r.URL.Query().Get("ticket"), time.Now())
	if !ok {
		http.Error(w, "no such ticket", http.StatusUnauthorized)
		return
	}
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(senderBytes + headerBytes + maxOpusBytes)
	sender := &wsSender{out: make(chan []byte, wsQueue)}
	done := make(chan struct{})
	m := s.hub.join(t.roomID, t.peerID, sender, func() { _ = conn.Close() })
	defer s.hub.leave(m)
	go func() {
		for {
			select {
			case b := <-sender.out:
				_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
				if conn.WriteMessage(websocket.BinaryMessage, b) != nil {
					_ = conn.Close()
					return
				}
			case <-done:
				return
			}
		}
	}()
	defer close(done)
	for {
		kind, b, err := conn.ReadMessage()
		if err != nil {
			log.Printf("[AUDIO] websocket session %s ended: %v", t.peerID, err)
			return
		}
		if kind == websocket.BinaryMessage {
			s.hub.forward(m, b, time.Now())
		}
	}
}
