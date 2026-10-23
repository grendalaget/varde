package audiotransport

import (
	"encoding/binary"
	"sync"
	"time"
)

// datagramSender is what a member's session can do; a WebTransport session's stream in production,
// a fake in tests.
type datagramSender interface {
	SendDatagram([]byte) error
}

type member struct {
	id     uint32
	peerID string
	roomID string
	out    datagramSender
	close  func()
	rate   bucket
}

// hub fans audio out to everyone else in a room. The sender id it puts in front comes from the
// member, which came from a redeemed ticket: never from the packet.
type hub struct {
	mu     sync.Mutex
	rooms  map[string]map[string]*member
	nextID uint32
	// changed is told a room's member list moved, outside the lock (GRYT-1830).
	changed func(roomID string)
	// Counters, for metrics and tests.
	dropped struct{ malformed, rate uint64 }
}

func newHub() *hub { return &hub{rooms: map[string]map[string]*member{}} }

// join adds a peer's session to its room. A peer already there (a reconnect) has its old session closed.
func (h *hub) join(roomID, peerID string, out datagramSender, close func()) *member {
	h.mu.Lock()
	h.nextID++
	m := &member{id: h.nextID, peerID: peerID, roomID: roomID, out: out, close: close}
	room := h.rooms[roomID]
	if room == nil {
		room = map[string]*member{}
		h.rooms[roomID] = room
	}
	if old := room[peerID]; old != nil && old.close != nil {
		old.close()
	}
	room[peerID] = m
	h.mu.Unlock()
	h.notify(roomID)
	return m
}

// leave removes m, but not a newer session that has replaced it.
func (h *hub) leave(m *member) {
	h.mu.Lock()
	room := h.rooms[m.roomID]
	left := room[m.peerID] == m
	if left {
		delete(room, m.peerID)
	}
	if len(room) == 0 {
		delete(h.rooms, m.roomID)
	}
	h.mu.Unlock()
	if left {
		h.notify(m.roomID)
	}
}

func (h *hub) notify(roomID string) {
	if h.changed != nil {
		h.changed(roomID)
	}
}

// senders is who sends under which id in a room, by peer.
func (h *hub) senders(roomID string) map[string]uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]uint32{}
	for peerID, m := range h.rooms[roomID] {
		out[peerID] = m.id
	}
	return out
}

// drop closes a peer's session when it leaves voice over the WebSocket.
func (h *hub) drop(peerID string) {
	h.mu.Lock()
	var found []*member
	for _, room := range h.rooms {
		if m := room[peerID]; m != nil {
			found = append(found, m)
		}
	}
	h.mu.Unlock()
	for _, m := range found {
		if m.close != nil {
			m.close()
		}
		h.leave(m)
	}
}

// forward sends one datagram from m to everyone else in its room, or drops it.
func (h *hub) forward(m *member, b []byte, now time.Time) {
	h.mu.Lock()
	if !validDatagram(b) {
		h.dropped.malformed++
		h.mu.Unlock()
		return
	}
	if !m.rate.allow(now) {
		h.dropped.rate++
		h.mu.Unlock()
		return
	}
	others := make([]*member, 0, len(h.rooms[m.roomID]))
	for _, o := range h.rooms[m.roomID] {
		if o != m {
			others = append(others, o)
		}
	}
	h.mu.Unlock()
	if len(others) == 0 {
		return
	}
	out := make([]byte, senderBytes+len(b))
	binary.BigEndian.PutUint32(out, m.id)
	copy(out[senderBytes:], b)
	for _, o := range others {
		// A full or closed session loses this packet; audio doesn't wait.
		_ = o.out.SendDatagram(out)
	}
}
