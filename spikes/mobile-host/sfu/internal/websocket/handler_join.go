package websocket

import (
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"sfu-v2/internal/auth"
	"sfu-v2/internal/iceguard"
	"sfu-v2/internal/metrics"
	"sfu-v2/internal/recovery"
	peerManager "sfu-v2/internal/webrtc"
	"sfu-v2/pkg/types"
)

// handleClientConnection handles client WebRTC connections
func (h *Handler) handleClientConnection(conn *ThreadSafeWriter, clientID string, r *http.Request) error {
	return recovery.SafeExecuteWithContext("WEBSOCKET", "HANDLE_CLIENT", clientID, "", "Client connection handling", func() error {
		h.debugLog("👤 Client connection established: %s", clientID)

		// Wait for client join message with room information
		var raw []byte
		var err error

		err = recovery.SafeExecuteWithContext("WEBSOCKET", "READ_CLIENT_JOIN", clientID, "", "Reading initial client message", func() error {
			_, raw, err = conn.ReadMessage()
			return err
		})

		if err != nil {
			h.debugLog("❌ Error reading initial client message from %s: %v", clientID, err)
			return err
		}

		message := &types.WebSocketMessage{}
		if err := recovery.SafeJSONUnmarshal(raw, &message); err != nil {
			h.debugLog("❌ Error unmarshalling initial client message from %s: %v", clientID, err)
			return err
		}

		h.debugLog("📨 Client initial message from %s: event=%s", clientID, message.Event)

		if message.Event != types.EventClientJoin {
			h.debugLog("❌ Expected client_join event from %s, got: %s", clientID, message.Event)
			h.sendErrorToConnection(conn, "Expected client_join event")
			return fmt.Errorf("expected client_join event, got: %s", message.Event)
		}

		var joinData types.ClientJoinData
		if err := recovery.SafeJSONUnmarshal([]byte(message.Data), &joinData); err != nil {
			h.debugLog("❌ Error unmarshalling client join data from %s: %v", clientID, err)
			h.sendErrorToConnection(conn, "Invalid join data")
			return err
		}

		h.debugLog("👤 Client %s attempting to join room '%s' (Server: %s)", clientID, joinData.RoomID, joinData.ServerID)

		// Validate client can join the room
		claims, err := h.roomManager.ValidateClientJoin(joinData.RoomID, joinData.ServerID, joinData.ServerPassword, joinData.UserToken, joinData.UserID)
		if err != nil {
			h.debugLog("❌ Client join validation failed for %s: %v", clientID, err)
			h.sendErrorToConnection(conn, "Join validation failed: "+err.Error())
			return err
		}

		h.debugLog("✅ Client %s validated for room '%s'", clientID, joinData.RoomID)

		// Capacity guardrail on what this machine should carry, not on ports: one muxed UDP
		// port takes far more peers than a host has CPU and upload for.
		if h.config.MaxPeers > 0 {
			currentPeers := h.roomManager.TotalPeers()
			if currentPeers >= h.config.MaxPeers {
				msg := fmt.Sprintf("Sorry, there are no seats left in this voice server (%d/%d). Try again later.", currentPeers, h.config.MaxPeers)
				h.debugLog("🚫 Rejecting client %s: capacity reached (%d/%d)", clientID, currentPeers, h.config.MaxPeers)
				h.sendErrorToConnection(conn, msg)
				return fmt.Errorf("%w: %d/%d", ErrServerFull, currentPeers, h.config.MaxPeers)
			}
		}

		// Create WebRTC peer connection with recovery
		var peerConnection *webrtc.PeerConnection
		err = recovery.SafeExecuteWithContext("WEBSOCKET", "CREATE_PEER_CONNECTION", clientID, joinData.RoomID, "Creating WebRTC peer connection", func() error {
			config := webrtc.Configuration{
				ICEServers: h.config.ICEServers,
			}

			var createErr error
			peerConnection, createErr = peerManager.CreatePeerConnection(h.webrtcAPI, config)
			if createErr != nil {
				h.debugLog("❌ Error creating WebRTC peer connection for %s: %v", clientID, createErr)
				h.sendErrorToConnection(conn, "Failed to create peer connection")
				return createErr
			}

			h.debugLog("🔗 Created WebRTC peer connection for client %s", clientID)
			return nil
		})

		if err != nil {
			return err
		}

		// Ensure peer connection cleanup
		defer func() {
			recovery.SafeExecuteWithContext("WEBSOCKET", "CLEANUP_PEER_CONNECTION", clientID, joinData.RoomID, "Cleaning up peer connection", func() error {
				if peerConnection != nil {
					peerConnection.Close()
				}
				return nil
			})
		}()

		// Findable before AddPeerToRoom tells the server this peer is here: the server sends
		// capabilities on peer_joined, and one arriving earlier would find nobody.
		live := auth.NewLive(claims)
		h.live.add(clientID, joinData.RoomID, joinData.UserID, live)
		defer h.live.remove(clientID)

		// Add peer to room managers with recovery
		err = recovery.SafeExecuteWithContext("WEBSOCKET", "ADD_PEER_TO_ROOM", clientID, joinData.RoomID, "Adding peer to room", func() error {
			if err := h.roomManager.AddPeerToRoom(joinData.RoomID, clientID, joinData.UserID, peerConnection, conn); err != nil {
				h.debugLog("❌ Error adding peer %s to room %s: %v", clientID, joinData.RoomID, err)
				h.sendErrorToConnection(conn, "Failed to join room")
				return err
			}

			h.webrtcManager.AddPeerToRoom(joinData.RoomID, clientID, peerConnection, conn)
			return nil
		})

		if err != nil {
			return err
		}

		// Remove peer from both managers on disconnect
		defer func() {
			recovery.SafeExecuteWithContext("WEBSOCKET", "REMOVE_PEER_FROM_ROOM", clientID, joinData.RoomID, "Removing peer from room", func() error {
				h.debugLog("🚪 Client %s leaving room '%s'", clientID, joinData.RoomID)
				h.roomManager.RemovePeerFromRoom(joinData.RoomID, clientID)
				h.webrtcManager.RemovePeerFromRoom(joinData.RoomID, clientID)
				h.roomChanged(joinData.RoomID)
				// Its demand goes too, which can let a sender shrink or pause.
				for _, lf := range h.trackManager.GetForwardersInRoom(joinData.RoomID) {
					lf.RemoveReceiver(clientID)
				}
				h.coordinator.SignalPeerConnectionsInRoom(joinData.RoomID)
				return nil
			})
		}()

		// Send success message
		h.debugLog("✅ Client %s successfully joined room '%s'", clientID, joinData.RoomID)
		h.sendRoomJoined(conn, "Successfully joined room", h.audioOffer(clientID, joinData.RoomID))
		h.roomChanged(joinData.RoomID)
		if h.audio != nil {
			defer h.audio.Drop(clientID)
		}

		// Set up WebRTC event handlers with recovery
		h.setupWebRTCHandlers(peerConnection, conn, clientID, joinData.RoomID, live)

		// Signal the new peer connection to start the negotiation process
		recovery.SafeExecuteWithContext("WEBSOCKET", "SIGNAL_PEER_CONNECTIONS", clientID, joinData.RoomID, "Starting peer signaling", func() error {
			h.debugLog("🔄 Starting peer connection signaling for %s in room '%s'", clientID, joinData.RoomID)
			h.coordinator.SignalPeerConnectionsInRoom(joinData.RoomID)
			return nil
		})

		// Handle incoming WebSocket messages from the client
		return h.handleClientMessages(conn, peerConnection, joinData.RoomID, clientID)
	})
}

// setupWebRTCHandlers sets up WebRTC event handlers with crash protection. Exactly one
// OnConnectionStateChange registration: pion's Store replaces, so a second turns the first off.
func (h *Handler) setupWebRTCHandlers(peerConnection *webrtc.PeerConnection, conn *ThreadSafeWriter, clientID, roomID string, live *auth.Live) {
	// Closed once, when the peer connection reaches a state it cannot come back from. Every
	// track's cleanup waits on this, so they all fire rather than only the last registered.
	closed := make(chan struct{})
	var closeOnce sync.Once
	markClosed := func() {
		closeOnce.Do(func() { close(closed) })
	}
	// Set up ICE candidate handling with recovery
	peerConnection.OnICECandidate(func(i *webrtc.ICECandidate) {
		recovery.SafeExecuteWithContext("WEBRTC", "ICE_CANDIDATE", clientID, roomID, "Handling ICE candidate", func() error {
			if i == nil {
				h.debugLog("🔧 ICE gathering complete for %s (nil candidate sentinel)", clientID)
				return nil
			}

			h.debugLog("🔧 ICE candidate for %s: type=%s protocol=%s address=%s:%d",
				clientID, i.Typ.String(), i.Protocol.String(), i.Address, i.Port)

			/* Checked on the way out, not only rewritten on the way in. It never fires in the
			 * shipped configuration; it fires for an operator who turns STUN back on. */
			if !iceguard.Allowed(i.Address, h.config.ICEAdvertiseIPs) {
				log.Printf("🧊 Dropping ICE candidate for %s: address %s is not in ICE_ADVERTISE_IP (type=%s)",
					clientID, i.Address, i.Typ.String())
				return nil
			}

			candidateString, err := recovery.SafeJSONMarshal(i.ToJSON())
			if err != nil {
				h.debugLog("❌ Error marshalling ICE candidate for %s: %v", clientID, err)
				return err
			}

			if writeErr := conn.WriteJSON(&types.WebSocketMessage{
				Event: types.EventCandidate,
				Data:  string(candidateString),
			}); writeErr != nil {
				h.debugLog("❌ Error sending candidate JSON to %s: %v", clientID, writeErr)
				return writeErr
			}
			return nil
		})
	})

	// Handle connection state changes with recovery
	peerConnection.OnConnectionStateChange(func(p webrtc.PeerConnectionState) {
		recovery.SafeExecuteWithContext("WEBRTC", "CONNECTION_STATE_CHANGE", clientID, roomID, p.String(), func() error {
			h.debugLog("🔗 Peer state change %s: connection=%s ICE=%s signaling=%s gathering=%s",
				clientID, p.String(),
				peerConnection.ICEConnectionState().String(),
				peerConnection.SignalingState().String(),
				peerConnection.ICEGatheringState().String())
			switch p {
			case webrtc.PeerConnectionStateFailed:
				h.debugLog("❌ Peer connection failed for %s", clientID)
				markClosed()
				if err := peerConnection.Close(); err != nil {
					h.debugLog("❌ Peer connection failed to close for %s: %v", clientID, err)
				}
			case webrtc.PeerConnectionStateClosed:
				h.debugLog("🔌 Peer connection closed for %s", clientID)
				markClosed()
				h.coordinator.SignalPeerConnectionsInRoom(roomID)
			case webrtc.PeerConnectionStateConnected:
				h.debugLog("✅ Peer connection established for %s in room '%s'", clientID, roomID)
			}
			return nil
		})
	})

	// Already gone by the time the handler was attached, which pion will not call back
	// about. Rare, and the cost of missing it is the exact bug this function fixes.
	if st := peerConnection.ConnectionState(); st == webrtc.PeerConnectionStateClosed || st == webrtc.PeerConnectionStateFailed {
		markClosed()
	}

	// Each track alternates between held and forwarded for as long as the peer connection
	// lives, as the server changes what this member may send (GRYT-1426).
	peerConnection.OnTrack(func(t *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		recovery.SafeExecuteWithContext("WEBRTC", "TRACK_RECEIVED", clientID, roomID, fmt.Sprintf("Track: %s", t.Kind().String()), func() error {
			h.debugLog("🎵 Incoming track from %s in room '%s': %s (SSRC: %d)", clientID, roomID, t.Kind().String(), t.SSRC())

			drainingRTCP := false
			for {
				if !h.holdWhileRefused(t, peerConnection, receiver, live, closed, clientID, roomID) {
					return nil
				}
				// The sender's reports only reach the interceptors when read. Unread, the receiver
				// reports we send back carry no LSR, and the browser gets no round-trip time.
				if !drainingRTCP {
					go drainRTCP(receiver)
					drainingRTCP = true
				}
				if !h.forwardWhileAllowed(t, peerConnection, receiver, conn, live, closed, clientID, roomID) {
					return nil
				}
			}
		})
	})
}

// holdPoll is how often a held track looks for a changed capability while nothing arrives.
// It bounds how long a re-granted track waits, and costs a timer per held track.
const holdPoll = 100 * time.Millisecond

// holdWhileRefused reads and drops a track until the claims let it through, which returns
// true, or the peer goes, false. Read so what is let through later is live, not a backlog.
func (h *Handler) holdWhileRefused(t *webrtc.TrackRemote, pc *webrtc.PeerConnection, receiver *webrtc.RTPReceiver, live *auth.Live, closed <-chan struct{}, clientID, roomID string) bool {
	buf := make([]byte, 1500)
	held := false
	defer func() {
		if held {
			_ = t.SetReadDeadline(time.Time{})
		}
	}()
	for {
		claims, changed := live.Snapshot()
		denied := refusedBy(claims, pc, receiver, t.Kind())
		if denied == "" {
			return true
		}
		// Denied on this channel: drop the track rather than forward it. This is the only
		// place the gate can be real — a client check is decoration.
		if !held {
			h.debugLog("🔇 Refusing %s track from %s in room '%s': not granted %q", t.Kind().String(), clientID, roomID, denied)
			metrics.TracksRefused.Inc()
			held = true
		}
		select {
		case <-closed:
			return false
		case <-changed:
			continue
		default:
		}
		_ = t.SetReadDeadline(time.Now().Add(holdPoll))
		if _, _, err := t.Read(buf); err != nil && !isReadDeadline(err) {
			return false
		}
	}
}

// forwardWhileAllowed puts the track in the room until the claims take it away, which
// returns true, or the peer goes, false. Either way it leaves the room before returning.
func (h *Handler) forwardWhileAllowed(t *webrtc.TrackRemote, pc *webrtc.PeerConnection, receiver *webrtc.RTPReceiver, conn *ThreadSafeWriter, live *auth.Live, closed <-chan struct{}, clientID, roomID string) bool {
	// The LayerForwarder created inside AddTrackToRoom does all RTP forwarding, so there is
	// no separate goroutine.
	trackLocal := h.trackManager.AddTrackToRoom(roomID, t, pc)
	if trackLocal == nil {
		h.debugLog("❌ Failed to create local track for %s", clientID)
		return false
	}

	defer func() {
		recovery.SafeExecuteWithContext("WEBRTC", "CLEANUP_TRACK", clientID, roomID, "Cleaning up track", func() error {
			h.trackManager.RemoveTrackFromRoom(roomID, trackLocal)
			h.coordinator.OnTrackRemovedFromRoom(roomID)
			metrics.TracksActive.Dec()
			return nil
		})
	}()

	if t.Kind() == webrtc.RTPCodecTypeVideo {
		if lf, ok := h.trackManager.GetForwarder(roomID, t.ID()); ok {
			h.watchVideoDemand(lf, conn, midOf(pc, receiver))
		}
	}

	h.debugLog("🎵 Created local track with LayerForwarder for %s", clientID)
	metrics.TracksActive.Inc()
	h.coordinator.OnTrackAddedToRoom(roomID)

	// Until the peer connection closes (the shared channel, see setupWebRTCHandlers), or the
	// server takes the capability away.
	for {
		claims, changed := live.Snapshot()
		if denied := refusedBy(claims, pc, receiver, t.Kind()); denied != "" {
			h.debugLog("🔇 Cutting %s track from %s in room '%s': %q taken away", t.Kind().String(), clientID, roomID, denied)
			return true
		}
		select {
		case <-closed:
			return false
		case <-changed:
		}
	}
}

// drainRTCP reads a receiver's incoming RTCP until the receiver stops, discarding it once
// the interceptors have seen it.
func drainRTCP(receiver *webrtc.RTPReceiver) {
	if receiver == nil {
		return
	}
	buf := make([]byte, 1500)
	for {
		if _, _, err := receiver.Read(buf); err != nil {
			return
		}
	}
}

// The SFU makes every offer, so these four slots are the only ones a client can send on,
// and it cannot reorder them. TestTheSFUOffersMicrophoneFirst pins the order.
const (
	slotMicrophone = iota
	slotCamera
	slotScreen
	slotScreenAudio
)

// slotOf finds which transceiver a track arrived on, or -1 for one this peer connection
// does not own.
func slotOf(pc *webrtc.PeerConnection, receiver *webrtc.RTPReceiver) (int, webrtc.RTPCodecType) {
	if receiver == nil {
		return -1, 0
	}
	for i, transceiver := range pc.GetTransceivers() {
		if transceiver.Receiver() == receiver {
			return i, transceiver.Kind()
		}
	}
	return -1, 0
}

// isMicrophone reports whether a track arrived on the transceiver set aside for microphone
// audio. Kind is checked as well as position, so reordering stops the gate rather than moves it.
func isMicrophone(pc *webrtc.PeerConnection, receiver *webrtc.RTPReceiver) bool {
	slot, kind := slotOf(pc, receiver)
	return slot == slotMicrophone && kind == webrtc.RTPCodecTypeAudio
}

// refusedBy names the capability a track needs and the token lacks, or "" to forward it.
// Camera and screen are told apart by slot, which is as far as the SFU can see.
func refusedBy(claims auth.Claims, pc *webrtc.PeerConnection, receiver *webrtc.RTPReceiver, trackKind webrtc.RTPCodecType) string {
	slot, kind := slotOf(pc, receiver)
	var need []string
	switch {
	case slot == slotMicrophone && kind == webrtc.RTPCodecTypeAudio:
		if !claims.Can(auth.CapSpeak) {
			return auth.CapSpeak
		}
		return ""
	case slot == slotCamera && kind == webrtc.RTPCodecTypeVideo:
		need = []string{auth.CapShareVideo}
	case slot == slotScreen && kind == webrtc.RTPCodecTypeVideo,
		slot == slotScreenAudio && kind == webrtc.RTPCodecTypeAudio:
		need = []string{auth.CapShareScreen}
	case trackKind == webrtc.RTPCodecTypeVideo:
		// Video from a slot that should not exist. Unreachable today, and fails closed: it
		// passes only a token that would let either kind through.
		need = []string{auth.CapShareVideo, auth.CapShareScreen}
	}
	for _, capability := range need {
		if !claims.MayShare(capability) {
			return capability
		}
	}
	return ""
}

// audioOffer is the own voice transport's ticket for a joined client, or nil when it's off or failed.
// A client without one stays on WebRTC audio, so a failure here costs latency, not the call.
func (h *Handler) audioOffer(clientID, roomID string) *types.AudioTransportOffer {
	if h.audio == nil {
		return nil
	}
	o, err := h.audio.Offer(clientID, roomID)
	if err != nil {
		log.Printf("[AUDIO] no offer for %s: %v", clientID, err)
		return nil
	}
	offer := &types.AudioTransportOffer{URLs: o.URLs, CertHash: o.CertHash, Ticket: o.Ticket}
	if o.UDP != nil {
		offer.UDP = &types.AudioUDPOffer{Addrs: o.UDP.Addrs, Session: o.UDP.Session, Key: o.UDP.Key}
	}
	return offer
}
