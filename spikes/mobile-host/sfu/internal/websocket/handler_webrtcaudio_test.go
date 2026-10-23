package websocket

import (
	"encoding/json"
	"testing"

	"github.com/pion/webrtc/v4"

	"sfu-v2/internal/config"
	"sfu-v2/internal/room"
	"sfu-v2/pkg/types"
)

type lastWanted struct{ wanted *bool }

func (w *lastWanted) WriteJSON(v interface{}) error {
	msg, ok := v.(*types.WebSocketMessage)
	if !ok || msg.Event != types.EventWebRTCAudio {
		return nil
	}
	var d types.WebRTCAudioData
	if err := json.Unmarshal([]byte(msg.Data), &d); err != nil {
		return err
	}
	w.wanted = &d.Wanted
	return nil
}

// GRYT-1853: WebRTC audio is wanted while anyone in the room has no transport session.
func TestWebRTCAudioWantedUntilEveryoneIsOnTheTransport(t *testing.T) {
	rooms := room.NewManager(false)
	if err := rooms.RegisterServer("srv", "secret", "srv_room"); err != nil {
		t.Fatalf("register: %v", err)
	}
	alice, bob := &lastWanted{}, &lastWanted{}
	for id, w := range map[string]*lastWanted{"a": alice, "b": bob} {
		pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			t.Fatalf("peer connection: %v", err)
		}
		defer pc.Close()
		if err := rooms.AddPeerToRoom("srv_room", id, id, pc, w); err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
	}
	h := &Handler{config: &config.Config{}, roomManager: rooms, coordinator: quietCoordinator{}}

	h.sendWebRTCAudioWanted("srv_room", map[string]uint32{"a": 1})
	if alice.wanted == nil || !*alice.wanted || bob.wanted == nil || !*bob.wanted {
		t.Fatal("bob has no session, so everyone still has to send WebRTC audio")
	}
	h.sendWebRTCAudioWanted("srv_room", map[string]uint32{"a": 1, "b": 2})
	if *alice.wanted || *bob.wanted {
		t.Fatal("everyone is on the transport, so nobody needs WebRTC audio")
	}
}
