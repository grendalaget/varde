package room

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"sfu-v2/internal/auth"
	"sfu-v2/internal/recovery"
	"sfu-v2/pkg/types"
)

// SetRequireClientToken turns off the legacy shared-password join path.
func (m *Manager) SetRequireClientToken(require bool) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.requireClientToken = require
}

// JSONWriter is satisfied by any connection that can write JSON (e.g. ThreadSafeWriter).
type JSONWriter interface {
	WriteJSON(v interface{}) error
}

// Room represents a voice chat room
type Room struct {
	ID              string
	ServerID        string
	PeerConnections map[string]*webrtc.PeerConnection
	Connections     map[string]JSONWriter
	UserIDs         map[string]string // clientID -> userID (server-assigned user identifier)
	DeafenedUsers   map[string]bool   // userID -> true if deafened (SFU stops forwarding audio)
	CreatedAt       time.Time
	LastActivity    time.Time
	mutex           sync.RWMutex
	// userID -> userIDs whose media they don't get. Kept when the peer goes: the server
	// only sends it on a join, and a reconnect isn't one. Made on first use.
	HiddenPeers map[string]map[string]bool
}

// Manager handles room creation and management
type Manager struct {
	rooms             map[string]*Room
	serverToRooms     map[string][]string
	registeredServers map[string]string     // serverID -> serverPassword
	serverConns       map[string]JSONWriter // serverID -> server WebSocket connection
	// When a call room was first seen holding one person. Kept by the sweep in calls.go
	// rather than by the paths that add and remove peers. Guarded by mutex.
	aloneSince map[string]time.Time
	mutex      sync.RWMutex
	debug      bool
	// Refuse the legacy shared-password join path. Guarded by mutex.
	requireClientToken bool
}

// NewManager creates a new room manager
func NewManager(debug bool) *Manager {
	return &Manager{
		rooms:             make(map[string]*Room),
		serverToRooms:     make(map[string][]string),
		registeredServers: make(map[string]string),
		serverConns:       make(map[string]JSONWriter),
		aloneSince:        make(map[string]time.Time),
		debug:             debug,
	}
}

// SetServerConnection stores the WebSocket connection for a registered server.
func (m *Manager) SetServerConnection(serverID string, conn JSONWriter) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.serverConns[serverID] = conn
	m.debugLog("🔗 Stored server connection for '%s'", serverID)
}

// notifyServer sends a WebSocket message to the server that owns the given room.
// Must be called WITHOUT holding m.mutex (or with a read-lock at most).
func (m *Manager) notifyServer(serverID string, event string, payload interface{}) {
	m.mutex.RLock()
	conn, ok := m.serverConns[serverID]
	m.mutex.RUnlock()
	if !ok || conn == nil {
		return
	}
	data, err := json.Marshal(payload)
	if err != nil {
		m.debugLog("❌ Failed to marshal %s payload: %v", event, err)
		return
	}
	recovery.SafeExecute("ROOM_MANAGER", "NOTIFY_SERVER", func() error {
		return conn.WriteJSON(&types.WebSocketMessage{
			Event: event,
			Data:  string(data),
		})
	})
}

// TotalPeers returns the total number of connected peers across all rooms.
func (m *Manager) TotalPeers() int {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	total := 0
	for _, room := range m.rooms {
		room.mutex.RLock()
		total += len(room.PeerConnections)
		room.mutex.RUnlock()
	}
	return total
}

// debugLog logs debug messages if debug mode is enabled
func (m *Manager) debugLog(format string, args ...interface{}) {
	if m.debug {
		log.Printf("[ROOM-MANAGER] "+format, args...)
	}
}

// RegisterServer registers a server and creates a room for it
func (m *Manager) RegisterServer(serverID, serverPassword, roomID string) error {
	return recovery.SafeExecuteWithContext("ROOM_MANAGER", "REGISTER_SERVER", "", roomID, fmt.Sprintf("Server: %s", serverID), func() error {
		m.mutex.Lock()
		defer m.mutex.Unlock()

		m.debugLog("Attempting to register server '%s' with room '%s'", serverID, roomID)

		// An empty secret is not a secret: it is the HMAC key client tokens are verified
		// against, so "" lets anybody mint a token this SFU accepts (GRYT-786).
		if serverPassword == "" {
			log.Printf("❌ Refusing to register server %s with an empty secret. Upgrade the server; it generates one now (GRYT-786)", serverID)
			return fmt.Errorf("server %s registered with an empty secret", serverID)
		}

		// Check if server is already registered
		if existingPassword, exists := m.registeredServers[serverID]; exists {
			if existingPassword != serverPassword {
				m.debugLog("❌ Server '%s' registration failed: password mismatch", serverID)
				// Worth saying what to do about it: this SFU remembers a server's key for
				// the life of the process, so a rotated key needs a restart.
				return fmt.Errorf("server %s is already registered with a different secret; if its key changed, restart this SFU so it forgets the old one", serverID)
			}
			m.debugLog("✅ Server '%s' already registered with matching password", serverID)
		} else {
			m.registeredServers[serverID] = serverPassword
			m.debugLog("✅ Server '%s' registered successfully", serverID)
		}

		// Check if room already exists
		if room, exists := m.rooms[roomID]; exists {
			if room.ServerID != serverID {
				m.debugLog("❌ Room '%s' already exists for different server '%s' (requested by '%s')", roomID, room.ServerID, serverID)
				return fmt.Errorf("room %s already exists for different server", roomID)
			}
			m.debugLog("✅ Room '%s' already exists for server '%s'", roomID, serverID)
			return nil // Room already exists for this server
		}

		// Create new room with recovery protection
		room := &Room{
			ID:              roomID,
			ServerID:        serverID,
			PeerConnections: make(map[string]*webrtc.PeerConnection),
			Connections:     make(map[string]JSONWriter),
			UserIDs:         make(map[string]string),
			DeafenedUsers:   make(map[string]bool),
			CreatedAt:       time.Now(),
			LastActivity:    time.Now(),
		}

		m.rooms[roomID] = room
		m.serverToRooms[serverID] = append(m.serverToRooms[serverID], roomID)

		m.debugLog("🏠 Created new room '%s' for server '%s' (Total rooms: %d)", roomID, serverID, len(m.rooms))
		m.logRoomStats()

		return nil
	})
}

// ValidateClientJoin decides whether a client may enter a room, creating it if needed. The
// token is trusted, not the shared password; the password path is deprecated and logged.
func (m *Manager) ValidateClientJoin(roomID, serverID, serverPassword, userToken, userID string) (auth.Claims, error) {
	claims := auth.Claims{Capabilities: []string{auth.CapSpeak}}
	err := recovery.SafeExecuteWithContext("ROOM_MANAGER", "VALIDATE_CLIENT_JOIN", "", roomID, fmt.Sprintf("Server: %s", serverID), func() error {
		m.mutex.Lock() // Use Lock instead of RLock since we might need to create a room
		defer m.mutex.Unlock()

		m.debugLog("Validating client join: room='%s', server='%s'", roomID, serverID)

		// Check if server is registered
		registeredPassword, exists := m.registeredServers[serverID]
		if !exists {
			m.debugLog("❌ Validation failed: server '%s' not registered", serverID)
			return fmt.Errorf("server %s not registered", serverID)
		}

		// Belt as well as braces. Registration refuses an empty secret above, so this should
		// be unreachable; being wrong means every room on this server stands open.
		if registeredPassword == "" {
			m.debugLog("❌ Validation failed: server '%s' has an empty secret", serverID)
			return fmt.Errorf("server %s has an empty secret", serverID)
		}

		switch {
		case userToken != "":
			verified, err := auth.Verify(registeredPassword, userToken, roomID, userID, time.Now())
			if err != nil {
				m.debugLog("❌ Validation failed: client token rejected for user '%s' in room '%s': %v", userID, roomID, err)
				return fmt.Errorf("client token rejected: %w", err)
			}
			claims = verified
		case m.requireClientToken:
			m.debugLog("❌ Validation failed: no client token, and SFU_REQUIRE_CLIENT_TOKEN is set")
			return fmt.Errorf("a client token is required")
		case registeredPassword == serverPassword:
			// Deprecated. Anybody who has been in a call knows this value.
			log.Printf("⚠️  Client joined room %s on server %s with a server password and no token; upgrade the server so it mints one, then set SFU_REQUIRE_CLIENT_TOKEN=true (GRYT-736)", roomID, serverID)
		default:
			m.debugLog("❌ Validation failed: no client token and the server password did not match, for server '%s'", serverID)
			return fmt.Errorf("client presented neither a valid token nor a matching server password")
		}

		// Check if room exists - if not, create it automatically
		room, exists := m.rooms[roomID]
		if !exists {
			m.debugLog("🏠 Room '%s' does not exist, creating it automatically for server '%s'", roomID, serverID)

			// Create new room automatically
			room = &Room{
				ID:              roomID,
				ServerID:        serverID,
				PeerConnections: make(map[string]*webrtc.PeerConnection),
				Connections:     make(map[string]JSONWriter),
				UserIDs:         make(map[string]string),
				DeafenedUsers:   make(map[string]bool),
				CreatedAt:       time.Now(),
				LastActivity:    time.Now(),
			}

			m.rooms[roomID] = room
			m.serverToRooms[serverID] = append(m.serverToRooms[serverID], roomID)

			m.debugLog("✅ Auto-created room '%s' for server '%s' (Total rooms: %d)", roomID, serverID, len(m.rooms))
			m.logRoomStats()
		} else {
			// Check if room belongs to the server
			if room.ServerID != serverID {
				m.debugLog("❌ Validation failed: room '%s' belongs to server '%s', not '%s'", roomID, room.ServerID, serverID)
				return fmt.Errorf("room %s does not belong to server %s", roomID, serverID)
			}
		}

		m.debugLog("✅ Client join validation passed for room '%s'", roomID)
		return nil
	})
	if err != nil {
		return auth.Claims{}, err
	}
	return claims, nil
}

// GetRoom returns a room by ID
func (m *Manager) GetRoom(roomID string) (*Room, bool) {
	var room *Room
	var exists bool

	recovery.SafeExecuteWithContext("ROOM_MANAGER", "GET_ROOM", "", roomID, "Retrieving room", func() error {
		m.mutex.RLock()
		defer m.mutex.RUnlock()
		room, exists = m.rooms[roomID]

		if m.debug {
			if exists {
				m.debugLog("Retrieved room '%s' (Server: %s, Peers: %d)", roomID, room.ServerID, len(room.PeerConnections))
			} else {
				m.debugLog("Room '%s' not found", roomID)
			}
		}
		return nil
	})

	return room, exists
}

// AddPeerToRoom adds a peer connection to a room and notifies the owning server. If the same
// userID already has a connection there, the old one is evicted first.
func (m *Manager) AddPeerToRoom(roomID, clientID, userID string, pc *webrtc.PeerConnection, conn JSONWriter) error {
	var serverID string
	var evictedPC *webrtc.PeerConnection
	var evictedConn JSONWriter

	err := recovery.SafeExecuteWithContext("ROOM_MANAGER", "ADD_PEER", clientID, roomID, "Adding peer to room", func() error {
		m.mutex.Lock()
		defer m.mutex.Unlock()

		room, exists := m.rooms[roomID]
		if !exists {
			m.debugLog("❌ Cannot add peer '%s': room '%s' does not exist", clientID, roomID)
			return fmt.Errorf("room %s does not exist", roomID)
		}

		if pc == nil {
			m.debugLog("❌ Cannot add peer '%s': peer connection is nil", clientID)
			return fmt.Errorf("peer connection is nil for client %s", clientID)
		}

		if conn == nil {
			m.debugLog("❌ Cannot add peer '%s': websocket connection is nil", clientID)
			return fmt.Errorf("websocket connection is nil for client %s", clientID)
		}

		serverID = room.ServerID

		return recovery.SafeExecuteWithContext("ROOM_MANAGER", "MODIFY_ROOM", clientID, roomID, "Modifying room state", func() error {
			room.mutex.Lock()
			defer room.mutex.Unlock()

			if userID != "" {
				for oldClientID, oldUserID := range room.UserIDs {
					if oldUserID == userID && oldClientID != clientID {
						m.debugLog("♻️ Evicting stale peer '%s' (same user '%s') from room '%s'", oldClientID, userID, roomID)
						evictedPC = room.PeerConnections[oldClientID]
						evictedConn = room.Connections[oldClientID]
						delete(room.PeerConnections, oldClientID)
						delete(room.Connections, oldClientID)
						delete(room.UserIDs, oldClientID)
						delete(room.DeafenedUsers, userID)
						break
					}
				}
			}

			room.PeerConnections[clientID] = pc
			room.Connections[clientID] = conn
			if userID != "" {
				room.UserIDs[clientID] = userID
			}
			room.LastActivity = time.Now()

			m.debugLog("👤 Added peer '%s' (user=%s) to room '%s' (Total peers: %d)", clientID, userID, roomID, len(room.PeerConnections))
			m.logRoomDetails(room)

			return nil
		})
	})

	// Close evicted resources outside locks, to avoid deadlocks with
	// OnConnectionStateChange. The old goroutine's defers find the peer already removed.
	if evictedConn != nil {
		if closer, ok := evictedConn.(io.Closer); ok {
			go closer.Close()
		}
	}
	if evictedPC != nil {
		go evictedPC.Close()
	}

	if err == nil && userID != "" && serverID != "" {
		m.notifyServer(serverID, types.EventPeerJoined, types.PeerEventData{
			RoomID: roomID,
			UserID: userID,
		})
	}

	return err
}

// RemovePeerFromRoom removes a peer connection from a room and notifies the owning server.
func (m *Manager) RemovePeerFromRoom(roomID, clientID string) error {
	var serverID, userID string

	err := recovery.SafeExecuteWithContext("ROOM_MANAGER", "REMOVE_PEER", clientID, roomID, "Removing peer from room", func() error {
		m.mutex.Lock()
		defer m.mutex.Unlock()

		room, exists := m.rooms[roomID]
		if !exists {
			m.debugLog("❌ Cannot remove peer '%s': room '%s' does not exist", clientID, roomID)
			return fmt.Errorf("room %s does not exist", roomID)
		}

		serverID = room.ServerID

		return recovery.SafeExecuteWithContext("ROOM_MANAGER", "MODIFY_ROOM", clientID, roomID, "Modifying room state", func() error {
			room.mutex.Lock()
			defer room.mutex.Unlock()

			userID = room.UserIDs[clientID]
			delete(room.PeerConnections, clientID)
			delete(room.Connections, clientID)
			delete(room.UserIDs, clientID)
			if userID != "" {
				delete(room.DeafenedUsers, userID)
			}
			room.LastActivity = time.Now()

			m.debugLog("👤 Removed peer '%s' (user=%s) from room '%s' (Remaining peers: %d)", clientID, userID, roomID, len(room.PeerConnections))
			m.logRoomDetails(room)

			return nil
		})
	})

	if err == nil && userID != "" && serverID != "" {
		m.notifyServer(serverID, types.EventPeerLeft, types.PeerEventData{
			RoomID: roomID,
			UserID: userID,
		})
	}

	return err
}

// GetPeersInRoom returns all peer connections in a room
func (m *Manager) GetPeersInRoom(roomID string) (map[string]*webrtc.PeerConnection, error) {
	var result map[string]*webrtc.PeerConnection

	err := recovery.SafeExecuteWithContext("ROOM_MANAGER", "GET_PEERS", "", roomID, "Getting peers in room", func() error {
		m.mutex.RLock()
		defer m.mutex.RUnlock()

		room, exists := m.rooms[roomID]
		if !exists {
			m.debugLog("❌ Cannot get peers: room '%s' does not exist", roomID)
			return fmt.Errorf("room %s does not exist", roomID)
		}

		// Safe room access
		return recovery.SafeExecuteWithContext("ROOM_MANAGER", "ACCESS_ROOM", "", roomID, "Accessing room state", func() error {
			room.mutex.RLock()
			defer room.mutex.RUnlock()

			// Create a copy to avoid concurrent map access
			result = make(map[string]*webrtc.PeerConnection)
			for clientID, pc := range room.PeerConnections {
				if pc != nil { // Only include non-nil peer connections
					result[clientID] = pc
				}
			}

			m.debugLog("Retrieved %d peers from room '%s'", len(result), roomID)
			return nil
		})
	})

	if err != nil {
		return nil, err
	}
	return result, nil
}

// GetConnectionsInRoom returns all WebSocket connections in a room
func (m *Manager) GetConnectionsInRoom(roomID string) (map[string]JSONWriter, error) {
	var result map[string]JSONWriter

	err := recovery.SafeExecuteWithContext("ROOM_MANAGER", "GET_CONNECTIONS", "", roomID, "Getting connections in room", func() error {
		m.mutex.RLock()
		defer m.mutex.RUnlock()

		room, exists := m.rooms[roomID]
		if !exists {
			m.debugLog("❌ Cannot get connections: room '%s' does not exist", roomID)
			return fmt.Errorf("room %s does not exist", roomID)
		}

		// Safe room access
		return recovery.SafeExecuteWithContext("ROOM_MANAGER", "ACCESS_ROOM", "", roomID, "Accessing room state", func() error {
			room.mutex.RLock()
			defer room.mutex.RUnlock()

			// Create a copy to avoid concurrent map access
			result = make(map[string]JSONWriter)
			for clientID, conn := range room.Connections {
				if conn != nil { // Only include non-nil connections
					result[clientID] = conn
				}
			}

			m.debugLog("Retrieved %d connections from room '%s'", len(result), roomID)
			return nil
		})
	})

	if err != nil {
		return nil, err
	}
	return result, nil
}

// DisconnectUser force-disconnects a user from a room by closing their WebSocket.
// The deferred cleanup in the handler goroutine will call RemovePeerFromRoom automatically.
func (m *Manager) DisconnectUser(roomID, userID string) error {
	return recovery.SafeExecuteWithContext("ROOM_MANAGER", "DISCONNECT_USER", userID, roomID, "Disconnecting user", func() error {
		m.mutex.RLock()
		room, exists := m.rooms[roomID]
		m.mutex.RUnlock()
		if !exists {
			return fmt.Errorf("room %s does not exist", roomID)
		}

		room.mutex.RLock()
		var targetClientID string
		var targetConn JSONWriter
		for cid, uid := range room.UserIDs {
			if uid == userID {
				targetClientID = cid
				targetConn = room.Connections[cid]
				break
			}
		}
		room.mutex.RUnlock()

		if targetClientID == "" {
			return fmt.Errorf("user %s not found in room %s", userID, roomID)
		}

		m.debugLog("🔌 Force-disconnecting user '%s' (client=%s) from room '%s'", userID, targetClientID, roomID)

		if closer, ok := targetConn.(io.Closer); ok {
			return closer.Close()
		}
		return fmt.Errorf("connection for user %s does not support Close", userID)
	})
}

// GetRoomPeersForServer returns a list of RoomPeers for all rooms
// belonging to the given server. Used for sync_response.
func (m *Manager) GetRoomPeersForServer(serverID string) []types.RoomPeers {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	roomIDs := m.serverToRooms[serverID]
	result := make([]types.RoomPeers, 0, len(roomIDs))

	for _, roomID := range roomIDs {
		room, exists := m.rooms[roomID]
		if !exists {
			continue
		}
		room.mutex.RLock()
		userIDs := make([]string, 0, len(room.UserIDs))
		for _, uid := range room.UserIDs {
			userIDs = append(userIDs, uid)
		}
		room.mutex.RUnlock()

		if len(userIDs) > 0 {
			result = append(result, types.RoomPeers{
				RoomID:  roomID,
				UserIDs: userIDs,
			})
		}
	}

	return result
}

// ValidateServerCredentials checks if the given server ID and password are valid.
func (m *Manager) ValidateServerCredentials(serverID, serverPassword string) bool {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	pw, exists := m.registeredServers[serverID]
	return exists && pw == serverPassword
}

// ServerOwnsRoom checks the server's credentials and that the room is one it registered, so
// one server on a shared SFU cannot change what another's members may do.
func (m *Manager) ServerOwnsRoom(serverID, serverPassword, roomID string) bool {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	pw, exists := m.registeredServers[serverID]
	if !exists || pw == "" || subtle.ConstantTimeCompare([]byte(pw), []byte(serverPassword)) != 1 {
		return false
	}
	room, exists := m.rooms[roomID]
	return exists && room.ServerID == serverID
}

// SetUserDeafened updates the deafen state for a user in a room.
func (m *Manager) SetUserDeafened(roomID, userID string, deafened bool) error {
	m.mutex.RLock()
	room, exists := m.rooms[roomID]
	m.mutex.RUnlock()
	if !exists {
		return fmt.Errorf("room %s does not exist", roomID)
	}

	room.mutex.Lock()
	defer room.mutex.Unlock()

	if deafened {
		room.DeafenedUsers[userID] = true
	} else {
		delete(room.DeafenedUsers, userID)
	}
	m.debugLog("🔇 User '%s' in room '%s': deafened=%v", userID, roomID, deafened)
	return nil
}

// IsUserDeafened checks whether a peer (identified by SFU clientID) is deafened.
// It resolves the clientID to a userID via the room's UserIDs map.
func (m *Manager) IsUserDeafened(roomID, clientID string) bool {
	m.mutex.RLock()
	room, exists := m.rooms[roomID]
	m.mutex.RUnlock()
	if !exists {
		return false
	}

	room.mutex.RLock()
	defer room.mutex.RUnlock()

	userID, ok := room.UserIDs[clientID]
	if !ok {
		return false
	}
	return room.DeafenedUsers[userID]
}

// SetHiddenPeers replaces the users whose media userID doesn't get in this room.
func (m *Manager) SetHiddenPeers(roomID, userID string, hidden []string) error {
	m.mutex.RLock()
	room, exists := m.rooms[roomID]
	m.mutex.RUnlock()
	if !exists {
		return fmt.Errorf("room %s does not exist", roomID)
	}

	room.mutex.Lock()
	defer room.mutex.Unlock()

	if room.HiddenPeers == nil {
		room.HiddenPeers = make(map[string]map[string]bool)
	}
	if len(hidden) == 0 {
		delete(room.HiddenPeers, userID)
		return nil
	}
	set := make(map[string]bool, len(hidden))
	for _, id := range hidden {
		set[id] = true
	}
	room.HiddenPeers[userID] = set
	m.debugLog("🙈 User '%s' in room '%s' hides %d users", userID, roomID, len(set))
	return nil
}

// HiddenSendersFor returns the peer connections whose tracks the peer clientID must not get.
func (m *Manager) HiddenSendersFor(roomID, clientID string) map[*webrtc.PeerConnection]bool {
	m.mutex.RLock()
	room, exists := m.rooms[roomID]
	m.mutex.RUnlock()
	if !exists {
		return nil
	}

	room.mutex.RLock()
	defer room.mutex.RUnlock()

	hidden := room.HiddenPeers[room.UserIDs[clientID]]
	if len(hidden) == 0 {
		return nil
	}
	senders := make(map[*webrtc.PeerConnection]bool)
	for otherClientID, otherUserID := range room.UserIDs {
		if hidden[otherUserID] && room.PeerConnections[otherClientID] != nil {
			senders[room.PeerConnections[otherClientID]] = true
		}
	}
	return senders
}
