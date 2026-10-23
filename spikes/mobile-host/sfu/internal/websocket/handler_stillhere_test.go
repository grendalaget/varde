package websocket

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	gorilla "github.com/gorilla/websocket"

	"sfu-v2/internal/config"
	"sfu-v2/internal/room"
	"sfu-v2/pkg/types"
)

/*
still_here from the wire to the clock (GRYT-715): that a frame a client sends reaches it,
keyed by the same room id the sweep iterates. Getting it wrong is silent.
*/

const stillHereServerID = "gryt-wire-test"

// A call room with one peer in it, and the socket the SFU would hang up on.
func callRoomWithOnePeer(t *testing.T, m *room.Manager) (string, *gorilla.Conn) {
	t.Helper()

	roomID := stillHereServerID + "_dm_" + strings.Repeat("a1b2", 8)
	if err := m.RegisterServer(stillHereServerID, "pw", roomID); err != nil {
		t.Fatalf("register server: %v", err)
	}

	victimServer, victimClient := newTestSocketPair(t)
	if err := m.AddPeerToRoom(roomID, "lonely", "user_lonely", newTestPeer(t), victimServer); err != nil {
		t.Fatalf("add peer: %v", err)
	}
	return roomID, victimClient
}

// hungUpOn reports whether the SFU closed the peer's socket: a close comes back as an error,
// a live call as the read deadline expiring. Treating any error as a hang-up passes always.
func hungUpOn(conn *gorilla.Conn, within time.Duration) bool {
	_ = conn.SetReadDeadline(time.Now().Add(within))
	_, _, err := conn.ReadMessage()
	if err == nil {
		return false
	}

	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return false
	}
	return true
}

func TestStillHereFromTheWireKeepsTheCallUp(t *testing.T) {
	const timeout = 200 * time.Millisecond

	roomManager := room.NewManager(false)
	h := &Handler{config: &config.Config{}, roomManager: roomManager}
	roomID, victimClient := callRoomWithOnePeer(t, roomManager)

	// A second socket, standing in for the peer's own message loop.
	senderServer, senderClient := newTestSocketPair(t)
	go func() { _ = h.handleClientMessages(senderServer, nil, roomID, "lonely") }()

	// The clock starts.
	roomManager.EndAbandonedCalls(timeout)
	time.Sleep(2 * timeout)

	if err := senderClient.WriteJSON(&types.WebSocketMessage{Event: types.EventStillHere}); err != nil {
		t.Fatalf("send still_here: %v", err)
	}

	// The handler runs on its own goroutine, so give the message time to land
	// before sweeping. Short next to the timeout above.
	time.Sleep(50 * time.Millisecond)

	roomManager.EndAbandonedCalls(timeout)
	if hungUpOn(victimClient, 150*time.Millisecond) {
		t.Fatal("still_here reached the SFU and the call was ended anyway — the clock did not move")
	}
}

func TestWithoutStillHereTheCallStillEnds(t *testing.T) {
	const timeout = 200 * time.Millisecond

	roomManager := room.NewManager(false)
	h := &Handler{config: &config.Config{}, roomManager: roomManager}
	roomID, victimClient := callRoomWithOnePeer(t, roomManager)

	senderServer, senderClient := newTestSocketPair(t)
	go func() { _ = h.handleClientMessages(senderServer, nil, roomID, "lonely") }()

	// Something else on the same loop, so this is the message and not the
	// socket being alive that made the difference above.
	if err := senderClient.WriteJSON(&types.WebSocketMessage{Event: types.EventKeepAlive}); err != nil {
		t.Fatalf("send keep_alive: %v", err)
	}

	roomManager.EndAbandonedCalls(timeout)
	time.Sleep(2 * timeout)
	roomManager.EndAbandonedCalls(timeout)

	if !hungUpOn(victimClient, time.Second) {
		t.Fatal("nobody said they were here, so the call should have ended")
	}
}
