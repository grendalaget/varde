package websocket

import (
	"encoding/json"
	"strconv"

	"sfu-v2/pkg/types"
)

// sendAudioSenders tells everyone in a room which own-transport sender id is which user, each time
// a session joins or leaves it (GRYT-1830). A peer that has already left is skipped.
func (h *Handler) sendAudioSenders(roomID string, senders map[string]uint32) {
	users := make(map[string]string, len(senders))
	for peerID, id := range senders {
		if userID, ok := h.live.userOf(peerID); ok {
			users[strconv.FormatUint(uint64(id), 10)] = userID
		}
	}
	payload, err := json.Marshal(types.AudioSendersData{Senders: users})
	if err != nil {
		return
	}
	conns, err := h.roomManager.GetConnectionsInRoom(roomID)
	if err != nil {
		return
	}
	msg := &types.WebSocketMessage{Event: types.EventAudioSenders, Data: string(payload)}
	for clientID, conn := range conns {
		if err := conn.WriteJSON(msg); err != nil {
			h.debugLog("⚠️ audio_senders to %s failed: %v", clientID, err)
		}
	}
	h.sendWebRTCAudioWanted(roomID, senders)
}

// sendWebRTCAudioWanted tells a room whether anyone in it has no transport session (GRYT-1853), on
// every join, leave and session change. A peer between joining and its session counts as needing it.
func (h *Handler) sendWebRTCAudioWanted(roomID string, senders map[string]uint32) {
	conns, err := h.roomManager.GetConnectionsInRoom(roomID)
	if err != nil {
		return
	}
	wanted := false
	for clientID := range conns {
		if _, ok := senders[clientID]; !ok {
			wanted = true
			break
		}
	}
	payload, err := json.Marshal(types.WebRTCAudioData{Wanted: wanted})
	if err != nil {
		return
	}
	msg := &types.WebSocketMessage{Event: types.EventWebRTCAudio, Data: string(payload)}
	for clientID, conn := range conns {
		if err := conn.WriteJSON(msg); err != nil {
			h.debugLog("⚠️ webrtc_audio_wanted to %s failed: %v", clientID, err)
		}
	}
}

// roomChanged is sendWebRTCAudioWanted for a join or leave, when the transport is on.
func (h *Handler) roomChanged(roomID string) {
	if h.audio != nil {
		h.sendWebRTCAudioWanted(roomID, h.audio.Senders(roomID))
	}
}
