package websocket

import (
	"net"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"sfu-v2/internal/auth"
	"sfu-v2/internal/config"
	"sfu-v2/internal/track"
	peerManager "sfu-v2/internal/webrtc"
)

// loopbackAPI keeps ICE on loopback: a machine with many interfaces otherwise spends
// seconds checking pairs. No interceptor registry is given, so pion installs its defaults.
func loopbackAPI() *webrtc.API {
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	return webrtc.NewAPI(webrtc.WithSettingEngine(se))
}

type quietCoordinator struct{}

func (quietCoordinator) SignalPeerConnectionsInRoom(string) {}
func (quietCoordinator) OnTrackAddedToRoom(string)          {}
func (quietCoordinator) OnTrackRemovedFromRoom(string)      {}

// connectForTest runs the offer/answer the SFU and a client do, with candidates in the
// descriptions instead of trickled. The SFU offers, as it does in production.
func connectForTest(t *testing.T, sfu, client *webrtc.PeerConnection, addTrack func()) {
	t.Helper()

	offer, err := sfu.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	gathered := webrtc.GatheringCompletePromise(sfu)
	if err := sfu.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local offer: %v", err)
	}
	<-gathered
	if err := client.SetRemoteDescription(*sfu.LocalDescription()); err != nil {
		t.Fatalf("set remote offer: %v", err)
	}

	addTrack()

	answer, err := client.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("create answer: %v", err)
	}
	gathered = webrtc.GatheringCompletePromise(client)
	if err := client.SetLocalDescription(answer); err != nil {
		t.Fatalf("set local answer: %v", err)
	}
	<-gathered
	if err := sfu.SetRemoteDescription(*client.LocalDescription()); err != nil {
		t.Fatalf("set remote answer: %v", err)
	}
}

// A browser works out round-trip time from the LSR a receiver report echoes back. The SFU
// only has one to echo if it reads the sender's reports, which pion leaves to the caller.
func TestReceiverReportsEchoTheSendersReport(t *testing.T) {
	h := &Handler{config: &config.Config{}, trackManager: track.NewManager(false), coordinator: quietCoordinator{}}
	conn, _ := newTestSocketPair(t)

	sfu, err := peerManager.CreatePeerConnection(loopbackAPI(), webrtc.Configuration{})
	if err != nil {
		t.Fatalf("create SFU peer: %v", err)
	}
	t.Cleanup(func() { _ = sfu.Close() })
	h.setupWebRTCHandlers(sfu, conn, "client-a", "room-1", auth.NewLive(auth.Claims{Capabilities: []string{auth.CapSpeak}}))

	client, err := loopbackAPI().NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("create client peer: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	camera, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, "camera", "client-a")
	if err != nil {
		t.Fatalf("create track: %v", err)
	}
	var sender *webrtc.RTPSender
	connectForTest(t, sfu, client, func() {
		if sender, err = client.AddTrack(camera); err != nil {
			t.Fatalf("add track: %v", err)
		}
	})

	echoed := make(chan uint32, 1)
	go func() {
		for {
			packets, _, readErr := sender.ReadRTCP()
			if readErr != nil {
				return
			}
			for _, packet := range packets {
				rr, ok := packet.(*rtcp.ReceiverReport)
				if !ok {
					continue
				}
				for _, report := range rr.Reports {
					if report.LastSenderReport != 0 {
						select {
						case echoed <- report.LastSenderReport:
						default:
						}
						return
					}
				}
			}
		}
	}()

	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(10 * time.Second)
	for seq := uint16(0); ; seq++ {
		select {
		case <-echoed:
			return
		case <-deadline:
			t.Fatal("no receiver report echoed a sender report within ten seconds, so a browser gets no RTT from them")
		case <-tick.C:
			packet := &rtp.Packet{
				Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(seq) * 1800},
				Payload: []byte{0x10, 0x00, 0x00, 0x00},
			}
			if writeErr := camera.WriteRTP(packet); writeErr != nil {
				t.Fatalf("write RTP: %v", writeErr)
			}
		}
	}
}
