package audiotransport

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWebSocketFallbackForwardsInARoom(t *testing.T) {
	s := &Server{tickets: newTickets(), hub: newHub()}
	srv := httptest.NewServer(http.HandlerFunc(s.ServeWebSocket))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/audio-ws?ticket="

	dial := func(peer string) *websocket.Conn {
		ticket, err := s.tickets.issue(peer, "room", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		c, _, err := websocket.DefaultDialer.Dial(url+ticket, nil)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	a, b := dial("a"), dial("b")
	defer a.Close()
	defer b.Close()
	for len(s.hub.senders("room")) < 2 {
		time.Sleep(5 * time.Millisecond)
	}

	packet := append([]byte{packetVersion, 0, 0, 1, 0, 0, 0, 0}, []byte("opus")...)
	if err := a.WriteMessage(websocket.BinaryMessage, packet); err != nil {
		t.Fatal(err)
	}
	_ = b.SetReadDeadline(time.Now().Add(2 * time.Second))
	kind, got, err := b.ReadMessage()
	if err != nil || kind != websocket.BinaryMessage {
		t.Fatalf("b heard nothing: %v", err)
	}
	if len(got) != senderBytes+len(packet) || string(got[senderBytes:]) != string(packet) {
		t.Fatalf("b got %x, want the sender id then %x", got, packet)
	}

	// A used or made-up ticket gets nowhere.
	if _, resp, err := websocket.DefaultDialer.Dial(url+"nope", nil); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a bad ticket was let in: %v", err)
	}
}
