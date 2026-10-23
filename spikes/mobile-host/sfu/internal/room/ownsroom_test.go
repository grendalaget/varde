package room

import "testing"

// user_capabilities changes what a member may send, so only the server that registered the
// room may send it: two servers can share one SFU.
func TestOnlyTheServerThatRegisteredARoomOwnsIt(t *testing.T) {
	m := NewManager(false)
	if err := m.RegisterServer("srv-a", "secret-a", "srv-a_room"); err != nil {
		t.Fatalf("register a: %v", err)
	}
	if err := m.RegisterServer("srv-b", "secret-b", "srv-b_room"); err != nil {
		t.Fatalf("register b: %v", err)
	}

	cases := []struct {
		name, server, password, room string
		want                         bool
	}{
		{"its own room", "srv-a", "secret-a", "srv-a_room", true},
		{"wrong password", "srv-a", "secret-b", "srv-a_room", false},
		{"another server's room", "srv-a", "secret-a", "srv-b_room", false},
		{"no such room", "srv-a", "secret-a", "nowhere", false},
		{"unregistered server", "srv-c", "", "srv-a_room", false},
	}
	for _, c := range cases {
		if got := m.ServerOwnsRoom(c.server, c.password, c.room); got != c.want {
			t.Errorf("%s: ServerOwnsRoom = %v, want %v", c.name, got, c.want)
		}
	}
}
