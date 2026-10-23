package types

// WebSocketMessage represents the structure for WebSocket messages
type WebSocketMessage struct {
	Event string `json:"event"`
	Data  string `json:"data"`
}

// ServerRegistrationData represents server registration information
type ServerRegistrationData struct {
	ServerID       string `json:"server_id"`
	ServerPassword string `json:"server_password"`
	RoomID         string `json:"room_id"`
}

// ClientJoinData represents client join information
type ClientJoinData struct {
	RoomID         string `json:"room_id"`
	ServerID       string `json:"server_id"`
	ServerPassword string `json:"server_password"`
	UserToken      string `json:"user_token"`
	UserID         string `json:"user_id"`
}

// DisconnectUserData is sent by the server to force-disconnect a user from a room.
type DisconnectUserData struct {
	RoomID         string `json:"room_id"`
	UserID         string `json:"user_id"`
	ServerID       string `json:"server_id"`
	ServerPassword string `json:"server_password"`
}

// PeerEventData is sent from SFU to server when a peer joins or leaves.
type PeerEventData struct {
	RoomID string `json:"room_id"`
	UserID string `json:"user_id"`
}

// RoomPeers describes connected users in a single room (used in sync responses).
type RoomPeers struct {
	RoomID  string   `json:"room_id"`
	UserIDs []string `json:"user_ids"`
}

// SyncResponseData is the payload for a sync_response from SFU to server.
type SyncResponseData struct {
	Rooms []RoomPeers `json:"rooms"`
}

// SyncRequestData is sent by the server to request the current peer state.
type SyncRequestData struct {
	ServerID       string `json:"server_id"`
	ServerPassword string `json:"server_password"`
}

// AudioControlData is sent by the server to update a user's mute/deafen state.
type AudioControlData struct {
	RoomID         string `json:"room_id"`
	UserID         string `json:"user_id"`
	ServerID       string `json:"server_id"`
	ServerPassword string `json:"server_password"`
	IsMuted        bool   `json:"is_muted"`
	IsDeafened     bool   `json:"is_deafened"`
}

// HiddenPeersData is sent by the server with the users whose media UserID must not get:
// the people they blocked. It replaces the last list sent for them in that room.
type HiddenPeersData struct {
	RoomID         string   `json:"room_id"`
	UserID         string   `json:"user_id"`
	ServerID       string   `json:"server_id"`
	ServerPassword string   `json:"server_password"`
	Hidden         []string `json:"hidden"`
}

// UserCapabilitiesData replaces what a connected member may send, as the server works it
// out now. It is the join token's capability list, sent again after a permission change.
type UserCapabilitiesData struct {
	RoomID         string   `json:"room_id"`
	UserID         string   `json:"user_id"`
	ServerID       string   `json:"server_id"`
	ServerPassword string   `json:"server_password"`
	Capabilities   []string `json:"capabilities"`
}

// SetLayerData lets a client manually set the max temporal layer for a track.
type SetLayerData struct {
	TrackID          string `json:"track_id"`
	MaxTemporalLayer int    `json:"max_temporal_layer"` // -1 = all layers, 0 = T0 only, 1 = T0+T1, 2 = T0+T1+T2
}

// VideoDemandData is a viewer saying how big it draws the video of one stream, in device
// pixels, and its display's refresh rate. Zero width or height means nobody can see it.
type VideoDemandData struct {
	StreamID string `json:"stream_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FPS      int    `json:"fps"`
}

// VideoWantedData tells a sender the most anybody wants of the stream on its m-line mid.
// Unknown counts viewers that never reported, which the sender treats as full size.
type VideoWantedData struct {
	Mid      string `json:"mid"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FPS      int    `json:"fps"`
	Watchers int    `json:"watchers"`
	Unknown  int    `json:"unknown"`
}

// AudioSendersData maps each own-transport sender id in a room, as a decimal string, to the user
// the join token proved it is, so a packet's speaking flag can light the right ring (GRYT-1830).
type AudioSendersData struct {
	Senders map[string]string `json:"senders"`
}

// WebRTCAudioData says whether anyone in a room is off the own transport, and so still needs
// everyone's WebRTC audio (GRYT-1853). False lets a client on the transport stop sending its copy.
type WebRTCAudioData struct {
	Wanted bool `json:"wanted"`
}

// RoomJoinedData is the payload of the room_joined a client gets on joining. The server
// connection gets a plain string on the same event: only a client has a countdown to draw.
type RoomJoinedData struct {
	Message string `json:"message"`

	// CallAloneTimeoutSeconds is SFU_CALL_ALONE_TIMEOUT in seconds, zero meaning the sweep is
	// off. Sent so the client stops carrying its own copy of a default an operator can change.
	CallAloneTimeoutSeconds int `json:"call_alone_timeout_seconds"`

	// MaxIngestKbps is SFU_MAX_INGEST_KBPS. The client keeps its uplink under it; the SFU doesn't enforce it.
	MaxIngestKbps int `json:"max_ingest_kbps,omitempty"`

	// AudioTransport opens the own voice transport (GRYT-1818); absent when it's off. Older clients ignore it.
	AudioTransport *AudioTransportOffer `json:"audio_transport,omitempty"`
}

// AudioTransportOffer is where and how a client opens its voice session: the cert it pins, and a one-time ticket.
type AudioTransportOffer struct {
	URLs     []string `json:"urls"`
	CertHash string   `json:"certHash"`
	Ticket   string   `json:"ticket"`
	// The native engine's plain UDP, when AUDIO_UDP_PORT is set (GRYT-1822).
	UDP *AudioUDPOffer `json:"udp,omitempty"`
}

// AudioUDPOffer is a session id and its key; a packet that opens with the key is the only proof needed.
type AudioUDPOffer struct {
	Addrs   []string `json:"addrs"`
	Session uint32   `json:"session"`
	Key     string   `json:"key"`
}

// Supported WebSocket message events
const (
	EventOffer            = "offer"
	EventAnswer           = "answer"
	EventCandidate        = "candidate"
	EventServerRegister   = "server_register"
	EventClientJoin       = "client_join"
	EventRoomJoined       = "room_joined"
	EventRoomError        = "room_error"
	EventKeepAlive        = "keep_alive"
	EventDisconnectUser   = "disconnect_user"
	EventUserAudioControl = "user_audio_control"
	EventUserHiddenPeers  = "user_hidden_peers"
	EventUserCapabilities = "user_capabilities"
	EventPeerJoined       = "peer_joined"
	EventPeerLeft         = "peer_left"
	EventRenegotiate      = "renegotiate"
	EventSyncRequest      = "sync_request"
	EventSyncResponse     = "sync_response"
	EventSetLayer         = "set_layer"
	EventStillHere        = "still_here"
	EventVideoDemand      = "video_demand"
	EventVideoWanted      = "video_wanted"
	EventAudioSenders     = "audio_senders"
	EventWebRTCAudio      = "webrtc_audio_wanted"
)
