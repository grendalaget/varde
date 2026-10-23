package embed

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pion/ice/v4"
	pion "github.com/pion/webrtc/v4"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"sfu-v2/internal/audiotransport"
	"sfu-v2/internal/config"
	"sfu-v2/internal/metrics"
	"sfu-v2/internal/readiness"
	"sfu-v2/internal/recovery"
	"sfu-v2/internal/room"
	"sfu-v2/internal/signaling"
	"sfu-v2/internal/track"
	webrtcmanager "sfu-v2/internal/webrtc"
	"sfu-v2/internal/websocket"
)

var Version = "dev"

func Run(ctx context.Context) (runErr error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("🚨 FATAL PANIC in SFU: %v", r)
			recovery.GetLogger().DumpRecentActions()
			runErr = fmt.Errorf("server crashed with panic: %v", r)
		}
	}()

	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Initialize recovery system
	logger := recovery.GetLogger()
	logger.LogAction("MAIN", "STARTUP", "", "", "SFU Server starting")

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		logger.LogAction("MAIN", "CONFIG_ERROR", "", "", err.Error())
		return fmt.Errorf("load configuration: %w", err)
	}

	// Bound before anything is served. The public port names this one to servers, so an SFU
	// that couldn't take it would send them, password and all, to whoever holds it.
	controlAddr := net.JoinHostPort(cfg.ControlHost, strconv.Itoa(cfg.ControlPort))
	controlListener, err := net.Listen("tcp", controlAddr)
	if err != nil {
		logger.LogAction("MAIN", "CONTROL_PORT_TAKEN", "", "", err.Error())
		hint := "Set SFU_CONTROL_PORT to a free port"
		if cfg.ControlHost != "" {
			hint += ", or SFU_CONTROL_HOST to an address this machine has"
		}
		return fmt.Errorf("cannot take server registration on %s: %w. %s", controlAddr, err, hint)
	}
	defer controlListener.Close()

	// Log startup information
	banner := fmt.Sprintf("Gryt SFU v%s", Version)
	border := strings.Repeat("─", len(banner)+4)
	log.Printf("┌%s┐", border)
	log.Printf("│  %s  │", banner)
	log.Printf("└%s┘", border)
	log.Printf("📊 Configuration: Port=%s, Debug=%t, VerboseLog=%t", cfg.Port, cfg.Debug, cfg.VerboseLog)
	log.Printf("🧊 ICE Servers: %v", cfg.STUNServers)

	if cfg.Debug {
		log.Printf("🔍 Debug mode enabled - detailed logging active")
	}

	if cfg.VerboseLog {
		log.Printf("📝 Verbose logging enabled - RTP packet logging active")
	}

	// Start system monitoring
	recovery.StartSystemMonitor(ctx, 30*time.Second)
	logger.LogAction("MAIN", "MONITOR_STARTED", "", "", "System monitoring active")

	// Initialize managers with crash protection
	log.Printf("🏗️  Initializing components...")

	var trackManager *track.Manager
	var webrtcManager *webrtcmanager.Manager
	var roomManager *room.Manager
	var coordinator *signaling.Coordinator
	var webrtcAPI *pion.API

	// Initialize track manager with recovery
	err = recovery.SafeExecute("MAIN", "INIT_TRACK_MANAGER", func() error {
		trackManager = track.NewManager(cfg.Debug)
		log.Printf("✅ Track manager initialized (debug: %t)", cfg.Debug)
		return nil
	})
	if err != nil {
		return fmt.Errorf("initialize track manager: %w", err)
	}

	// Initialize WebRTC manager with recovery
	err = recovery.SafeExecute("MAIN", "INIT_WEBRTC_MANAGER", func() error {
		webrtcManager = webrtcmanager.NewManager(cfg.Debug)
		log.Printf("✅ WebRTC manager initialized (debug: %t)", cfg.Debug)
		return nil
	})
	if err != nil {
		return fmt.Errorf("initialize WebRTC manager: %w", err)
	}

	// Build a Pion WebRTC API with a configured SettingEngine (UDP port range, advertised IP, etc.)
	err = recovery.SafeExecute("MAIN", "INIT_WEBRTC_API", func() error {
		se, seErr := newSettingEngine(cfg)
		if seErr != nil {
			return seErr
		}
		me := &pion.MediaEngine{}
		if err := registerCodecs(me); err != nil {
			return fmt.Errorf("failed to register codecs: %w", err)
		}
		webrtcAPI = pion.NewAPI(pion.WithSettingEngine(se), pion.WithMediaEngine(me))
		return nil
	})
	if err != nil {
		return fmt.Errorf("initialize WebRTC API: %w", err)
	}

	// Verify UDP connectivity before reporting healthy. This gates /health so Compose waits
	// for the networking stack before starting dependent services.
	readiness.VerifyUDP(cfg.STUNServers, cfg.DisableSTUN)

	// Initialize room manager with recovery
	err = recovery.SafeExecute("MAIN", "INIT_ROOM_MANAGER", func() error {
		roomManager = room.NewManager(cfg.Debug)
		roomManager.SetRequireClientToken(cfg.RequireClientToken)
		if cfg.RequireClientToken {
			log.Printf("🔒 Client tokens required; the legacy shared-password join path is off")
		} else {
			log.Printf("⚠️  SFU_REQUIRE_CLIENT_TOKEN=false: clients may still join by presenting the server password, which every browser used to be given. Only for a staged upgrade; unset it once the servers mint tokens (GRYT-736)")
		}
		log.Printf("✅ Room manager initialized (debug: %t)", cfg.Debug)
		return nil
	})
	if err != nil {
		return fmt.Errorf("initialize room manager: %w", err)
	}

	// Initialize signaling coordinator with recovery
	err = recovery.SafeExecute("MAIN", "INIT_COORDINATOR", func() error {
		coordinator = signaling.NewCoordinator(trackManager, webrtcManager, roomManager, cfg.Debug)
		log.Printf("✅ Signaling coordinator initialized (debug: %t)", cfg.Debug)
		return nil
	})
	if err != nil {
		return fmt.Errorf("initialize signaling coordinator: %w", err)
	}

	// Initialize WebSocket handler with recovery
	var wsHandler *websocket.Handler
	err = recovery.SafeExecute("MAIN", "INIT_WEBSOCKET_HANDLER", func() error {
		wsHandler = websocket.NewHandler(cfg, webrtcAPI, trackManager, webrtcManager, roomManager, coordinator)
		log.Printf("✅ WebSocket handler initialized")
		return nil
	})
	if err != nil {
		return fmt.Errorf("initialize WebSocket handler: %w", err)
	}

	// The own voice transport (GRYT-1818): clients reach it at the addresses WebRTC advertises.
	var audio *audiotransport.Server
	switch {
	case cfg.AudioTransportPort == 0:
		log.Printf("🎙️ Audio transport off (AUDIO_TRANSPORT_PORT=0)")
	case len(cfg.ICEAdvertiseIPs) == 0:
		log.Printf("🎙️ Audio transport off: it needs ICE_ADVERTISE_IP to tell clients where to connect")
	default:
		audio = audiotransport.New(cfg.AudioTransportPort, cfg.ICEAdvertiseIPs)
		recovery.SafeGoroutine("MAIN", "AUDIO_TRANSPORT", func() {
			if err := audio.ListenAndServe(); err != nil {
				log.Printf("❌ Audio transport stopped: %v", err)
			}
		})
		wsHandler.SetAudioTransport(audio)
		log.Printf("🎙️ Audio transport on UDP %d for %v", cfg.AudioTransportPort, cfg.ICEAdvertiseIPs)
		if cfg.AudioUDPPort > 0 {
			recovery.SafeGoroutine("MAIN", "AUDIO_UDP", func() {
				if err := audio.ListenUDP(cfg.AudioUDPPort); err != nil {
					log.Printf("❌ Native audio UDP stopped: %v", err)
				}
			})
			log.Printf("🎙️ Native audio on UDP %d", cfg.AudioUDPPort)
		}
	}

	// Start room cleanup routine with recovery
	recovery.SafeGoroutine("MAIN", "ROOM_CLEANUP", func() {
		ticker := time.NewTicker(5 * time.Minute) // Check every 5 minutes
		defer ticker.Stop()

		log.Printf("🧹 Room cleanup routine started (check interval: 5m, cleanup threshold: 30m)")

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				recovery.SafeExecute("ROOM_CLEANUP", "CLEANUP_CYCLE", func() error {
					if cfg.Debug {
						log.Printf("🧹 Running scheduled room cleanup...")
					}
					roomManager.CleanupEmptyRooms(30 * time.Minute)
					return nil
				})
			}
		}
	})

	// Ending calls somebody is sitting in alone. Its own routine because the cleanup above
	// runs every five minutes against a thirty-minute threshold, and this has minutes.
	if cfg.CallAloneTimeout > 0 {
		recovery.SafeGoroutine("MAIN", "CALL_SWEEP", func() {
			ticker := time.NewTicker(config.DefaultCallSweepInterval)
			defer ticker.Stop()

			log.Printf("📴 Abandoned-call sweep started (check interval: %s, ends a call after %s alone)",
				config.DefaultCallSweepInterval, cfg.CallAloneTimeout)

			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					recovery.SafeExecute("CALL_SWEEP", "SWEEP_CYCLE", func() error {
						roomManager.EndAbandonedCalls(cfg.CallAloneTimeout)
						return nil
					})
				}
			}
		})
	} else {
		log.Printf("📴 Abandoned-call sweep off (SFU_CALL_ALONE_TIMEOUT=0)")
	}

	httpMux := http.NewServeMux()
	httpMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		ts := time.Now().Format(time.RFC3339)
		if !readiness.IsUDPReady() {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, `{"status":"starting","detail":"verifying UDP connectivity","service":"sfu","version":"%s","timestamp":"%s"}`, Version, ts)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"healthy","service":"sfu","version":"%s","timestamp":"%s"}`, Version, ts)
	})

	// Metrics get a listener of their own: on the default mux, /metrics sat beside the
	// signalling WebSocket. Publishing this port puts the full register back on the internet.
	var metricsServer *http.Server
	if cfg.MetricsPort > 0 {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("/metrics", promhttp.Handler())
		addr := net.JoinHostPort(cfg.MetricsHost, strconv.Itoa(cfg.MetricsPort))
		metricsServer = &http.Server{Addr: addr, Handler: metricsMux}
		recovery.SafeGoroutine("MAIN", "METRICS_LISTENER", func() {
			log.Printf("📊 Metrics on %s (container-only; do not publish this port)", addr)
			if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("❌ Metrics listener stopped: %v", err)
			}
		})
	} else {
		log.Printf("📊 SFU_METRICS_PORT=0, so metrics are recorded but not served anywhere")
	}

	// Periodically sync room/peer gauges with actual state
	recovery.SafeGoroutine("MAIN", "METRICS_SYNC", func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				stats := webrtcManager.GetRoomStats()
				metrics.RoomsActive.Set(float64(len(stats)))
				totalPeers := 0
				for _, count := range stats {
					totalPeers += count
				}
				metrics.PeersActive.Set(float64(totalPeers))
			}
		}
	})

	// Registration gets a listener of its own, for the same reason metrics do: on
	// one port, publishing it for clients published /server to strangers too.

	// Any stranger could then claim an unused server id and use this SFU as a relay.
	controlMux := http.NewServeMux()
	controlMux.HandleFunc("/server", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" && r.Header.Get("Connection") != "" {
			recovery.SafeExecuteWithContext("WEBSOCKET", "HANDLE_CONTROL_CONNECTION", "", "", r.RemoteAddr, func() error {
				wsHandler.HandleWebSocket(w, r)
				return nil
			})
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("This endpoint only accepts WebSocket connections."))
	})
	controlServer := &http.Server{Handler: controlMux}
	serveErr := make(chan error, 2)
	go func() {
		log.Printf("🔐 Server registration on %s (container-only; do not publish this port)", controlListener.Addr())
		if err := controlServer.Serve(controlListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- fmt.Errorf("control listener: %w", err)
		}
	}()

	// Handle WebSocket connections with recovery wrapper
	httpMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Registration lives on the control port now, and refusing it here is the whole
		// fix: this port is published, so anything reachable on it is reachable by anyone.
		if r.URL.Path == "/server" {
			// The header names the control port so a server still pointed here can move
			// itself. It hands a stranger nothing: an unpublished port is no less shut.
			w.Header().Set("X-Gryt-Control-Port", strconv.Itoa(cfg.ControlPort))
			log.Printf("🚫 Refused server registration on the public port from %s — pointed it at %d", r.RemoteAddr, cfg.ControlPort)
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, "Server registration is not served on this port. Point SFU_WS_HOST at port %d.\n", cfg.ControlPort)
			return
		}

		// The own transport's fallback for clients without WebTransport or UDP (GRYT-1831).
		if r.URL.Path == "/audio-ws" && audio != nil {
			audio.ServeWebSocket(w, r)
			return
		}

		// Check if this is a WebSocket upgrade request
		if r.Header.Get("Upgrade") == "websocket" && r.Header.Get("Connection") != "" {
			recovery.SafeExecuteWithContext("WEBSOCKET", "HANDLE_CONNECTION", "", "", r.RemoteAddr, func() error {
				wsHandler.HandleWebSocket(w, r)
				return nil
			})
		} else {
			// Handle non-WebSocket requests (health checks, monitoring, etc.)
			log.Printf("📋 Non-WebSocket request from %s: %s %s (User-Agent: %s)",
				r.RemoteAddr, r.Method, r.URL.Path, r.Header.Get("User-Agent"))

			// Return a helpful response for non-WebSocket requests
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("This endpoint only accepts WebSocket connections. Use /health for health checks."))
		}
	})

	log.Printf("✅ Endpoints configured:")
	log.Printf("   📡 / (WebSocket client endpoint)")
	log.Printf("   📡 /client (explicit WebSocket client endpoint)")
	log.Printf("   🔐 /server on %s (registration; container-only)", controlListener.Addr())
	log.Printf("   🏥 /health (HTTP health check endpoint)")
	log.Printf("   📊 /metrics (Prometheus metrics endpoint)")

	// Log initial system stats
	recovery.LogSystemStats()

	// Start the HTTP server with recovery
	log.Printf("🌐 Starting HTTP server on port %s", cfg.Port)
	// A wildcard bind, unlike the UDP mux above, so this is every address the host has. The
	// two can disagree, and when they do it is worth being able to see it.
	hostV4, hostV6 := hostAddresses(cfg.Port)
	logBoundAddresses("🌐 Signalling reachable on", hostV4, hostV6)
	log.Printf("🎯 SFU Server ready!")
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	logger.LogAction("MAIN", "SERVER_READY", "", "", "HTTP server starting on port "+cfg.Port)

	publicServer := &http.Server{Addr: ":" + cfg.Port, Handler: httpMux}
	go func() {
		if err := publicServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- fmt.Errorf("public HTTP listener: %w", err)
		}
	}()
	shutdown := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = publicServer.Shutdown(shutdownCtx)
		_ = controlServer.Shutdown(shutdownCtx)
		if metricsServer != nil {
			_ = metricsServer.Shutdown(shutdownCtx)
		}
		if audio != nil {
			_ = audio.Close()
		}
	}
	select {
	case <-ctx.Done():
		shutdown()
		return nil
	case err := <-serveErr:
		logger.LogAction("MAIN", "SERVER_ERROR", "", "", err.Error())
		shutdown()
		return err
	}
}

// logBoundAddresses says where a listener answers. IPv4 in full, IPv6 as a count: a host can
// have thirty IPv6 addresses, and printing them buries the two anybody is looking for.
func logBoundAddresses(label string, v4 []string, v6Count int) {
	if len(v4) == 0 && v6Count == 0 {
		log.Printf("%s: nothing. No usable interface was found.", label)

		return
	}

	suffix := ""
	if v6Count > 0 {
		suffix = fmt.Sprintf(" (and %d IPv6)", v6Count)
	}

	if len(v4) == 0 {
		log.Printf("%s: no IPv4%s", label, suffix)

		return
	}

	log.Printf("%s: %s%s", label, strings.Join(v4, ", "), suffix)
}

// interfaceNames maps an IP to the interface it sits on. Best effort: an address with no
// match is printed without one rather than held back.
func interfaceNames() map[string]string {
	names := map[string]string{}

	ifaces, err := net.Interfaces()
	if err != nil {
		return names
	}

	for _, iface := range ifaces {
		addrs, addrErr := iface.Addrs()
		if addrErr != nil {
			continue
		}
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok {
				names[ipNet.IP.String()] = iface.Name
			}
		}
	}

	return names
}

// muxAddresses asks the mux what it actually bound, rather than repeating the
// interface enumeration and hoping the two agree.
func muxAddresses(mux ice.UDPMux, port string) ([]string, int) {
	names := interfaceNames()

	v4 := []string{}
	v6 := 0

	for _, addr := range mux.GetListenAddresses() {
		udpAddr, ok := addr.(*net.UDPAddr)
		if !ok || udpAddr.IP.To4() == nil {
			v6++

			continue
		}
		v4 = append(v4, describe(udpAddr.IP, port, names))
	}
	sort.Strings(v4)

	return v4, v6
}

// hostAddresses lists the addresses a wildcard listener on this port answers on. Loopback is
// included deliberately: "127.0.0.1 and nothing else" is a real and diagnosable state.
func hostAddresses(port string) ([]string, int) {
	ifaces, err := net.Interfaces()
	if err != nil {
		log.Printf("⚠️  Could not enumerate interfaces: %v", err)

		return nil, 0
	}

	v4 := []string{}
	v6 := 0

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, addrErr := iface.Addrs()
		if addrErr != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			if ipNet.IP.To4() == nil {
				v6++

				continue
			}
			v4 = append(v4, fmt.Sprintf("%s:%s (%s)", ipNet.IP, port, iface.Name))
		}
	}
	sort.Strings(v4)

	return v4, v6
}

// describe renders one address as ip:port (iface), dropping the interface when
// it is not known.
func describe(ip net.IP, port string, names map[string]string) string {
	if name, ok := names[ip.String()]; ok {
		return fmt.Sprintf("%s:%s (%s)", ip, port, name)
	}

	return fmt.Sprintf("%s:%s", ip, port)
}

// srtpReplayWindow is how far behind the newest packet a retransmit can arrive and still count.
const srtpReplayWindow = 1024

// newSettingEngine builds the ICE half of the WebRTC API from the config. Extracted so a
// test can gather against it: a wrong answer here looks right until somebody reads an SDP.
func newSettingEngine(cfg *config.Config) (pion.SettingEngine, error) {
	se := pion.SettingEngine{}
	// Pion's 64-packet window threw away retransmits of a big keyframe, so the NACKs never
	// stopped and a share started on a thin link never showed (GRYT-1571). Chrome uses 1024.
	se.SetSRTPReplayProtectionWindow(srtpReplayWindow)

	// All ICE traffic flows over one UDP port: far easier through a firewall than a range,
	// and networks that drop UDP on high ports let a recognised one through.
	udpMux, muxErr := ice.NewMultiUDPMuxFromPort(
		cfg.ICEUDPMuxPort,
		ice.UDPMuxFromPortWithIPFilter(shouldBindICEUDPAddress),
	)
	if muxErr != nil {
		return se, fmt.Errorf("failed to create ICE UDP mux on port %d: %w", cfg.ICEUDPMuxPort, muxErr)
	}
	se.SetICEUDPMux(udpMux)
	log.Printf("🧊 ICE UDP mux on port: %d", cfg.ICEUDPMuxPort)
	// Where, not just which port. The mux binds one socket per interface address, so an
	// address missing here is one media cannot arrive on, whatever the firewall says (GRYT-482).
	muxV4, muxV6 := muxAddresses(udpMux, strconv.Itoa(cfg.ICEUDPMuxPort))
	logBoundAddresses("🧊 ICE UDP mux bound on", muxV4, muxV6)

	if len(cfg.ICEAdvertiseIPs) > 0 {
		if rewriteErr := se.SetICEAddressRewriteRules(pion.ICEAddressRewriteRule{
			External:        cfg.ICEAdvertiseIPs,
			AsCandidateType: pion.ICECandidateTypeHost,
			Mode:            pion.ICEAddressRewriteReplace,
		}); rewriteErr != nil {
			return se, fmt.Errorf("failed to set ICE address rewrite rules: %w", rewriteErr)
		}
		log.Printf("🧊 ICE address rewrite (host replace): %v", cfg.ICEAdvertiseIPs)
	}

	return se, nil
}

func shouldBindICEUDPAddress(ip net.IP) bool {
	return !ip.IsLinkLocalUnicast()
}

func ShouldBindICEUDPAddress(ip net.IP) bool {
	return shouldBindICEUDPAddress(ip)
}

func NewSettingEngine(cfg *config.Config) (pion.SettingEngine, error) {
	return newSettingEngine(cfg)
}

func RegisterCodecs(me *pion.MediaEngine) error {
	return registerCodecs(me)
}

// registerCodecs registers audio and video codecs with H264 listed first so
// SDP offers prefer H264, which has the widest hardware-accelerated support.
func registerCodecs(me *pion.MediaEngine) error {
	audioCodecs := []pion.RTPCodecParameters{
		{RTPCodecCapability: pion.RTPCodecCapability{MimeType: pion.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"}, PayloadType: 111},
		{RTPCodecCapability: pion.RTPCodecCapability{MimeType: pion.MimeTypeG722, ClockRate: 8000}, PayloadType: 9},
		{RTPCodecCapability: pion.RTPCodecCapability{MimeType: pion.MimeTypePCMU, ClockRate: 8000}, PayloadType: 0},
		{RTPCodecCapability: pion.RTPCodecCapability{MimeType: pion.MimeTypePCMA, ClockRate: 8000}, PayloadType: 8},
	}
	for _, c := range audioCodecs {
		if err := me.RegisterCodec(c, pion.RTPCodecTypeAudio); err != nil {
			return err
		}
	}

	videoFB := []pion.RTCPFeedback{
		{Type: "goog-remb"},
		{Type: "ccm", Parameter: "fir"},
		{Type: "nack"},
		{Type: "nack", Parameter: "pli"},
	}
	videoCodecs := []pion.RTPCodecParameters{
		// H264 first — widest HW-accelerated support across browsers & platforms
		{RTPCodecCapability: pion.RTPCodecCapability{MimeType: pion.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f", RTCPFeedback: videoFB}, PayloadType: 102},
		{RTPCodecCapability: pion.RTPCodecCapability{MimeType: pion.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f", RTCPFeedback: videoFB}, PayloadType: 125},
		{RTPCodecCapability: pion.RTPCodecCapability{MimeType: pion.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640032", RTCPFeedback: videoFB}, PayloadType: 123},
		{RTPCodecCapability: pion.RTPCodecCapability{MimeType: pion.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42001f", RTCPFeedback: videoFB}, PayloadType: 127},
		{RTPCodecCapability: pion.RTPCodecCapability{MimeType: pion.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42e01f", RTCPFeedback: videoFB}, PayloadType: 108},
		{RTPCodecCapability: pion.RTPCodecCapability{MimeType: pion.MimeTypeVP9, ClockRate: 90000, RTCPFeedback: videoFB}, PayloadType: 98},
		{RTPCodecCapability: pion.RTPCodecCapability{MimeType: pion.MimeTypeVP9, ClockRate: 90000, SDPFmtpLine: "profile-id=1", RTCPFeedback: videoFB}, PayloadType: 100},
		{RTPCodecCapability: pion.RTPCodecCapability{MimeType: pion.MimeTypeVP8, ClockRate: 90000, RTCPFeedback: videoFB}, PayloadType: 96},
		{RTPCodecCapability: pion.RTPCodecCapability{MimeType: pion.MimeTypeAV1, ClockRate: 90000, RTCPFeedback: videoFB}, PayloadType: 35},
	}
	for _, c := range videoCodecs {
		if err := me.RegisterCodec(c, pion.RTPCodecTypeVideo); err != nil {
			return err
		}
	}

	// Dependency Descriptor RTP header extension for SVC layer-aware forwarding.
	// Browsers include this when encoding with scalabilityMode (e.g. L1T3).
	const ddExtensionURI = "https://aomediacodec.github.io/av1-rtp-spec/#dependency-descriptor-rtp-header-extension"
	if err := me.RegisterHeaderExtension(
		pion.RTPHeaderExtensionCapability{URI: ddExtensionURI},
		pion.RTPCodecTypeVideo,
	); err != nil {
		return fmt.Errorf("failed to register DD header extension: %w", err)
	}

	return nil
}
