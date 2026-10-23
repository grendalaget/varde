package track

import (
	"log"
	"sync"

	"github.com/pion/webrtc/v4"

	"sfu-v2/internal/svc"
)

// SenderInfo holds the originating peer connection and remote SSRC for a track,
// so RTCP from receivers can be relayed back to the sender.
type SenderInfo struct {
	PC         *webrtc.PeerConnection
	RemoteSSRC uint32
}

// Manager handles the lifecycle of media tracks per room
type Manager struct {
	mu sync.RWMutex
	// Map of roomID -> trackID -> track
	roomTracks map[string]map[string]*webrtc.TrackLocalStaticRTP
	// Map of roomID -> trackID -> sender info (peer connection + SSRC)
	roomSenderInfo map[string]map[string]SenderInfo
	// Map of roomID -> trackID -> LayerForwarder (SVC-aware forwarding)
	roomForwarders map[string]map[string]*svc.LayerForwarder
	debug          bool
}

// NewManager creates a new track manager
func NewManager(debug bool) *Manager {
	return &Manager{
		roomTracks:     make(map[string]map[string]*webrtc.TrackLocalStaticRTP),
		roomSenderInfo: make(map[string]map[string]SenderInfo),
		roomForwarders: make(map[string]map[string]*svc.LayerForwarder),
		debug:          debug,
	}
}

// debugLog logs debug messages if debug mode is enabled
func (m *Manager) debugLog(format string, args ...interface{}) {
	if m.debug {
		log.Printf("[TRACK-MANAGER] "+format, args...)
	}
}

// AddTrackToRoom adds a media track to a room, storing the sender's peer connection and SSRC
// so RTCP can be relayed. The returned TrackLocalStaticRTP is a legacy fallback.
func (m *Manager) AddTrackToRoom(roomID string, t *webrtc.TrackRemote, senderPC *webrtc.PeerConnection) *webrtc.TrackLocalStaticRTP {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Create a new local track with the same codec as the incoming remote track
	trackLocal, err := webrtc.NewTrackLocalStaticRTP(t.Codec().RTPCodecCapability, t.ID(), t.StreamID())
	if err != nil {
		m.debugLog("❌ Error creating local track for room '%s': %v", roomID, err)
		return nil
	}

	// Initialize room tracks map if it doesn't exist
	if m.roomTracks[roomID] == nil {
		m.roomTracks[roomID] = make(map[string]*webrtc.TrackLocalStaticRTP)
		m.debugLog("🏠 Initialized track storage for room '%s'", roomID)
	}

	// Store the local track in the room
	m.roomTracks[roomID][t.ID()] = trackLocal

	// Store sender info for RTCP relay
	if m.roomSenderInfo[roomID] == nil {
		m.roomSenderInfo[roomID] = make(map[string]SenderInfo)
	}
	m.roomSenderInfo[roomID][t.ID()] = SenderInfo{
		PC:         senderPC,
		RemoteSSRC: uint32(t.SSRC()),
	}

	// Create a LayerForwarder for SVC-aware per-receiver forwarding.
	if m.roomForwarders[roomID] == nil {
		m.roomForwarders[roomID] = make(map[string]*svc.LayerForwarder)
	}
	lf := svc.NewLayerForwarder(t, senderPC, m.debug)
	m.roomForwarders[roomID][t.ID()] = lf

	codec := t.Codec()
	roomTrackCount := len(m.roomTracks[roomID])
	m.debugLog("🎵 Added track to room '%s': ID=%s, StreamID=%s, Kind=%s, Codec=%s, SSRC=%d (Room tracks: %d)",
		roomID, t.ID(), t.StreamID(), t.Kind().String(), codec.MimeType, t.SSRC(), roomTrackCount)

	return trackLocal
}

// GetSenderInfo returns the sender peer connection and remote SSRC for a track,
// allowing RTCP packets from receivers to be relayed back to the originating sender.
func (m *Manager) GetSenderInfo(roomID, trackID string) (SenderInfo, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	senders, exists := m.roomSenderInfo[roomID]
	if !exists {
		return SenderInfo{}, false
	}
	info, ok := senders[trackID]
	return info, ok
}

// RemoveTrackFromRoom removes a media track from a specific room
func (m *Manager) RemoveTrackFromRoom(roomID string, t *webrtc.TrackLocalStaticRTP) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if the room exists
	roomTracks, roomExists := m.roomTracks[roomID]
	if !roomExists {
		m.debugLog("❌ Cannot remove track: room '%s' does not exist", roomID)
		return
	}

	// Check if the track exists
	if t == nil || roomTracks[t.ID()] == nil {
		m.debugLog("❌ Track or track ID not found in room '%s'", roomID)
		return
	}

	// Stop and remove the layer forwarder
	if fwds := m.roomForwarders[roomID]; fwds != nil {
		if lf, ok := fwds[t.ID()]; ok {
			lf.Stop()
			delete(fwds, t.ID())
		}
		if len(fwds) == 0 {
			delete(m.roomForwarders, roomID)
		}
	}

	// Remove the track and its sender info
	delete(roomTracks, t.ID())
	if senders := m.roomSenderInfo[roomID]; senders != nil {
		delete(senders, t.ID())
		if len(senders) == 0 {
			delete(m.roomSenderInfo, roomID)
		}
	}
	m.debugLog("🗑️  Removed track from room '%s': ID=%s (Remaining tracks: %d)",
		roomID, t.ID(), len(roomTracks))

	// Clean up empty room track storage
	if len(roomTracks) == 0 {
		delete(m.roomTracks, roomID)
		m.debugLog("🧹 Cleaned up empty track storage for room '%s'", roomID)
	}
}

// GetTracksInRoom returns a copy of all tracks in a specific room
func (m *Manager) GetTracksInRoom(roomID string) map[string]*webrtc.TrackLocalStaticRTP {
	m.mu.RLock()
	defer m.mu.RUnlock()

	roomTracks, exists := m.roomTracks[roomID]
	if !exists {
		m.debugLog("📭 No tracks found for room '%s'", roomID)
		return make(map[string]*webrtc.TrackLocalStaticRTP)
	}

	// Create a copy to avoid race conditions
	tracks := make(map[string]*webrtc.TrackLocalStaticRTP)
	for id, track := range roomTracks {
		tracks[id] = track
	}

	m.debugLog("📦 Retrieved %d tracks from room '%s'", len(tracks), roomID)
	return tracks
}

// HasTrack checks whether a track with the given ID exists in a room
func (m *Manager) HasTrack(roomID, trackID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if roomTracks, ok := m.roomTracks[roomID]; ok {
		_, exists := roomTracks[trackID]
		return exists
	}
	return false
}

// GetTrackInRoom returns a specific track by ID from a specific room
func (m *Manager) GetTrackInRoom(roomID, trackID string) (*webrtc.TrackLocalStaticRTP, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	roomTracks, roomExists := m.roomTracks[roomID]
	if !roomExists {
		return nil, false
	}

	track, exists := roomTracks[trackID]
	return track, exists
}

// GetRoomStats returns statistics about tracks per room
func (m *Manager) GetRoomStats() map[string]int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	stats := make(map[string]int)
	for roomID, tracks := range m.roomTracks {
		stats[roomID] = len(tracks)
	}
	return stats
}

// GetForwarder returns the LayerForwarder for a track in a room.
func (m *Manager) GetForwarder(roomID, trackID string) (*svc.LayerForwarder, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if fwds, ok := m.roomForwarders[roomID]; ok {
		lf, exists := fwds[trackID]
		return lf, exists
	}
	return nil, false
}

// GetVideoForwarderByStream finds the video forwarder for a stream id. A viewer knows remote
// video by stream: the track id it sees differs from ours when the SFU reuses an m-line.
func (m *Manager) GetVideoForwarderByStream(roomID, streamID string) (*svc.LayerForwarder, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, lf := range m.roomForwarders[roomID] {
		if lf.StreamID() == streamID && lf.Kind() == webrtc.RTPCodecTypeVideo {
			return lf, true
		}
	}
	return nil, false
}

// GetForwardersInRoom returns all LayerForwarders for tracks in a room.
func (m *Manager) GetForwardersInRoom(roomID string) map[string]*svc.LayerForwarder {
	m.mu.RLock()
	defer m.mu.RUnlock()
	fwds, exists := m.roomForwarders[roomID]
	if !exists {
		return nil
	}
	result := make(map[string]*svc.LayerForwarder, len(fwds))
	for id, lf := range fwds {
		result[id] = lf
	}
	return result
}

// CleanupEmptyRooms removes track storage for rooms with no tracks
func (m *Manager) CleanupEmptyRooms() {
	m.mu.Lock()
	defer m.mu.Unlock()

	cleanedRooms := 0
	for roomID, tracks := range m.roomTracks {
		if len(tracks) == 0 {
			delete(m.roomTracks, roomID)
			delete(m.roomSenderInfo, roomID)
			if fwds, ok := m.roomForwarders[roomID]; ok {
				for _, lf := range fwds {
					lf.Stop()
				}
				delete(m.roomForwarders, roomID)
			}
			cleanedRooms++
			m.debugLog("🧹 Cleaned up empty track storage for room '%s'", roomID)
		}
	}

	if cleanedRooms > 0 {
		m.debugLog("🧹 Cleaned up %d empty room track storages", cleanedRooms)
	}
}
