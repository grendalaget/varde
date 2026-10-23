package room

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"sfu-v2/internal/recovery"
)

/*
Ending a call one person is sitting in alone (GRYT-711). A channel is a place and sitting in
one alone is ordinary; a call is over. This catches the clients that never end their own.
*/

// callRoomID matches the ids the server gives a conversation, and only those — the whole
// shape, not the `dm_` prefix, since an admin may name a channel `dm_anything`.
var callRoomID = regexp.MustCompile(`^dm_g?[0-9a-f]{32}$`)

// IsCallRoom reports whether an SFU room is a call rather than a voice channel. The prefix is
// taken off by the server id the room carries, since a server id may contain an underscore.
func IsCallRoom(roomID, serverID string) bool {
	if serverID == "" {
		return false
	}
	channelID := strings.TrimPrefix(roomID, serverID+"_")
	if channelID == roomID {
		return false
	}
	return callRoomID.MatchString(channelID)
}

// EndAbandonedCalls hangs up on calls one person has been alone in for longer than timeout;
// zero switches it off. It observes state rather than trusting an "alone since" field.
func (m *Manager) EndAbandonedCalls(timeout time.Duration) {
	if timeout <= 0 {
		return
	}

	recovery.SafeExecuteWithContext("ROOM_MANAGER", "END_ABANDONED_CALLS", "", "", fmt.Sprintf("Alone for: %v", timeout), func() error {
		now := time.Now()

		type victim struct {
			roomID   string
			clientID string
			userID   string
			conn     JSONWriter
			alone    time.Duration
		}
		var victims []victim

		m.mutex.Lock()
		seen := map[string]bool{}

		for roomID, room := range m.rooms {
			room.mutex.RLock()
			peers := len(room.PeerConnections)
			serverID := room.ServerID

			// Only the one-peer case. Zero is already handled: the room empties
			// and CleanupEmptyRooms takes it away.
			if peers != 1 || !IsCallRoom(roomID, serverID) {
				room.mutex.RUnlock()
				continue
			}

			seen[roomID] = true
			since, known := m.aloneSince[roomID]
			if !known {
				m.aloneSince[roomID] = now
				room.mutex.RUnlock()
				continue
			}

			if now.Sub(since) > timeout {
				for clientID, conn := range room.Connections {
					victims = append(victims, victim{
						roomID:   roomID,
						clientID: clientID,
						userID:   room.UserIDs[clientID],
						conn:     conn,
						alone:    now.Sub(since),
					})
					break
				}
			}
			room.mutex.RUnlock()
		}

		// Anything that filled up again, emptied, or was deleted. Left behind, a room that
		// went back to two and then to one would be hung up on with the old clock.
		for roomID := range m.aloneSince {
			if !seen[roomID] {
				delete(m.aloneSince, roomID)
			}
		}
		m.mutex.Unlock()

		// Closing outside the lock. The close runs the connection's own
		// teardown, which calls back into RemovePeerFromRoom and takes m.mutex.
		for _, v := range victims {
			m.debugLog("📴 Ending call '%s': user '%s' has been alone in it for %v", v.roomID, v.userID, v.alone.Round(time.Second))
			if closer, ok := v.conn.(io.Closer); ok {
				go closer.Close()
			}
		}

		return nil
	})
}

// StillHere restarts the alone clock for a room and reports whether it did. It restarts
// rather than granting one reprieve: somebody was at the keyboard, which is the question.
func (m *Manager) StillHere(roomID string) bool {
	moved := false

	recovery.SafeExecuteWithContext("ROOM_MANAGER", "STILL_HERE", "", roomID, "Restarting the alone clock", func() error {
		m.mutex.Lock()
		defer m.mutex.Unlock()

		room, exists := m.rooms[roomID]
		if !exists {
			return nil
		}

		// Only calls, so a client cannot fill the map with rooms the sweep never looks at.
		// A voice channel is never ended for being quiet, so it has no clock.
		room.mutex.RLock()
		isCall := IsCallRoom(roomID, room.ServerID)
		room.mutex.RUnlock()
		if !isCall {
			return nil
		}

		m.aloneSince[roomID] = time.Now()
		moved = true
		return nil
	})

	return moved
}
