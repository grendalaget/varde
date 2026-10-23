package signaling

import (
	"fmt"
	"log"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"

	"sfu-v2/internal/recovery"
	"sfu-v2/internal/room"
	"sfu-v2/internal/track"
	peerManager "sfu-v2/internal/webrtc"
	"sfu-v2/pkg/types"
)

// Coordinator manages the signaling process between peers and tracks
type Coordinator struct {
	trackManager  *track.Manager
	webrtcManager *peerManager.Manager
	roomManager   *room.Manager
	debug         bool
}

// NewCoordinator creates a new signaling coordinator
func NewCoordinator(trackManager *track.Manager, webrtcManager *peerManager.Manager, roomManager *room.Manager, debug bool) *Coordinator {
	return &Coordinator{
		trackManager:  trackManager,
		webrtcManager: webrtcManager,
		roomManager:   roomManager,
		debug:         debug,
	}
}

// debugLog logs debug messages if debug mode is enabled
func (c *Coordinator) debugLog(format string, args ...interface{}) {
	if c.debug {
		log.Printf("[SIGNALING] "+format, args...)
	}
}

// SignalPeerConnectionsInRoom updates each peer connection in a specific room
func (c *Coordinator) SignalPeerConnectionsInRoom(roomID string) {
	recovery.SafeExecuteWithContext("SIGNALING", "SIGNAL_PEERS", "", roomID, "Starting peer signaling", func() error {
		c.debugLog("🔄 Starting peer connection signaling for room '%s'", roomID)

		// Get all peer connections in the room from room manager
		peerMap, err := c.roomManager.GetPeersInRoom(roomID)
		if err != nil {
			c.debugLog("❌ Error getting peers in room %s: %v", roomID, err)
			return err
		}

		connectionMap, err := c.roomManager.GetConnectionsInRoom(roomID)
		if err != nil {
			c.debugLog("❌ Error getting connections in room %s: %v", roomID, err)
			return err
		}

		c.debugLog("🔄 Room '%s' has %d peers and %d connections", roomID, len(peerMap), len(connectionMap))

		// If no peers, nothing to signal
		if len(peerMap) == 0 {
			c.debugLog("📭 No peers in room '%s', skipping signaling", roomID)
			return nil
		}

		// Attempt to synchronize peer connections with recovery
		attemptSync := func() (tryAgain bool) {
			return recovery.SafeExecuteWithContext("SIGNALING", "SYNC_ATTEMPT", "", roomID, "Synchronizing peers", func() error {
				// Get room-specific tracks instead of global tracks
				tracks := c.trackManager.GetTracksInRoom(roomID)
				c.debugLog("🎵 Available tracks for room '%s': %d", roomID, len(tracks))

				syncSuccess := 0
				syncErrors := 0

				for clientID, peerConnection := range peerMap {
					// Validate peer connection with nil check
					if peerConnection == nil {
						c.debugLog("⚠️ Nil peer connection for client %s, skipping", clientID)
						syncErrors++
						continue
					}

					// Check if peer connection is still valid before processing
					connectionState := peerConnection.ConnectionState()
					if connectionState == webrtc.PeerConnectionStateClosed ||
						connectionState == webrtc.PeerConnectionStateFailed {
						c.debugLog("⚠️ Skipping closed/failed peer connection for %s (state: %s)", clientID, connectionState.String())
						continue
					}

					c.debugLog("🔄 Synchronizing peer %s in room '%s' (state: %s)", clientID, roomID, connectionState.String())

					// Get the corresponding WebSocket connection with nil check
					wsConn, exists := connectionMap[clientID]
					if !exists || wsConn == nil {
						c.debugLog("❌ No WebSocket connection found for client %s", clientID)
						syncErrors++
						continue
					}

					// Process peer connection with individual recovery
					peerErr := recovery.SafeExecuteWithContext("SIGNALING", "PROCESS_PEER", clientID, roomID, "Processing individual peer", func() error {
						return c.processPeerConnection(clientID, peerConnection, wsConn, tracks, roomID)
					})

					if peerErr != nil {
						c.debugLog("❌ Error processing peer %s: %v", clientID, peerErr)
						syncErrors++
					} else {
						syncSuccess++
					}
				}

				c.debugLog("🔄 Sync attempt complete: %d successful, %d errors", syncSuccess, syncErrors)

				if syncErrors > 0 {
					return recovery.SafeExecute("SIGNALING", "SYNC_ERROR_HANDLING", func() error {
						// Return error to indicate retry needed, but don't panic
						return fmt.Errorf("sync had %d errors out of %d peers", syncErrors, len(peerMap))
					})
				}

				return nil
			}) != nil // Convert error to boolean for tryAgain
		}

		backoffs := []time.Duration{100 * time.Millisecond, 300 * time.Millisecond, 500 * time.Millisecond}
		for syncAttempt := 0; syncAttempt < len(backoffs); syncAttempt++ {
			c.debugLog("🔄 Sync attempt %d/%d for room '%s'", syncAttempt+1, len(backoffs), roomID)
			if !attemptSync() {
				c.debugLog("✅ Synchronization successful for room '%s' after %d attempts", roomID, syncAttempt+1)
				break
			}
			if syncAttempt == len(backoffs)-1 {
				c.debugLog("⚠️  Max sync attempts reached for room '%s', giving up", roomID)
				return nil
			}
			time.Sleep(backoffs[syncAttempt])
		}

		c.debugLog("✅ Peer connection signaling completed for room '%s'", roomID)
		return nil
	})
}

// processPeerConnection handles the signaling for a single peer connection.
// It uses LayerForwarder to provide per-receiver tracks with SVC layer filtering.
func (c *Coordinator) processPeerConnection(clientID string, peerConnection *webrtc.PeerConnection, wsConn interface{}, tracks map[string]*webrtc.TrackLocalStaticRTP, roomID string) error {
	signalingState := peerConnection.SignalingState()
	if signalingState != webrtc.SignalingStateStable {
		c.debugLog("⏳ Skipping peer %s, signaling state: %v (will retry after backoff)", clientID, signalingState)
		return fmt.Errorf("peer %s signaling not stable (%v)", clientID, signalingState)
	}

	isDeafened := c.roomManager.IsUserDeafened(roomID, clientID)
	hiddenSenders := c.roomManager.HiddenSendersFor(roomID, clientID)

	// Build the set of track IDs this peer should receive.
	wantedTrackIDs := make(map[string]bool, len(tracks))
	for id, t := range tracks {
		if t == nil {
			continue
		}
		if isDeafened && t.Kind() == webrtc.RTPCodecTypeAudio {
			continue
		}
		if len(hiddenSenders) > 0 {
			if info, ok := c.trackManager.GetSenderInfo(roomID, id); ok && hiddenSenders[info.PC] {
				continue
			}
		}
		wantedTrackIDs[id] = true
	}
	if isDeafened {
		c.debugLog("🔇 Peer %s is deafened, reduced to %d tracks", clientID, len(wantedTrackIDs))
	}

	// Map of senders we are already using to avoid duplicates.
	existingSenders := map[string]bool{}
	senderCount := 0
	tracksRemoved := 0

	senders := peerConnection.GetSenders()
	if senders != nil {
		for _, sender := range senders {
			if sender == nil || sender.Track() == nil {
				continue
			}

			senderCount++
			stale := c.fedByNoForwarder(roomID, clientID, sender.Track())
			if !stale {
				existingSenders[sender.Track().ID()] = true
			}

			if !wantedTrackIDs[sender.Track().ID()] || stale {
				c.debugLog("🗑️  Removing obsolete sender track %s from peer %s", sender.Track().ID(), clientID)
				if err := peerConnection.RemoveTrack(sender); err != nil {
					c.debugLog("❌ Error removing sender track: %v", err)
					return err
				}
				tracksRemoved++
			}
		}
	}

	// Avoid receiving tracks we are sending to prevent loopback.
	receiverCount := 0
	receivers := peerConnection.GetReceivers()
	if receivers != nil {
		for _, receiver := range receivers {
			if receiver == nil || receiver.Track() == nil {
				continue
			}
			receiverCount++
			existingSenders[receiver.Track().ID()] = true
		}
	}

	c.debugLog("🔗 Peer %s has %d senders, %d receivers", clientID, senderCount, receiverCount)

	// Add any missing tracks to the peer connection.
	tracksAdded := 0
	for trackID := range wantedTrackIDs {
		if existingSenders[trackID] {
			continue
		}

		// Try to get a per-receiver track from the LayerForwarder.
		var localTrack *webrtc.TrackLocalStaticRTP
		if lf, ok := c.trackManager.GetForwarder(roomID, trackID); ok {
			localTrack = lf.AddReceiver(clientID, -1)
		}

		// Fallback to the shared track if no forwarder is available.
		if localTrack == nil {
			localTrack = tracks[trackID]
		}
		if localTrack == nil {
			c.debugLog("⚠️ Nil track for ID %s, skipping", trackID)
			continue
		}

		c.debugLog("➕ Adding track %s to peer %s", trackID, clientID)
		rtpSender, err := peerConnection.AddTrack(localTrack)
		if err != nil {
			c.debugLog("❌ Error adding track to peer connection: %v", err)
			return err
		}
		tracksAdded++
		c.debugLog("✅ Added track to peer connection in room %s: ID=%s", roomID, trackID)

		if lf, ok := c.trackManager.GetForwarder(roomID, trackID); ok {
			go c.relayReceiverRTCP(rtpSender, lf.GetSenderPC(), lf.GetRemoteSSRC(), clientID, trackID)
		} else if info, ok := c.trackManager.GetSenderInfo(roomID, trackID); ok {
			go c.relayReceiverRTCP(rtpSender, info.PC, info.RemoteSSRC, clientID, trackID)
		}
	}

	if tracksAdded > 0 {
		c.debugLog("➕ Added %d tracks to peer %s", tracksAdded, clientID)
	}

	isNewPeer := peerConnection.ConnectionState() == webrtc.PeerConnectionStateNew
	if tracksAdded == 0 && tracksRemoved == 0 && !isNewPeer {
		c.debugLog("🔗 No track changes for peer %s, skipping offer", clientID)
		return nil
	}

	if isNewPeer {
		c.debugLog("🆕 New peer %s — sending initial offer to establish transport", clientID)
	}

	transceivers := peerConnection.GetTransceivers()
	c.debugLog("📋 Peer %s has %d transceivers before CreateOffer:", clientID, len(transceivers))
	for i, t := range transceivers {
		c.debugLog("   [%d] mid=%q kind=%s direction=%s", i, t.Mid(), t.Kind(), t.Direction())
	}
	c.debugLog("📋 Peer %s connection state=%s, ICE state=%s, ICE gathering=%s",
		clientID, peerConnection.ConnectionState().String(),
		peerConnection.ICEConnectionState().String(),
		peerConnection.ICEGatheringState().String())

	c.debugLog("📤 Creating offer for peer %s (added=%d, removed=%d)", clientID, tracksAdded, tracksRemoved)
	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		c.debugLog("❌ Error creating offer for %s: %v", clientID, err)
		c.debugLog("❌ Post-error transceiver state for %s:", clientID)
		for i, t := range peerConnection.GetTransceivers() {
			c.debugLog("   [%d] mid=%q kind=%s direction=%s", i, t.Mid(), t.Kind(), t.Direction())
		}
		return err
	}
	c.debugLog("✅ Offer created for %s (%d bytes SDP)", clientID, len(offer.SDP))

	if err = peerConnection.SetLocalDescription(offer); err != nil {
		c.debugLog("❌ Error setting local description for %s: %v", clientID, err)
		return err
	}

	offerString, err := recovery.SafeJSONMarshal(offer)
	if err != nil {
		c.debugLog("❌ Error marshalling offer for %s: %v", clientID, err)
		return err
	}

	c.debugLog("📤 Sending offer to peer %s (%d bytes)", clientID, len(offerString))

	return recovery.SafeExecuteWithContext("SIGNALING", "SEND_OFFER", clientID, roomID, "Sending WebRTC offer", func() error {
		if conn, ok := wsConn.(interface{ WriteJSON(interface{}) error }); ok && conn != nil {
			return conn.WriteJSON(&types.WebSocketMessage{
				Event: types.EventOffer,
				Data:  string(offerString),
			})
		}
		return fmt.Errorf("invalid WebSocket connection type for client %s", clientID)
	})
}

// fedByNoForwarder reports a sender whose track the room's forwarder for that id no longer
// feeds. A track taken away and given back gets a new forwarder under the same id (GRYT-1426).
func (c *Coordinator) fedByNoForwarder(roomID, clientID string, sent webrtc.TrackLocal) bool {
	lf, ok := c.trackManager.GetForwarder(roomID, sent.ID())
	return ok && lf.GetReceiverTrack(clientID) != sent
}

// OnTrackAddedToRoom should be called when a new track is added to a room
func (c *Coordinator) OnTrackAddedToRoom(roomID string) {
	recovery.SafeExecuteWithContext("SIGNALING", "TRACK_ADDED", "", roomID, "Track added to room", func() error {
		c.debugLog("🎵 Track added to room '%s', triggering signaling", roomID)
		c.SignalPeerConnectionsInRoom(roomID)
		return nil
	})
}

// OnTrackRemovedFromRoom should be called when a track is removed from a room
func (c *Coordinator) OnTrackRemovedFromRoom(roomID string) {
	recovery.SafeExecuteWithContext("SIGNALING", "TRACK_REMOVED", "", roomID, "Track removed from room", func() error {
		c.debugLog("🎵 Track removed from room '%s', triggering signaling", roomID)
		c.SignalPeerConnectionsInRoom(roomID)
		return nil
	})
}

// relayReceiverRTCP relays PLI and FIR back to the original sender so it can generate
// keyframes. REMB is deliberately not relayed: TWCC gives the sender its own feedback.
func (c *Coordinator) relayReceiverRTCP(rtpSender *webrtc.RTPSender, senderPC *webrtc.PeerConnection, remoteSSRC uint32, receiverID, trackID string) {
	defer func() {
		if r := recover(); r != nil {
			c.debugLog("❌ Panic in RTCP relay for receiver %s track %s: %v", receiverID, trackID, r)
		}
	}()

	for {
		packets, _, err := rtpSender.ReadRTCP()
		if err != nil {
			return
		}

		if senderPC.ConnectionState() == webrtc.PeerConnectionStateClosed {
			return
		}

		var toRelay []rtcp.Packet
		for _, pkt := range packets {
			switch pkt.(type) {
			case *rtcp.PictureLossIndication:
				toRelay = append(toRelay, &rtcp.PictureLossIndication{
					MediaSSRC: remoteSSRC,
				})
			case *rtcp.FullIntraRequest:
				toRelay = append(toRelay, &rtcp.PictureLossIndication{
					MediaSSRC: remoteSSRC,
				})
			}
		}

		if len(toRelay) > 0 {
			if writeErr := senderPC.WriteRTCP(toRelay); writeErr != nil {
				return
			}
		}
	}
}
