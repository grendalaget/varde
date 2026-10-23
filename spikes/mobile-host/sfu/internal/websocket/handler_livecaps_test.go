package websocket

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"sfu-v2/internal/auth"
	"sfu-v2/internal/config"
	"sfu-v2/internal/room"
	"sfu-v2/internal/track"
	peerManager "sfu-v2/internal/webrtc"
	"sfu-v2/pkg/types"
)

var (
	everything = claimsOf(auth.CapSpeak, auth.CapVideoChecked, auth.CapShareVideo, auth.CapShareScreen)
	noCamera   = claimsOf(auth.CapSpeak, auth.CapVideoChecked, auth.CapShareScreen)
	noSpeak    = claimsOf(auth.CapVideoChecked, auth.CapShareVideo, auth.CapShareScreen)
)

// GRYT-1426: the camera goes when share_video is taken mid-call and comes back when it is
// given back, on the same transceiver and without the client renegotiating.
func TestACameraFollowsShareVideoMidCall(t *testing.T) {
	live := auth.NewLive(everything)
	room := publishLive(t, live)

	room.waitFor(t, "the camera to arrive", func(k kindCount) bool { return k.video == 1 && k.audio == 1 })
	live.Set(noCamera)
	room.waitFor(t, "the camera to leave", func(k kindCount) bool { return k.video == 0 && k.audio == 1 })
	live.Set(everything)
	room.waitFor(t, "the camera to come back", func(k kindCount) bool { return k.video == 1 && k.audio == 1 })
}

// The harder direction: refused at join, so never read, then granted.
func TestACameraRefusedAtJoinStartsWhenGranted(t *testing.T) {
	live := auth.NewLive(noCamera)
	room := publishLive(t, live)

	room.waitFor(t, "the microphone to arrive", func(k kindCount) bool { return k.audio == 1 })
	room.stays(t, "the camera held back", func(k kindCount) bool { return k.video == 0 })
	live.Set(everything)
	room.waitFor(t, "the camera to be let through", func(k kindCount) bool { return k.video == 1 })
}

func TestAMicrophoneFollowsSpeakMidCall(t *testing.T) {
	live := auth.NewLive(everything)
	room := publishLive(t, live)

	room.waitFor(t, "the microphone to arrive", func(k kindCount) bool { return k.audio == 1 && k.video == 1 })
	live.Set(noSpeak)
	room.waitFor(t, "the microphone to leave", func(k kindCount) bool { return k.audio == 0 && k.video == 1 })
	live.Set(everything)
	room.waitFor(t, "the microphone to come back", func(k kindCount) bool { return k.audio == 1 })
}

// Over the control socket's handler: the owning server reaches the peer, anyone else not.
func TestUserCapabilitiesAnswersOnlyToTheRoomsServer(t *testing.T) {
	rooms := room.NewManager(false)
	if err := rooms.RegisterServer("srv-a", "secret-a", "srv-a_room"); err != nil {
		t.Fatalf("register a: %v", err)
	}
	if err := rooms.RegisterServer("srv-b", "secret-b", "srv-b_room"); err != nil {
		t.Fatalf("register b: %v", err)
	}
	h := &Handler{config: &config.Config{}, roomManager: rooms}
	live := auth.NewLive(everything)
	h.live.add("client-1", "srv-a_room", "user-1", live)

	send := func(server, password, room string, caps ...string) {
		data, _ := json.Marshal(types.UserCapabilitiesData{
			RoomID: room, UserID: "user-1", ServerID: server, ServerPassword: password, Capabilities: caps,
		})
		if err := h.handleUserCapabilities(string(data)); err != nil {
			t.Fatalf("handleUserCapabilities: %v", err)
		}
	}
	can := func() bool { c, _ := live.Snapshot(); return c.MayShare(auth.CapShareVideo) }

	send("srv-a", "wrong", "srv-a_room", auth.CapVideoChecked)
	send("srv-b", "secret-b", "srv-a_room", auth.CapVideoChecked)
	if !can() {
		t.Fatal("a server without the room's credentials changed a member's capabilities")
	}

	send("srv-a", "secret-a", "srv-a_room", auth.CapVideoChecked)
	if can() {
		t.Fatal("the room's own server could not take share_video away")
	}

	h.live.remove("client-1")
	send("srv-a", "secret-a", "srv-a_room", auth.CapVideoChecked, auth.CapShareVideo)
	if can() {
		t.Fatal("a peer that had left was still reachable")
	}
}

type kindCount struct{ audio, video int }

type liveRoom struct {
	tracks *track.Manager
}

func (r liveRoom) count() kindCount {
	var k kindCount
	for _, local := range r.tracks.GetTracksInRoom("room-1") {
		if local.Kind() == webrtc.RTPCodecTypeAudio {
			k.audio++
		} else {
			k.video++
		}
	}
	return k
}

func (r liveRoom) waitFor(t *testing.T, what string, ok func(kindCount) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok(r.count()) {
		if time.Now().After(deadline) {
			t.Fatalf("waited five seconds for %s; room has %+v", what, r.count())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (r liveRoom) stays(t *testing.T, what string, ok func(kindCount) bool) {
	t.Helper()
	for end := time.Now().Add(500 * time.Millisecond); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if !ok(r.count()) {
			t.Fatalf("expected %s; room has %+v", what, r.count())
		}
	}
}

// publishLive connects a client sending a microphone and a camera, RTP flowing on both until
// the test ends, through an SFU peer gated on live.
func publishLive(t *testing.T, live *auth.Live) liveRoom {
	t.Helper()
	tracks := track.NewManager(false)
	h := &Handler{config: &config.Config{}, trackManager: tracks, coordinator: quietCoordinator{}}
	conn, _ := newTestSocketPair(t)

	sfu, err := peerManager.CreatePeerConnection(loopbackAPI(), webrtc.Configuration{})
	if err != nil {
		t.Fatalf("create SFU peer: %v", err)
	}
	t.Cleanup(func() { _ = sfu.Close() })
	h.setupWebRTCHandlers(sfu, conn, "client-a", "room-1", live)

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
		for _, local := range []webrtc.TrackLocal{mic, camera} {
			if _, addErr := client.AddTrack(local); addErr != nil {
				t.Fatalf("add track: %v", addErr)
			}
		}
	})

	stop := make(chan struct{})
	done := make(chan struct{})
	t.Cleanup(func() { close(stop); <-done })
	go func() {
		defer close(done)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for seq := uint16(0); ; seq++ {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			for _, local := range []*webrtc.TrackLocalStaticRTP{mic, camera} {
				_ = local.WriteRTP(&rtp.Packet{
					Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(seq) * 960},
					Payload: []byte{0x10, 0x00, 0x00, 0x00},
				})
			}
		}
	}()
	return liveRoom{tracks: tracks}
}
