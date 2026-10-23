package websocket

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"

	"sfu-v2/internal/audiotransport"
	"sfu-v2/internal/config"
	"sfu-v2/internal/metrics"
	"sfu-v2/internal/recovery"
	"sfu-v2/internal/room"
	"sfu-v2/internal/track"
	peerManager "sfu-v2/internal/webrtc"
	"sfu-v2/pkg/types"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// ErrServerFull is the capacity guardrail in handler_join.go, named so the close frame can
// be 1013 "try again later" rather than a flat 1011.
var ErrServerFull = errors.New("server full")

// ErrPeerUnresponsive is the read deadline in keepalive.go firing. Named for the same reason
// ErrServerFull is: "we gave up on you" and "we hit a bug" are different close codes.
var ErrPeerUnresponsive = errors.New("peer unresponsive")

// closeCodePingTimeout says the SFU hung up because nothing came back. A private-use code:
// none of the 1000-series means "you stopped answering", and reusing one merges the cases.
const closeCodePingTimeout = 4000

// isReadDeadline reports whether a read ended because the deadline armed in keepalive.go
// expired, rather than because the connection broke or the peer closed it.
func isReadDeadline(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}

	// Belt and braces for anything that reports a timeout without wrapping the sentinel.
	// Cheap, and the alternative is this case quietly reporting itself as "sfu error".
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// closeStatusFor picks the close code and reason for a connection that is ending. The
// reasons are fixed, since error text here carries token detail, and 1006 is never returned.
func closeStatusFor(err error) (code int, reason string) {
	switch {
	case err == nil:
		return websocket.CloseNormalClosure, ""
	case errors.Is(err, ErrServerFull):
		return websocket.CloseTryAgainLater, "server full"
	case errors.Is(err, ErrPeerUnresponsive):
		return closeCodePingTimeout, "ping timeout"
	default:
		return websocket.CloseInternalServerErr, "sfu error"
	}
}

// Coordinator interface to avoid circular imports
type Coordinator interface {
	SignalPeerConnectionsInRoom(roomID string)
	OnTrackAddedToRoom(roomID string)
	OnTrackRemovedFromRoom(roomID string)
}

// Handler manages WebSocket connections and integrates with other components
// AudioTransport hands joined clients a ticket for the own voice transport (GRYT-1818).
type AudioTransport interface {
	Offer(peerID, roomID string) (audiotransport.Offer, error)
	Drop(peerID string)
	OnSenders(fn func(roomID string, senders map[string]uint32))
	Senders(roomID string) map[string]uint32
}

type Handler struct {
	audio         AudioTransport
	config        *config.Config
	webrtcAPI     *webrtc.API
	trackManager  *track.Manager
	webrtcManager *peerManager.Manager
	roomManager   *room.Manager
	coordinator   Coordinator
	live          livePeers
}

// NewHandler creates a new WebSocket handler
// SetAudioTransport turns on the own voice transport's offers; unset, room_joined carries none.
func (h *Handler) SetAudioTransport(a AudioTransport) {
	h.audio = a
	a.OnSenders(h.sendAudioSenders)
}

func NewHandler(cfg *config.Config, webrtcAPI *webrtc.API, trackManager *track.Manager, webrtcManager *peerManager.Manager, roomManager *room.Manager, coordinator Coordinator) *Handler {
	return &Handler{
		config:        cfg,
		webrtcAPI:     webrtcAPI,
		trackManager:  trackManager,
		webrtcManager: webrtcManager,
		roomManager:   roomManager,
		coordinator:   coordinator,
	}
}

// debugLog logs debug messages if debug mode is enabled
func (h *Handler) debugLog(format string, args ...interface{}) {
	if h.config.Debug {
		log.Printf("[WEBSOCKET] "+format, args...)
	}
}

// generateClientID generates a unique client ID
func generateClientID() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

// HandleWebSocket handles incoming WebSocket connections
func (h *Handler) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	recovery.SafeExecuteWithContext("WEBSOCKET", "HANDLE_CONNECTION", "", "", r.RemoteAddr, func() error {
		unsafeConn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			if h.config.Debug {
				h.debugLog("❌ WebSocket upgrade error: %v", err)
			}
			return err
		}

		metrics.WebSocketConnections.Inc()
		safeConn := NewThreadSafeWriter(unsafeConn)

		// Started here rather than inside either handler, so it covers the join handshake:
		// a socket that never sends client_join used to sit in a blocking read forever.
		keepAlive := StartKeepAlive(safeConn, h.config.PingInterval, h.config.PongTimeout)
		defer keepAlive.Stop()

		// Why this connection ended, so the close frame below can say. Set by the handler
		// that owns the connection, read by the deferred close after it returns.
		var handlerErr error

		defer func() {
			metrics.WebSocketConnections.Dec()
			recovery.SafeExecute("WEBSOCKET", "CLOSE_CONNECTION", func() error {
				code, reason := closeStatusFor(handlerErr)
				safeConn.CloseWithReason(code, reason)
				return nil
			})
		}()

		clientID := generateClientID()
		parsedURL, _ := url.Parse(r.RequestURI)

		h.debugLog("🔌 New WebSocket connection: %s (Path: %s, RemoteAddr: %s)", clientID, parsedURL.Path, r.RemoteAddr)

		switch parsedURL.Path {
		case "/server":
			h.debugLog("🖥️  Handling server connection: %s", clientID)
			handlerErr = h.handleServerConnection(safeConn, clientID)
		case "/client":
			h.debugLog("👤 Handling client connection: %s", clientID)
			handlerErr = h.handleClientConnection(safeConn, clientID, r)
		default:
			h.debugLog("👤 Handling default client connection: %s", clientID)
			handlerErr = h.handleClientConnection(safeConn, clientID, r)
		}

		// Translated in one place rather than in each of the three read loops: a read that
		// ended on the deadline would otherwise be reported as an SFU bug.
		if isReadDeadline(handlerErr) {
			metrics.PingTimeouts.Inc()
			h.debugLog("💀 No word from %s in %s — hanging up", clientID, h.config.PongTimeout)
			handlerErr = fmt.Errorf("%w after %s", ErrPeerUnresponsive, h.config.PongTimeout)
		}

		return handlerErr
	})
}

// handleServerConnection handles server registration connections
func (h *Handler) handleServerConnection(conn *ThreadSafeWriter, clientID string) error {
	return recovery.SafeExecuteWithContext("WEBSOCKET", "HANDLE_SERVER", clientID, "", "Server connection handling", func() error {
		h.debugLog("🖥️  Server connection established: %s", clientID)

		for {
			var raw []byte
			var err error

			err = recovery.SafeExecuteWithContext("WEBSOCKET", "READ_SERVER_MESSAGE", clientID, "", "Reading server message", func() error {
				_, raw, err = conn.ReadMessage()
				return err
			})

			if err != nil {
				h.debugLog("❌ Error reading server message from %s: %v", clientID, err)
				return err
			}

			message := &types.WebSocketMessage{}
			if err := recovery.SafeJSONUnmarshal(raw, &message); err != nil {
				h.debugLog("❌ Error unmarshalling server message from %s: %v", clientID, err)
				continue
			}

			h.debugLog("📨 Server message from %s: event=%s", clientID, message.Event)

			err = recovery.SafeExecuteWithContext("WEBSOCKET", "PROCESS_SERVER_MESSAGE", clientID, "", message.Event, func() error {
				switch message.Event {
				case types.EventServerRegister:
					return h.handleServerRegistration(conn, clientID, message.Data)
				case types.EventDisconnectUser:
					return h.handleDisconnectUser(message.Data)
				case types.EventUserAudioControl:
					return h.handleUserAudioControl(message.Data)
				case types.EventUserHiddenPeers:
					return h.handleUserHiddenPeers(message.Data)
				case types.EventUserCapabilities:
					return h.handleUserCapabilities(message.Data)
				case types.EventSyncRequest:
					return h.handleSyncRequest(conn, message.Data)
				case types.EventKeepAlive:
					if h.config.Debug {
						h.debugLog("💓 Keep-alive received from server %s", clientID)
					}
					return nil
				default:
					h.debugLog("❓ Unknown server event from %s: %s", clientID, message.Event)
					return nil
				}
			})

			if err != nil {
				h.debugLog("❌ Error processing server message from %s: %v", clientID, err)
			}
		}
	})
}

// handleServerRegistration processes server registration
func (h *Handler) handleServerRegistration(conn *ThreadSafeWriter, clientID, data string) error {
	var regData types.ServerRegistrationData
	if err := recovery.SafeJSONUnmarshal([]byte(data), &regData); err != nil {
		h.debugLog("❌ Error unmarshalling server registration data from %s: %v", clientID, err)
		h.sendErrorToConnection(conn, "Invalid registration data")
		return err
	}

	h.debugLog("🖥️  Server registration attempt: ServerID=%s, RoomID=%s", regData.ServerID, regData.RoomID)

	if err := h.roomManager.RegisterServer(regData.ServerID, regData.ServerPassword, regData.RoomID); err != nil {
		h.debugLog("❌ Server registration failed for %s: %v", regData.ServerID, err)
		h.sendErrorToConnection(conn, "Registration failed: "+err.Error())
		return err
	}

	// Store the server connection so peer join/leave notifications can be sent back.
	h.roomManager.SetServerConnection(regData.ServerID, conn)

	h.debugLog("✅ Server %s registered room %s successfully", regData.ServerID, regData.RoomID)
	h.sendSuccessToConnection(conn, "Server registered successfully")
	return nil
}

// handleDisconnectUser processes a server request to force-disconnect a user.
func (h *Handler) handleDisconnectUser(data string) error {
	var req types.DisconnectUserData
	if err := recovery.SafeJSONUnmarshal([]byte(data), &req); err != nil {
		h.debugLog("❌ Error unmarshalling disconnect_user data: %v", err)
		return err
	}

	if !h.roomManager.ValidateServerCredentials(req.ServerID, req.ServerPassword) {
		h.debugLog("❌ disconnect_user: invalid credentials for server '%s'", req.ServerID)
		return nil
	}

	h.debugLog("🔌 disconnect_user: server=%s room=%s user=%s", req.ServerID, req.RoomID, req.UserID)
	if err := h.roomManager.DisconnectUser(req.RoomID, req.UserID); err != nil {
		h.debugLog("❌ disconnect_user failed: %v", err)
	}
	return nil
}

// handleUserAudioControl processes a server request to update a user's mute/deafen state.
func (h *Handler) handleUserAudioControl(data string) error {
	var req types.AudioControlData
	if err := recovery.SafeJSONUnmarshal([]byte(data), &req); err != nil {
		h.debugLog("❌ Error unmarshalling user_audio_control data: %v", err)
		return err
	}

	if !h.roomManager.ValidateServerCredentials(req.ServerID, req.ServerPassword) {
		h.debugLog("❌ user_audio_control: invalid credentials for server '%s'", req.ServerID)
		return nil
	}

	h.debugLog("🔇 user_audio_control: server=%s room=%s user=%s muted=%v deafened=%v",
		req.ServerID, req.RoomID, req.UserID, req.IsMuted, req.IsDeafened)

	if err := h.roomManager.SetUserDeafened(req.RoomID, req.UserID, req.IsDeafened); err != nil {
		h.debugLog("❌ user_audio_control: failed to set deafen state: %v", err)
		return nil
	}

	go h.coordinator.SignalPeerConnectionsInRoom(req.RoomID)
	return nil
}

// handleUserHiddenPeers processes a server request to stop forwarding some users' media to a user.
func (h *Handler) handleUserHiddenPeers(data string) error {
	var req types.HiddenPeersData
	if err := recovery.SafeJSONUnmarshal([]byte(data), &req); err != nil {
		h.debugLog("❌ Error unmarshalling user_hidden_peers data: %v", err)
		return err
	}

	if !h.roomManager.ValidateServerCredentials(req.ServerID, req.ServerPassword) {
		h.debugLog("❌ user_hidden_peers: invalid credentials for server '%s'", req.ServerID)
		return nil
	}

	if err := h.roomManager.SetHiddenPeers(req.RoomID, req.UserID, req.Hidden); err != nil {
		h.debugLog("❌ user_hidden_peers: %v", err)
		return nil
	}

	go h.coordinator.SignalPeerConnectionsInRoom(req.RoomID)
	return nil
}

// handleSyncRequest responds with all connected peers for the requesting server.
func (h *Handler) handleSyncRequest(conn *ThreadSafeWriter, data string) error {
	var req types.SyncRequestData
	if err := recovery.SafeJSONUnmarshal([]byte(data), &req); err != nil {
		h.debugLog("❌ Error unmarshalling sync_request data: %v", err)
		return err
	}

	if !h.roomManager.ValidateServerCredentials(req.ServerID, req.ServerPassword) {
		h.debugLog("❌ sync_request: invalid credentials for server '%s'", req.ServerID)
		return nil
	}

	rooms := h.roomManager.GetRoomPeersForServer(req.ServerID)
	payload := types.SyncResponseData{Rooms: rooms}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	h.debugLog("📡 sync_response: server=%s rooms=%d", req.ServerID, len(rooms))
	return conn.WriteJSON(&types.WebSocketMessage{
		Event: types.EventSyncResponse,
		Data:  string(payloadBytes),
	})
}

// handleClientMessages processes incoming WebSocket messages from clients
func (h *Handler) handleClientMessages(conn *ThreadSafeWriter, peerConnection *webrtc.PeerConnection, roomID, clientID string) error {
	return recovery.SafeExecuteWithContext("WEBSOCKET", "HANDLE_CLIENT_MESSAGES", clientID, roomID, "Processing client messages", func() error {
		h.debugLog("📨 Starting message handling for client %s in room '%s'", clientID, roomID)

		message := &types.WebSocketMessage{}
		messageCount := 0
		pendingRenegotiate := false

		// When this connection last said it was still here. Per-connection, so it costs a
		// comparison rather than the room manager's lock, which still_here would serialise.
		var lastStillHere time.Time

		for {
			var raw []byte
			var err error

			err = recovery.SafeExecuteWithContext("WEBSOCKET", "READ_CLIENT_MESSAGE", clientID, roomID, "Reading client message", func() error {
				_, raw, err = conn.ReadMessage()
				return err
			})

			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
					h.debugLog("🔌 WebSocket closed normally for %s: %v", clientID, err)
					break
				}

				h.debugLog("❌ Error reading WebSocket message from %s: %v", clientID, err)
				return err
			}

			messageCount++

			if err := recovery.SafeJSONUnmarshal(raw, &message); err != nil {
				h.debugLog("❌ Error unmarshalling WebSocket message from %s: %v", clientID, err)
				continue
			}

			h.debugLog("📨 Message #%d from %s in room '%s': event=%s", messageCount, clientID, roomID, message.Event)

			err = recovery.SafeExecuteWithContext("WEBSOCKET", "PROCESS_CLIENT_MESSAGE", clientID, roomID, message.Event, func() error {
				switch message.Event {
				case types.EventCandidate:
					return h.handleICECandidate(peerConnection, message.Data, clientID)
				case types.EventAnswer:
					if answerErr := h.handleAnswer(peerConnection, message.Data, clientID); answerErr != nil {
						return answerErr
					}
					// The peer is stable again, so anything it asked for gets its turn now —
					// before the room re-signal, which used to take it back out of stable.
					if pendingRenegotiate {
						h.debugLog("🔄 Executing deferred renegotiation for %s", clientID)
						pendingRenegotiate = h.renegotiateOrDefer(peerConnection, conn, clientID, roomID)
					}
					// Distribute any tracks that arrived while this peer was in
					// have-local-offer.
					go h.coordinator.SignalPeerConnectionsInRoom(roomID)
					return nil
				case types.EventRenegotiate:
					pendingRenegotiate = h.renegotiateOrDefer(peerConnection, conn, clientID, roomID)
					return nil
				case types.EventSetLayer:
					return h.handleSetLayer(message.Data, clientID, roomID)
				case types.EventVideoDemand:
					return h.handleVideoDemand(message.Data, clientID, roomID, peerConnection)
				case types.EventStillHere:
					now := time.Now()
					if now.Sub(lastStillHere) < stillHereMinInterval {
						h.debugLog("🙋 Ignoring still_here from %s: %s since the last one", clientID, now.Sub(lastStillHere).Round(time.Millisecond))
						return nil
					}
					lastStillHere = now
					if h.roomManager.StillHere(roomID) {
						h.debugLog("🙋 %s says they are still in call '%s'", clientID, roomID)
					}
					return nil
				case types.EventKeepAlive:
					if h.config.Debug {
						h.debugLog("💓 Keep-alive received from %s", clientID)
					}
					return nil
				default:
					h.debugLog("❓ Unknown message event from %s: %s", clientID, message.Event)
					return nil
				}
			})

			if err != nil {
				h.debugLog("❌ Error processing message from %s: %v", clientID, err)
			}
		}

		h.debugLog("📨 Message handling ended for client %s (Total messages: %d)", clientID, messageCount)
		return nil
	})
}

// handleICECandidate processes ICE candidate messages
func (h *Handler) handleICECandidate(peerConnection *webrtc.PeerConnection, data, clientID string) error {
	candidate := webrtc.ICECandidateInit{}
	if err := recovery.SafeJSONUnmarshal([]byte(data), &candidate); err != nil {
		h.debugLog("❌ Error unmarshalling ICE candidate from %s: %v", clientID, err)
		return err
	}

	h.debugLog("🔧 Adding ICE candidate from %s", clientID)
	if err := peerConnection.AddICECandidate(candidate); err != nil {
		h.debugLog("❌ Error adding ICE candidate from %s: %v", clientID, err)
		return err
	}
	return nil
}

// handleAnswer applies an answer, leaving the peer's signaling state stable. The caller
// re-signals the room afterwards, once anything the peer is owed has had its turn.
func (h *Handler) handleAnswer(peerConnection *webrtc.PeerConnection, data, clientID string) error {
	answer := webrtc.SessionDescription{}
	if err := recovery.SafeJSONUnmarshal([]byte(data), &answer); err != nil {
		h.debugLog("❌ Error unmarshalling answer from %s: %v", clientID, err)
		return err
	}

	h.debugLog("🔄 Setting remote description (answer) from %s", clientID)
	if err := peerConnection.SetRemoteDescription(answer); err != nil {
		h.debugLog("❌ Error setting remote description from %s: %v", clientID, err)
		return err
	}

	return nil
}

// renegotiateOrDefer renegotiates now if the peer can, and otherwise records that it is owed
// one. A client adds a track and asks once, so a dropped request stranded it (GRYT-32).
func (h *Handler) renegotiateOrDefer(peerConnection *webrtc.PeerConnection, conn *ThreadSafeWriter, clientID, roomID string) (stillPending bool) {
	if peerConnection.SignalingState() != webrtc.SignalingStateStable {
		h.debugLog("⏳ Deferring renegotiate for %s: signaling state=%s (retried after next answer)", clientID, peerConnection.SignalingState().String())
		return true
	}

	if err := h.handleRenegotiate(peerConnection, conn, clientID, roomID); err != nil {
		h.debugLog("❌ Renegotiation for %s failed, keeping it pending: %v", clientID, err)
		return true
	}

	return false
}

// handleRenegotiate creates a fresh offer so the client can include newly added tracks in
// its answer. Callers go through renegotiateOrDefer, so a failed attempt is not forgotten.
func (h *Handler) handleRenegotiate(peerConnection *webrtc.PeerConnection, conn *ThreadSafeWriter, clientID, roomID string) error {
	if peerConnection.SignalingState() != webrtc.SignalingStateStable {
		return fmt.Errorf("peer %s not stable (%s)", clientID, peerConnection.SignalingState().String())
	}

	h.debugLog("🔄 Renegotiation requested by client %s in room '%s'", clientID, roomID)

	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		h.debugLog("❌ Error creating renegotiation offer for %s: %v", clientID, err)
		return err
	}

	if err := peerConnection.SetLocalDescription(offer); err != nil {
		h.debugLog("❌ Error setting local description for renegotiation (%s): %v", clientID, err)
		return err
	}

	offerJSON, marshalErr := recovery.SafeJSONMarshal(offer)
	if marshalErr != nil {
		h.debugLog("❌ Error marshalling renegotiation offer for %s: %v", clientID, marshalErr)
		return marshalErr
	}

	h.debugLog("📤 Sending renegotiation offer to %s (%d bytes SDP)", clientID, len(offer.SDP))
	return conn.WriteJSON(&types.WebSocketMessage{
		Event: types.EventOffer,
		Data:  string(offerJSON),
	})
}

// handleSetLayer processes a set_layer message to manually set the max temporal
// layer a receiver subscribes to for a given track.
func (h *Handler) handleSetLayer(data, clientID, roomID string) error {
	var req types.SetLayerData
	if err := recovery.SafeJSONUnmarshal([]byte(data), &req); err != nil {
		h.debugLog("❌ Error unmarshalling set_layer data from %s: %v", clientID, err)
		return nil
	}

	lf, ok := h.trackManager.GetForwarder(roomID, req.TrackID)
	if !ok {
		h.debugLog("⚠️ set_layer: no forwarder for track %s in room %s", req.TrackID, roomID)
		return nil
	}

	lf.SetMaxTemporalLayer(clientID, req.MaxTemporalLayer)
	h.debugLog("📊 set_layer: client %s track %s → maxTemporal=%d", clientID, req.TrackID, req.MaxTemporalLayer)
	return nil
}

// sendErrorToConnection sends an error message to a WebSocket connection
func (h *Handler) sendErrorToConnection(conn *ThreadSafeWriter, errorMsg string) {
	recovery.SafeExecute("WEBSOCKET", "SEND_ERROR", func() error {
		h.debugLog("❌ Sending error: %s", errorMsg)
		return conn.WriteJSON(&types.WebSocketMessage{
			Event: types.EventRoomError,
			Data:  errorMsg,
		})
	})
}

// stillHereMinInterval is how often one connection may say it is still here: far below
// anything a person can press and far above what a loop costs to ignore.
const stillHereMinInterval = time.Second

// sendRoomJoined tells a client it is in, and what this SFU's call timeout is. Neither side
// needs the other to move first: an older client reads the JSON as an opaque string.
func (h *Handler) sendRoomJoined(conn *ThreadSafeWriter, message string, audio *types.AudioTransportOffer) {
	recovery.SafeExecute("WEBSOCKET", "SEND_ROOM_JOINED", func() error {
		payload, err := json.Marshal(types.RoomJoinedData{
			Message:                 message,
			CallAloneTimeoutSeconds: int(h.config.CallAloneTimeout / time.Second),
			MaxIngestKbps:           h.config.MaxIngestKbps,
			AudioTransport:          audio,
		})
		if err != nil {
			return err
		}

		h.debugLog("✅ Sending room_joined: %s", string(payload))
		return conn.WriteJSON(&types.WebSocketMessage{
			Event: types.EventRoomJoined,
			Data:  string(payload),
		})
	})
}

// sendSuccessToConnection sends a success message to a WebSocket connection
func (h *Handler) sendSuccessToConnection(conn *ThreadSafeWriter, successMsg string) {
	recovery.SafeExecute("WEBSOCKET", "SEND_SUCCESS", func() error {
		h.debugLog("✅ Sending success: %s", successMsg)
		return conn.WriteJSON(&types.WebSocketMessage{
			Event: types.EventRoomJoined,
			Data:  successMsg,
		})
	})
}
