package config

import (
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/pion/webrtc/v4"
)

const (
	// DefaultICEUDPMuxPort is where media goes when nothing says otherwise.
	// 3478 needs no privileged bind and is already open on hosts that run STUN.
	DefaultICEUDPMuxPort = 3478
	// Its own port, so voice's QUIC never shares WebRTC's socket (decided 2026-10-20).
	DefaultAudioTransportPort = 4433
	// DefaultMaxPeers is a guardrail, not a property of the transport.
	DefaultMaxPeers = 200

	// DefaultPingInterval is how often the SFU pokes a quiet connection. It doubles as the
	// only server-to-client traffic in a call, which keeps a NAT mapping from being reaped.
	DefaultPingInterval = 30 * time.Second

	// DefaultPongTimeout is how long a peer may say nothing before the SFU gives up. Three
	// ping intervals, so one lost pong or a late one is not enough.
	DefaultPongTimeout = 90 * time.Second

	// DefaultCallAloneTimeout is how long one person may be alone in a call before the SFU
	// ends it. Calls only: a voice channel is a place, and calls.go tells the two apart.
	DefaultCallAloneTimeout = 2 * time.Minute

	// DefaultCallSweepInterval is how often that is checked, which bounds the error on the
	// timeout above and is the whole cost when nobody is in a call.
	DefaultCallSweepInterval = 15 * time.Second
)

// Config holds the application configuration
type Config struct {
	Port        string
	STUNServers []string
	ICEServers  []webrtc.ICEServer
	Debug       bool
	VerboseLog  bool

	// WebRTC / ICE networking. Every participant's media shares one UDP port,
	// so this is the only port that has to be open for voice to work.
	ICEUDPMuxPort   int
	ICEAdvertiseIPs []string
	DisableSTUN     bool

	// RequireClientToken refuses the legacy shared-password join path. See
	// internal/auth: until this is on, knowing the old password is still enough.
	RequireClientToken bool

	// Where Prometheus metrics are served. Deliberately not the main port; see
	// cmd/sfu/main.go. Zero switches serving them off entirely.
	MetricsPort int

	// The address MetricsPort binds. Empty is every interface, which Prometheus in another
	// container needs; the desktop app sets 127.0.0.1, since nothing off the machine reads them.
	MetricsHost string

	// Where servers register. Deliberately not the main port: the main one is
	// published so clients can reach it, and registration must not ride along.
	ControlPort int

	// The address ControlPort binds. Empty is every interface; the desktop app sets
	// 127.0.0.1, because a server on another machine has no business registering there.
	ControlHost string

	// Capacity guardrail. Nothing to do with ports any more: one muxed port
	// carries far more peers than a machine has CPU and upload for.
	MaxPeers int

	// Liveness. The SFU pings every PingInterval and gives up after PongTimeout;
	// keepalive.go has the reasoning, and a PingInterval of zero turns both off.
	PingInterval time.Duration
	PongTimeout  time.Duration

	// How long somebody may be alone in a call before it ends. Zero turns it
	// off and leaves the room up until the peer goes on its own.
	CallAloneTimeout time.Duration

	// The most one peer should send, in kbps, told to clients in room_joined. Zero is no cap.
	MaxIngestKbps int

	// AudioTransportPort is the UDP port of the own voice transport (GRYT-1818). 0 turns it off.
	AudioTransportPort int
	// AudioUDPPort is the native engine's plain UDP listener (GRYT-1822), next to the transport. 0, the default, is off.
	AudioUDPPort int
}

// Load reads configuration from environment variables
func Load() (*Config, error) {
	if err := godotenv.Load("config.env"); err != nil {
		if err2 := godotenv.Load(); err2 != nil {
			log.Printf("Warning: No config.env or .env file found: %v", err2)
		}
	}

	port := os.Getenv("SFU_PORT")
	if port == "" {
		port = os.Getenv("PORT")
	}
	if port == "" {
		port = "5005"
	}

	stunServers := strings.Split(os.Getenv("STUN_SERVERS"), ",")
	if len(stunServers) == 1 && stunServers[0] == "" {
		// Default STUN servers if none provided
		stunServers = []string{"stun:stun.l.google.com:19302"}
	}

	// One UDP port for all media, 3478 unset: the IANA STUN port, and the one a locked-down
	// network most likely already allows. 443 is tempting and blocked on purpose by vendors.
	audioTransportPort := DefaultAudioTransportPort
	if raw := strings.TrimSpace(os.Getenv("AUDIO_TRANSPORT_PORT")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 || parsed > 65535 {
			return nil, fmt.Errorf("AUDIO_TRANSPORT_PORT=%q is not a UDP port (0 turns it off)", raw)
		}
		audioTransportPort = parsed
	}

	audioUDPPort := 0
	if raw := strings.TrimSpace(os.Getenv("AUDIO_UDP_PORT")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 || parsed > 65535 {
			return nil, fmt.Errorf("AUDIO_UDP_PORT=%q is not a UDP port (0 turns it off)", raw)
		}
		audioUDPPort = parsed
	}

	iceUDPMuxPort, _ := strconv.Atoi(os.Getenv("ICE_UDP_MUX_PORT"))
	if iceUDPMuxPort <= 0 || iceUDPMuxPort > 65535 {
		iceUDPMuxPort = DefaultICEUDPMuxPort
	}

	// Parsed before the STUN decision below, because a forced address decides DISABLE_STUN's
	// default. Entries are validated and dropped out loud: the failure is otherwise invisible.
	var iceAdvertiseIPs []string
	hasRoutableAdvertiseIP := false
	if raw := os.Getenv("ICE_ADVERTISE_IP"); raw != "" {
		for _, entry := range strings.Split(raw, ",") {
			t := strings.TrimSpace(entry)
			if t == "" {
				continue
			}
			parsed := net.ParseIP(t)
			if parsed == nil {
				log.Printf("Warning: ICE_ADVERTISE_IP entry %q is not an IP address; ignoring it", t)
				continue
			}
			iceAdvertiseIPs = append(iceAdvertiseIPs, t)
			if !(parsed.IsLoopback() || parsed.IsPrivate() || parsed.IsLinkLocalUnicast()) {
				hasRoutableAdvertiseIP = true
			}
		}
	}

	// A private address alongside a public one is the ordinary multi-network setup. A list
	// with no routable address at all is the real problem, and it fails just as quietly.
	if len(iceAdvertiseIPs) > 0 && !hasRoutableAdvertiseIP {
		log.Printf("Warning: no ICE_ADVERTISE_IP entry is routable from outside this network (%s); peers elsewhere will not be able to connect",
			strings.Join(iceAdvertiseIPs, ", "))
	}

	// Forcing an address turns STUN discovery off unless the operator says otherwise: when a
	// tunnel drops, discovery hands out the fallback route's address instead (GRYT-768).
	disableSTUN := len(iceAdvertiseIPs) > 0
	if raw := strings.TrimSpace(os.Getenv("DISABLE_STUN")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			log.Printf("Warning: DISABLE_STUN=%q is not a boolean; using %t", raw, disableSTUN)
		} else {
			disableSTUN = parsed
		}
	} else if disableSTUN {
		log.Printf("ICE_ADVERTISE_IP is set, so STUN discovery is off; set DISABLE_STUN=false to gather server-reflexive candidates as well")
	}

	iceServers := []webrtc.ICEServer{}
	if !disableSTUN {
		iceServers = []webrtc.ICEServer{
			{
				URLs: stunServers,
			},
		}
	}

	// A muxed port accepts many peers, so this is a guardrail rather than a
	// limit the transport imposes. What runs out first is CPU and upload.
	maxPeers, _ := strconv.Atoi(os.Getenv("MAX_PEERS"))
	if maxPeers <= 0 {
		maxPeers = DefaultMaxPeers
	}

	// Both in whole seconds. SFU_PING_INTERVAL=0 switches liveness checking off entirely,
	// read deadline included — the escape hatch if this starts hanging up on healthy peers.
	pingInterval := durationSecondsFromEnv("SFU_PING_INTERVAL", DefaultPingInterval)
	pongTimeout := durationSecondsFromEnv("SFU_PONG_TIMEOUT", DefaultPongTimeout)

	// Also whole seconds, and zero is off — a server owner who would rather a
	// call stayed up until somebody closed it says so without a rebuild.
	callAloneTimeout := durationSecondsFromEnv("SFU_CALL_ALONE_TIMEOUT", DefaultCallAloneTimeout)

	maxIngestKbps, _ := strconv.Atoi(os.Getenv("SFU_MAX_INGEST_KBPS"))
	if maxIngestKbps < 0 {
		maxIngestKbps = 0
	}

	// A timeout shorter than two ping intervals disconnects healthy peers, so it is raised
	// rather than refused: an SFU that will not boot over a small number is worse.
	if pingInterval > 0 && pongTimeout < 2*pingInterval {
		log.Printf("Warning: SFU_PONG_TIMEOUT (%s) is under two ping intervals (%s); using %s, the least that survives a single lost pong",
			pongTimeout, pingInterval, 2*pingInterval)
		pongTimeout = 2 * pingInterval
	}

	// On unless somebody says otherwise. Defaulting off left every deployment with the hole
	// open until an operator flipped a flag they had no reason to know about.
	requireClientToken := true
	if raw := strings.TrimSpace(os.Getenv("SFU_REQUIRE_CLIENT_TOKEN")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			log.Printf("Warning: SFU_REQUIRE_CLIENT_TOKEN=%q is not a boolean; requiring client tokens", raw)
		} else {
			requireClientToken = parsed
		}
	}

	metricsPort := 9091
	if raw := strings.TrimSpace(os.Getenv("SFU_METRICS_PORT")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 0 && parsed <= 65535 {
			metricsPort = parsed
		} else {
			log.Printf("Warning: SFU_METRICS_PORT=%q is not a port; using %d", raw, metricsPort)
		}
	}

	// 9092, beside metrics on 9091, because both are container-only. 50xx is where
	// published ports live and dev.yml already puts a host 5006 on the SFU.

	// No zero-disables, unlike metrics: an SFU nothing can register with is not an
	// SFU, so the only choice here is which port, never whether.
	controlPort := 9092
	if raw := strings.TrimSpace(os.Getenv("SFU_CONTROL_PORT")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 65535 {
			controlPort = parsed
		} else {
			log.Printf("Warning: SFU_CONTROL_PORT=%q is not a port; using %d", raw, controlPort)
		}
	}

	// Refused rather than warned about like the ports above: falling back to every
	// interface would open the port to exactly the network this was set to keep out.
	controlHost := strings.TrimSpace(os.Getenv("SFU_CONTROL_HOST"))
	if controlHost != "" && net.ParseIP(controlHost) == nil {
		return nil, fmt.Errorf("SFU_CONTROL_HOST=%q is not an IP address. Use 127.0.0.1 to take registration from this machine only, or leave it unset for every interface", controlHost)
	}

	// Refused for the same reason, even though a metrics port that fails to bind is only logged.
	metricsHost := strings.TrimSpace(os.Getenv("SFU_METRICS_HOST"))
	if metricsHost != "" && net.ParseIP(metricsHost) == nil {
		return nil, fmt.Errorf("SFU_METRICS_HOST=%q is not an IP address. Use 127.0.0.1 to serve metrics to this machine only, or leave it unset for every interface", metricsHost)
	}

	debug, _ := strconv.ParseBool(os.Getenv("DEBUG"))
	verboseLog, _ := strconv.ParseBool(os.Getenv("VERBOSE_LOG"))

	// Default to debug mode if not specified
	if os.Getenv("DEBUG") == "" {
		debug = true
	}

	return &Config{
		Port:               port,
		STUNServers:        stunServers,
		ICEServers:         iceServers,
		Debug:              debug,
		VerboseLog:         verboseLog,
		ICEUDPMuxPort:      iceUDPMuxPort,
		ICEAdvertiseIPs:    iceAdvertiseIPs,
		AudioTransportPort: audioTransportPort,
		AudioUDPPort:       audioUDPPort,
		DisableSTUN:        disableSTUN,
		RequireClientToken: requireClientToken,
		MetricsPort:        metricsPort,
		MetricsHost:        metricsHost,
		ControlPort:        controlPort,
		ControlHost:        controlHost,
		MaxPeers:           maxPeers,
		PingInterval:       pingInterval,
		PongTimeout:        pongTimeout,

		CallAloneTimeout: callAloneTimeout,
		MaxIngestKbps:    maxIngestKbps,
	}, nil
}

// durationSecondsFromEnv reads a whole number of seconds, keeping the default when unset or
// unreadable. Zero is a value — it turns liveness off — so it cannot go through Atoi.
func durationSecondsFromEnv(name string, fallback time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}

	seconds, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || seconds < 0 {
		log.Printf("Warning: ignoring %s=%q — want a whole number of seconds; using %s", name, raw, fallback)
		return fallback
	}

	return time.Duration(seconds) * time.Second
}
