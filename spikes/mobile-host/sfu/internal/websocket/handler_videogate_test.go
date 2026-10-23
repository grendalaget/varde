package websocket

import (
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"sfu-v2/internal/auth"
	"sfu-v2/internal/config"
	"sfu-v2/internal/track"
	peerManager "sfu-v2/internal/webrtc"
)

func claimsOf(caps ...string) auth.Claims { return auth.Claims{Capabilities: caps} }

// Which slot needs which capability, for the tokens each server version mints.
func TestEachSlotAnswersToItsOwnCapability(t *testing.T) {
	pc := newSFUPeer(t)
	tr := pc.GetTransceivers()
	mic, camera, screen, screenAudio := tr[0].Receiver(), tr[1].Receiver(), tr[2].Receiver(), tr[3].Receiver()
	audio, video := webrtc.RTPCodecTypeAudio, webrtc.RTPCodecTypeVideo

	cases := []struct {
		name     string
		claims   auth.Claims
		receiver *webrtc.RTPReceiver
		kind     webrtc.RTPCodecType
		want     string
	}{
		// A server from before GRYT-1417: no marker, so video is allowed as it always was.
		{"old server, camera", claimsOf(auth.CapSpeak), camera, video, ""},
		{"old server, screen", claimsOf(auth.CapSpeak), screen, video, ""},
		{"old server, screen audio", claimsOf(auth.CapSpeak), screenAudio, audio, ""},
		{"old server, no speak", claimsOf(), mic, audio, auth.CapSpeak},

		{"all granted, camera", claimsOf(auth.CapSpeak, auth.CapVideoChecked, auth.CapShareVideo, auth.CapShareScreen), camera, video, ""},
		{"all granted, screen", claimsOf(auth.CapSpeak, auth.CapVideoChecked, auth.CapShareVideo, auth.CapShareScreen), screen, video, ""},

		{"no share_video, camera", claimsOf(auth.CapSpeak, auth.CapVideoChecked, auth.CapShareScreen), camera, video, auth.CapShareVideo},
		{"no share_video, screen", claimsOf(auth.CapSpeak, auth.CapVideoChecked, auth.CapShareScreen), screen, video, ""},
		{"no share_video, mic", claimsOf(auth.CapSpeak, auth.CapVideoChecked, auth.CapShareScreen), mic, audio, ""},

		{"no share_screen, screen", claimsOf(auth.CapSpeak, auth.CapVideoChecked, auth.CapShareVideo), screen, video, auth.CapShareScreen},
		{"no share_screen, screen audio", claimsOf(auth.CapSpeak, auth.CapVideoChecked, auth.CapShareVideo), screenAudio, audio, auth.CapShareScreen},
		{"no share_screen, camera", claimsOf(auth.CapSpeak, auth.CapVideoChecked, auth.CapShareVideo), camera, video, ""},

		// Speak and video are separate: losing one never takes the other.
		{"video only, mic", claimsOf(auth.CapVideoChecked, auth.CapShareVideo), mic, audio, auth.CapSpeak},
		{"video only, camera", claimsOf(auth.CapVideoChecked, auth.CapShareVideo), camera, video, ""},

		// Unreachable, since the SFU owns every transceiver. Video fails closed, audio open.
		{"unknown video, both granted", claimsOf(auth.CapVideoChecked, auth.CapShareVideo, auth.CapShareScreen), nil, video, ""},
		{"unknown video, camera only", claimsOf(auth.CapVideoChecked, auth.CapShareVideo), nil, video, auth.CapShareScreen},
		{"unknown audio", claimsOf(auth.CapVideoChecked), nil, audio, ""},
	}
	for _, c := range cases {
		if got := refusedBy(c.claims, pc, c.receiver, c.kind); got != c.want {
			t.Errorf("%s: refusedBy = %q, want %q", c.name, got, c.want)
		}
	}
}

// Through a real peer connection with RTP flowing: a refused camera never becomes a track
// in the room, so no other peer is sent it, and the microphone beside it still does.
func TestARefusedCameraNeverReachesTheRoom(t *testing.T) {
	denied := claimsOf(auth.CapSpeak, auth.CapVideoChecked, auth.CapShareScreen)
	if kinds := publishMicAndCamera(t, denied); len(kinds) != 1 || kinds[0] != webrtc.RTPCodecTypeAudio {
		t.Fatalf("room carries %v, want the microphone alone", kinds)
	}
}

func TestAnAllowedCameraReachesTheRoom(t *testing.T) {
	allowed := claimsOf(auth.CapSpeak, auth.CapVideoChecked, auth.CapShareVideo)
	if kinds := publishMicAndCamera(t, allowed); len(kinds) != 2 {
		t.Fatalf("room carries %v, want the microphone and the camera", kinds)
	}
}

// A token from a server that predates the video capabilities keeps the camera working.
func TestAnOldServersTokenStillCarriesTheCamera(t *testing.T) {
	if kinds := publishMicAndCamera(t, claimsOf(auth.CapSpeak)); len(kinds) != 2 {
		t.Fatalf("room carries %v, want the microphone and the camera", kinds)
	}
}

// publishMicAndCamera sends RTP on both and returns the kinds the room ends up forwarding.
// It waits for the microphone first, so the camera has had the same chance to arrive.
func publishMicAndCamera(t *testing.T, claims auth.Claims) []webrtc.RTPCodecType {
	t.Helper()
	tracks := track.NewManager(false)
	h := &Handler{config: &config.Config{}, trackManager: tracks, coordinator: quietCoordinator{}}
	conn, _ := newTestSocketPair(t)

	sfu, err := peerManager.CreatePeerConnection(loopbackAPI(), webrtc.Configuration{})
	if err != nil {
		t.Fatalf("create SFU peer: %v", err)
	}
	t.Cleanup(func() { _ = sfu.Close() })
	h.setupWebRTCHandlers(sfu, conn, "client-a", "room-1", auth.NewLive(claims))

	client, err := loopbackAPI().NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("create client peer: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	mic, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "mic", "client-a")
	if err != nil {
		t.Fatalf("create mic: %v", err)
	}
	camera, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, "camera", "client-a")
	if err != nil {
		t.Fatalf("create camera: %v", err)
	}
	connectForTest(t, sfu, client, func() {
		// AddTrack takes the first free transceiver of its kind: slot 0 and slot 1.
		for _, local := range []webrtc.TrackLocal{mic, camera} {
			if _, addErr := client.AddTrack(local); addErr != nil {
				t.Fatalf("add track: %v", addErr)
			}
		}
	})

	kinds := func() []webrtc.RTPCodecType {
		var out []webrtc.RTPCodecType
		for _, local := range tracks.GetTracksInRoom("room-1") {
			out = append(out, local.Kind())
		}
		return out
	}
	hasAudio := func() bool {
		for _, k := range kinds() {
			if k == webrtc.RTPCodecTypeAudio {
				return true
			}
		}
		return false
	}

	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(10 * time.Second)
	var settled <-chan time.Time
	for seq := uint16(0); ; seq++ {
		select {
		case <-deadline:
			t.Fatalf("the microphone never reached the room within ten seconds (room: %v)", kinds())
		case <-settled:
			return kinds()
		case <-tick.C:
			for _, local := range []*webrtc.TrackLocalStaticRTP{mic, camera} {
				packet := &rtp.Packet{
					Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(seq) * 960},
					Payload: []byte{0x10, 0x00, 0x00, 0x00},
				}
				if writeErr := local.WriteRTP(packet); writeErr != nil {
					t.Fatalf("write RTP: %v", writeErr)
				}
			}
			if settled == nil && hasAudio() {
				settled = time.After(500 * time.Millisecond)
			}
		}
	}
}
