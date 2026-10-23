package websocket

import (
	"sync"

	"sfu-v2/internal/auth"
	"sfu-v2/internal/recovery"
	"sfu-v2/pkg/types"
)

// livePeers finds a connected peer's claims by room and user, for user_capabilities. The
// zero value is ready to use.
type livePeers struct {
	mu    sync.Mutex
	peers map[string]livePeer // clientID ->
}

type livePeer struct {
	roomID, userID string
	claims         *auth.Live
}

func (p *livePeers) add(clientID, roomID, userID string, claims *auth.Live) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.peers == nil {
		p.peers = make(map[string]livePeer)
	}
	p.peers[clientID] = livePeer{roomID: roomID, userID: userID, claims: claims}
}

func (p *livePeers) remove(clientID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.peers, clientID)
}

// userOf is the user a connected peer joined as.
func (p *livePeers) userOf(clientID string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	peer, ok := p.peers[clientID]
	return peer.userID, ok
}

// set gives every peer of this user in this room new claims, and says how many it found.
func (p *livePeers) set(roomID, userID string, claims auth.Claims) int {
	p.mu.Lock()
	var found []*auth.Live
	for _, peer := range p.peers {
		if peer.roomID == roomID && peer.userID == userID {
			found = append(found, peer.claims)
		}
	}
	p.mu.Unlock()
	for _, live := range found {
		live.Set(claims)
	}
	return len(found)
}

// handleUserCapabilities replaces a connected member's capabilities, so a permission changed
// mid-call reaches the gate in OnTrack. Only the server that owns the room may send it.
func (h *Handler) handleUserCapabilities(data string) error {
	var req types.UserCapabilitiesData
	if err := recovery.SafeJSONUnmarshal([]byte(data), &req); err != nil {
		h.debugLog("❌ Error unmarshalling user_capabilities data: %v", err)
		return err
	}

	if !h.roomManager.ServerOwnsRoom(req.ServerID, req.ServerPassword, req.RoomID) {
		h.debugLog("❌ user_capabilities: server '%s' failed credentials or does not own room '%s'", req.ServerID, req.RoomID)
		return nil
	}

	n := h.live.set(req.RoomID, req.UserID, auth.Claims{Capabilities: req.Capabilities})
	h.debugLog("🎛️  user_capabilities: room=%s user=%s caps=%v peers=%d", req.RoomID, req.UserID, req.Capabilities, n)
	return nil
}
