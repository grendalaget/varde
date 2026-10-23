package room

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

/*
Ending a call somebody is alone in, and not ending anything else (GRYT-711). Both are the
same kind of SFU room, so the only thing separating them is the id.
*/

// A connection that records being closed. The real one is a WebSocket; what
// matters here is that closing it is what ends a call.
type fakeConn struct {
	mu     sync.Mutex
	closed bool
}

func (c *fakeConn) WriteJSON(interface{}) error { return nil }

func (c *fakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *fakeConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

const testServerID = "gryt-test"

// addRoom puts a room in the manager holding one connection per client id. The peer
// connections are nil, which nothing on this path dereferences.
func addRoom(m *Manager, channelID string, clients ...string) (string, map[string]*fakeConn) {
	roomID := testServerID + "_" + channelID
	conns := map[string]*fakeConn{}

	room := &Room{
		ID:              roomID,
		ServerID:        testServerID,
		PeerConnections: map[string]*webrtc.PeerConnection{},
		Connections:     map[string]JSONWriter{},
		UserIDs:         map[string]string{},
		DeafenedUsers:   map[string]bool{},
		CreatedAt:       time.Now(),
		LastActivity:    time.Now(),
	}
	for _, c := range clients {
		conn := &fakeConn{}
		conns[c] = conn
		room.PeerConnections[c] = nil
		room.Connections[c] = conn
		room.UserIDs[c] = "user_" + c
	}

	m.mutex.Lock()
	m.rooms[roomID] = room
	m.mutex.Unlock()

	return roomID, conns
}

const testTimeout = time.Minute

// rewind moves every clock the sweep has started back past the deadline, which is how these
// run in milliseconds. A negative timeout would not do: that is the off switch.
func rewind(m *Manager) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	for roomID, since := range m.aloneSince {
		m.aloneSince[roomID] = since.Add(-2 * testTimeout)
	}
}

// The first sweep starts the clock, the second acts on it.
func sweepPastTheDeadline(m *Manager) {
	m.EndAbandonedCalls(testTimeout)
	rewind(m)
	m.EndAbandonedCalls(testTimeout)
}

// The close runs in a goroutine, so "was not closed" is only true after giving one a chance.
// Checked by making IsCallRoom answer true for everything and watching them still pass.
func settle() {
	time.Sleep(50 * time.Millisecond)
}

// The close happens in a goroutine, so the state it sets has to be waited for.
func waitClosed(t *testing.T, c *fakeConn) bool {
	t.Helper()
	for i := 0; i < 200; i++ {
		if c.isClosed() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func TestIsCallRoom(t *testing.T) {
	hex32 := strings.Repeat("a1b2", 8) // 32 hex characters

	cases := []struct {
		name      string
		channelID string
		want      bool
	}{
		{"a one-to-one conversation", "dm_" + hex32, true},
		{"a group conversation", "dm_g" + hex32, true},
		{"a plain voice channel", "general", false},
		{"a channel somebody named to look like one", "dm_lounge", false},
		{"a channel named dm_ and something long but not hex", "dm_" + strings.Repeat("zz", 16), false},
		{"a conversation id one character short", "dm_" + hex32[1:], false},
		{"uppercase, which the server never produces", "dm_" + strings.ToUpper(hex32), false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := IsCallRoom(testServerID+"_"+c.channelID, testServerID)
			if got != c.want {
				t.Fatalf("IsCallRoom(%q) = %v, want %v", c.channelID, got, c.want)
			}
		})
	}
}

func TestIsCallRoomNeedsTheServerPrefix(t *testing.T) {
	hex32 := strings.Repeat("a1b2", 8)

	// A server id may hold an underscore, so the prefix comes off by name
	// rather than by splitting on the separator.
	if !IsCallRoom("my_server_dm_"+hex32, "my_server") {
		t.Fatal("a server id containing an underscore must still resolve its own rooms")
	}
	// And the room has to belong to the server it is being asked about.
	if IsCallRoom("other_dm_"+hex32, testServerID) {
		t.Fatal("a room from a different server is not this server's call")
	}
}

func TestEndsACallSomebodyIsAloneIn(t *testing.T) {
	m := NewManager(false)
	_, conns := addRoom(m, "dm_"+strings.Repeat("a1b2", 8), "lonely")

	sweepPastTheDeadline(m)

	if !waitClosed(t, conns["lonely"]) {
		t.Fatal("the last person in a call should be hung up on; the call is over")
	}
}

func TestLeavesAVoiceChannelAlone(t *testing.T) {
	m := NewManager(false)
	_, conns := addRoom(m, "general", "waiting")

	sweepPastTheDeadline(m)
	settle()

	if conns["waiting"].isClosed() {
		t.Fatal("sitting alone in a voice channel is an ordinary thing to do and must not end it")
	}
}

func TestLeavesACallWithTwoPeopleAlone(t *testing.T) {
	m := NewManager(false)
	_, conns := addRoom(m, "dm_"+strings.Repeat("a1b2", 8), "alice", "bob")

	sweepPastTheDeadline(m)
	settle()

	for id, c := range conns {
		if c.isClosed() {
			t.Fatalf("%s was hung up on during a call with two people in it", id)
		}
	}
}

func TestTheClockRestartsWhenSomebodyJoins(t *testing.T) {
	m := NewManager(false)
	roomID, conns := addRoom(m, "dm_"+strings.Repeat("a1b2", 8), "alice")

	// Alone, and observed.
	m.EndAbandonedCalls(testTimeout)
	rewind(m)

	// Bob arrives before the deadline.
	m.mutex.Lock()
	room := m.rooms[roomID]
	bob := &fakeConn{}
	room.PeerConnections["bob"] = nil
	room.Connections["bob"] = bob
	room.UserIDs["bob"] = "user_bob"
	m.mutex.Unlock()

	m.EndAbandonedCalls(testTimeout)
	settle()
	if conns["alice"].isClosed() || bob.isClosed() {
		t.Fatal("a call that filled up again must not be ended by the clock from before")
	}

	// Bob leaves. Alice is alone again, and the clock starts over rather than
	// being resumed — the previous one was long past.
	m.mutex.Lock()
	delete(room.PeerConnections, "bob")
	delete(room.Connections, "bob")
	delete(room.UserIDs, "bob")
	m.mutex.Unlock()

	m.EndAbandonedCalls(testTimeout)
	settle()
	if conns["alice"].isClosed() {
		t.Fatal("the clock has to start again from when she was left alone, not from the first time")
	}

	// And then it does end, once that new clock is past the deadline.
	rewind(m)
	m.EndAbandonedCalls(testTimeout)
	if !waitClosed(t, conns["alice"]) {
		t.Fatal("alone again and past the deadline: the call should end")
	}
}

func TestZeroTurnsItOff(t *testing.T) {
	m := NewManager(false)
	_, conns := addRoom(m, "dm_"+strings.Repeat("a1b2", 8), "lonely")

	m.EndAbandonedCalls(0)
	m.EndAbandonedCalls(0)
	settle()

	if conns["lonely"].isClosed() {
		t.Fatal("SFU_CALL_ALONE_TIMEOUT=0 has to leave the call up")
	}
}

func TestForgetsARoomThatWentAway(t *testing.T) {
	m := NewManager(false)
	roomID, _ := addRoom(m, "dm_"+strings.Repeat("a1b2", 8), "lonely")

	m.EndAbandonedCalls(testTimeout)

	m.mutex.Lock()
	delete(m.rooms, roomID)
	m.mutex.Unlock()

	m.EndAbandonedCalls(testTimeout)

	m.mutex.Lock()
	_, still := m.aloneSince[roomID]
	m.mutex.Unlock()
	if still {
		t.Fatal("a deleted room must not be left in aloneSince; the next call reusing that id would end at once")
	}
}

/*
Saying you are still here (GRYT-715). The sweep above catches a client that never speaks;
this is the half for one that does, and expects the call to still be there.
*/

func TestStillHereKeepsTheCallUp(t *testing.T) {
	m := NewManager(false)
	roomID, conns := addRoom(m, "dm_"+strings.Repeat("a1b2", 8), "lonely")

	// Alone, observed, and now past the deadline — the next sweep would end it.
	m.EndAbandonedCalls(testTimeout)
	rewind(m)

	if !m.StillHere(roomID) {
		t.Fatal("pressing stay in a call that is about to end should move its clock")
	}

	m.EndAbandonedCalls(testTimeout)
	settle()
	if conns["lonely"].isClosed() {
		t.Fatal("somebody said they were still here and was hung up on anyway")
	}
}

func TestStillHereBuysAWholeTimeoutEachTime(t *testing.T) {
	m := NewManager(false)
	roomID, conns := addRoom(m, "dm_"+strings.Repeat("a1b2", 8), "lonely")

	// Three presses, each one from a call already past its deadline. A reprieve
	// that could only be taken once would end the call on the second.
	for i := 0; i < 3; i++ {
		m.EndAbandonedCalls(testTimeout)
		rewind(m)
		m.StillHere(roomID)
		m.EndAbandonedCalls(testTimeout)
		settle()
		if conns["lonely"].isClosed() {
			t.Fatalf("hung up on after press %d; the clock restarts, it is not a single reprieve", i+1)
		}
	}

	// And the sweep still works afterwards: stop pressing and it ends.
	rewind(m)
	m.EndAbandonedCalls(testTimeout)
	if !waitClosed(t, conns["lonely"]) {
		t.Fatal("once nobody is pressing it, the call has to end like any other")
	}
}

func TestStillHereIgnoresAVoiceChannel(t *testing.T) {
	m := NewManager(false)
	roomID, _ := addRoom(m, "general", "waiting")

	if m.StillHere(roomID) {
		t.Fatal("a voice channel has no clock to restart, so nothing should be written for one")
	}

	m.mutex.Lock()
	_, tracked := m.aloneSince[roomID]
	m.mutex.Unlock()
	if tracked {
		t.Fatal("a client in a channel must not be able to put entries in aloneSince")
	}
}

func TestStillHereIgnoresARoomThatIsGone(t *testing.T) {
	m := NewManager(false)
	roomID := testServerID + "_dm_" + strings.Repeat("a1b2", 8)

	if m.StillHere(roomID) {
		t.Fatal("a room that does not exist has no clock")
	}

	m.mutex.Lock()
	_, tracked := m.aloneSince[roomID]
	m.mutex.Unlock()
	if tracked {
		t.Fatal("naming a room that is not there must not create an entry for it")
	}
}
