package websocket

import (
	"encoding/json"
	"testing"

	"sfu-v2/internal/config"
	"sfu-v2/internal/room"
	"sfu-v2/pkg/types"
)

// Only the server that owns the room may say whose media somebody doesn't get.
func TestHiddenPeersNeedTheServersCredentials(t *testing.T) {
	rooms := room.NewManager(false)
	if err := rooms.RegisterServer("srv", "secret", "srv_group"); err != nil {
		t.Fatalf("register: %v", err)
	}
	h := &Handler{config: &config.Config{}, roomManager: rooms, coordinator: quietCoordinator{}}

	send := func(password string) {
		data, _ := json.Marshal(types.HiddenPeersData{
			RoomID: "srv_group", UserID: "alice", ServerID: "srv", ServerPassword: password, Hidden: []string{"mallory"},
		})
		if err := h.handleUserHiddenPeers(string(data)); err != nil {
			t.Fatalf("handleUserHiddenPeers: %v", err)
		}
	}

	send("wrong")
	if r, _ := rooms.GetRoom("srv_group"); len(r.HiddenPeers["alice"]) != 0 {
		t.Fatal("a wrong password set the list")
	}
	send("secret")
	if r, _ := rooms.GetRoom("srv_group"); !r.HiddenPeers["alice"]["mallory"] {
		t.Fatal("the owning server could not set the list")
	}
}
