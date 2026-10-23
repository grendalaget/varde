package room

import (
	"testing"

	"github.com/pion/webrtc/v4"
)

// A block in a group call: the blocker stops getting the blocked person's media, and nobody
// else's changes (GRYT-1477).
func TestHiddenSendersAreTheBlockedPeopleInTheRoom(t *testing.T) {
	m := NewManager(false)
	roomID, _ := addRoom(m, "group", "c-alice", "c-bob", "c-mallory")
	room, _ := m.GetRoom(roomID)

	pcs := map[string]*webrtc.PeerConnection{}
	for clientID, userID := range map[string]string{"c-alice": "alice", "c-bob": "bob", "c-mallory": "mallory"} {
		pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			t.Fatalf("peer connection: %v", err)
		}
		t.Cleanup(func() { _ = pc.Close() })
		pcs[userID] = pc
		room.PeerConnections[clientID] = pc
		room.UserIDs[clientID] = userID
	}

	if got := m.HiddenSendersFor(roomID, "c-alice"); len(got) != 0 {
		t.Fatalf("before any block Alice hides %d senders", len(got))
	}

	// Somebody not in the room is on the list too: the server sends the whole block list.
	if err := m.SetHiddenPeers(roomID, "alice", []string{"mallory", "elsewhere"}); err != nil {
		t.Fatalf("SetHiddenPeers: %v", err)
	}
	got := m.HiddenSendersFor(roomID, "c-alice")
	if len(got) != 1 || !got[pcs["mallory"]] {
		t.Fatalf("Alice hides %v, want Mallory's connection alone", got)
	}
	if len(m.HiddenSendersFor(roomID, "c-mallory")) != 0 || len(m.HiddenSendersFor(roomID, "c-bob")) != 0 {
		t.Fatal("a block one way hid media from somebody else")
	}

	// A reconnect is a new client id for the same user, and the list still applies.
	delete(room.PeerConnections, "c-alice")
	delete(room.UserIDs, "c-alice")
	room.PeerConnections["c-alice-2"] = pcs["alice"]
	room.UserIDs["c-alice-2"] = "alice"
	if got := m.HiddenSendersFor(roomID, "c-alice-2"); !got[pcs["mallory"]] {
		t.Fatal("the list was lost when Alice reconnected")
	}

	// Unblocking sends an empty list.
	if err := m.SetHiddenPeers(roomID, "alice", nil); err != nil {
		t.Fatalf("SetHiddenPeers: %v", err)
	}
	if got := m.HiddenSendersFor(roomID, "c-alice-2"); len(got) != 0 {
		t.Fatalf("after unblocking Alice still hides %d senders", len(got))
	}
}

func TestHiddenPeersForAMissingRoomIsAnError(t *testing.T) {
	m := NewManager(false)
	if err := m.SetHiddenPeers("nope", "alice", []string{"mallory"}); err == nil {
		t.Fatal("no error for a room that does not exist")
	}
	if got := m.HiddenSendersFor("nope", "c-alice"); got != nil {
		t.Fatalf("a missing room hides %v", got)
	}
}
