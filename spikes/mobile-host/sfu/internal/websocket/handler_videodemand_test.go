package websocket

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	gorilla "github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"sfu-v2/internal/auth"
	"sfu-v2/internal/config"
	"sfu-v2/internal/svc"
	"sfu-v2/internal/track"
	peerManager "sfu-v2/internal/webrtc"
	"sfu-v2/pkg/types"
)

// nextWanted reads messages off the sender's socket until a video_wanted arrives.
func nextWanted(t *testing.T, ws *gorilla.Conn) types.VideoWantedData {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var msg types.WebSocketMessage
		if err := ws.ReadJSON(&msg); err != nil {
			t.Fatalf("no video_wanted reached the sender: %v", err)
		}
		if msg.Event != types.EventVideoWanted {
			continue
		}
		var w types.VideoWantedData
		if err := json.Unmarshal([]byte(msg.Data), &w); err != nil {
			t.Fatalf("video_wanted %q: %v", msg.Data, err)
		}
		return w
	}
}

// One sender and one viewer, through real peer connections: the viewer's demand reaches the
// sender as video_wanted, hiding stops forwarding, and coming back gets a keyframe.
func TestVideoDemandGatesTheViewerAndTellsTheSender(t *testing.T) {
	tracks := track.NewManager(false)
	h := &Handler{config: &config.Config{}, trackManager: tracks, coordinator: quietCoordinator{}}
	conn, senderWS := newTestSocketPair(t)

	sfu, err := peerManager.CreatePeerConnection(loopbackAPI(), webrtc.Configuration{})
	if err != nil {
		t.Fatalf("create SFU peer: %v", err)
	}
	t.Cleanup(func() { _ = sfu.Close() })
	h.setupWebRTCHandlers(sfu, conn, "sender", "room-1", auth.NewLive(auth.Claims{Capabilities: []string{auth.CapSpeak}}))

	sender, err := loopbackAPI().NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("create sender peer: %v", err)
	}
	t.Cleanup(func() { _ = sender.Close() })
	camera, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, "camera", "sender")
	if err != nil {
		t.Fatalf("create camera: %v", err)
	}
	var rtpSender *webrtc.RTPSender
	connectForTest(t, sfu, sender, func() {
		if rtpSender, err = sender.AddTrack(camera); err != nil {
			t.Fatalf("add track: %v", err)
		}
	})

	plis := make(chan struct{}, 16)
	go func() {
		for {
			packets, _, readErr := rtpSender.ReadRTCP()
			if readErr != nil {
				return
			}
			for _, p := range packets {
				if _, ok := p.(*rtcp.PictureLossIndication); ok {
					plis <- struct{}{}
				}
			}
		}
	}()

	// One packet per frame, so every packet is a frame start and the gate can move on any.
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for seq := uint16(0); ; seq++ {
			select {
			case <-stop:
				return
			case <-tick.C:
				_ = camera.WriteRTP(&rtp.Packet{
					Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(seq) * 3000},
					Payload: []byte{0x10, 0x00, 0x00, 0x00},
				})
			}
		}
	}()

	first := nextWanted(t, senderWS)
	if first.Mid == "" || first.Watchers != 0 || first.Unknown != 0 {
		t.Fatalf("first video_wanted = %+v, want a mid and nobody watching", first)
	}

	var lfFound bool
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && !lfFound; time.Sleep(20 * time.Millisecond) {
		_, lfFound = tracks.GetForwarder("room-1", "camera")
	}
	lf, _ := tracks.GetForwarder("room-1", "camera")
	if lf == nil {
		t.Fatal("the camera never reached the room")
	}

	// The viewer's leg, set up the way the coordinator does it.
	sfuToViewer, err := loopbackAPI().NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("create SFU viewer peer: %v", err)
	}
	t.Cleanup(func() { _ = sfuToViewer.Close() })
	viewer, err := loopbackAPI().NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("create viewer peer: %v", err)
	}
	t.Cleanup(func() { _ = viewer.Close() })
	seqs := make(chan uint16, 4096)
	viewer.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			p, _, readErr := remote.ReadRTP()
			if readErr != nil {
				return
			}
			seqs <- p.SequenceNumber
		}
	})
	forViewer, err := sfuToViewer.AddTrack(lf.AddReceiver("viewer", -1))
	if err != nil {
		t.Fatalf("add viewer track: %v", err)
	}
	go func() {
		for {
			if _, _, readErr := forViewer.ReadRTCP(); readErr != nil {
				return
			}
		}
	}()
	connectForTest(t, sfuToViewer, viewer, func() {})

	if w := nextWanted(t, senderWS); w.Unknown != 1 || w.Watchers != 0 {
		t.Fatalf("with an unreported viewer, video_wanted = %+v, want unknown 1", w)
	}
	last := awaitPackets(t, seqs, 10)

	demand := func(w, h2, fps int) {
		data, _ := json.Marshal(types.VideoDemandData{StreamID: "sender", Width: w, Height: h2, FPS: fps})
		if err := h.handleVideoDemand(string(data), "viewer", "room-1", nil); err != nil {
			t.Fatalf("video_demand: %v", err)
		}
	}

	// The sender drawing its own self-view counts for nothing.
	self, _ := json.Marshal(types.VideoDemandData{StreamID: "sender", Width: 1920, Height: 1080, FPS: 60})
	_ = h.handleVideoDemand(string(self), "sender", "room-1", sfu)
	if w := lf.Wanted(); w.Watchers != 0 || w.Unknown != 1 {
		t.Fatalf("a sender's own demand counted: %+v", w)
	}

	demand(0, 0, 60)
	if w := nextWanted(t, senderWS); w != (types.VideoWantedData{Mid: first.Mid}) {
		t.Fatalf("with the only viewer hidden, video_wanted = %+v", w)
	}
	time.Sleep(100 * time.Millisecond)
	last = drain(seqs, last)
	time.Sleep(300 * time.Millisecond)
	if got := drain(seqs, last); got != last {
		t.Fatalf("a hidden viewer was still forwarded packets (last %d, now %d)", last, got)
	}
	for len(plis) > 0 {
		<-plis
	}

	demand(640, 360, 60)
	if w := nextWanted(t, senderWS); w != (types.VideoWantedData{Mid: first.Mid, Width: 640, Height: 360, FPS: 60, Watchers: 1}) {
		t.Fatalf("with the viewer back, video_wanted = %+v", w)
	}
	select {
	case <-plis:
	case <-time.After(3 * time.Second):
		t.Fatal("the sender got no PLI for a viewer coming back from hidden")
	}
	select {
	case seq := <-seqs:
		if seq != last+1 {
			t.Fatalf("first packet after hidden is %d, want %d: the viewer sees a gap", seq, last+1)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("forwarding never resumed")
	}

	lf.RemoveReceiver("viewer")
	if w := nextWanted(t, senderWS); w != (types.VideoWantedData{Mid: first.Mid}) {
		t.Fatalf("after the viewer left, video_wanted = %+v", w)
	}
}

// awaitPackets waits for n packets and returns the last sequence number.
func awaitPackets(t *testing.T, seqs <-chan uint16, n int) uint16 {
	t.Helper()
	var last uint16
	for i := 0; i < n; i++ {
		select {
		case last = <-seqs:
		case <-time.After(5 * time.Second):
			t.Fatalf("got %d of %d packets", i, n)
		}
	}
	return last
}

func drain(seqs <-chan uint16, last uint16) uint16 {
	for {
		select {
		case last = <-seqs:
		default:
			return last
		}
	}
}

// A burst of changes reaches the sender as one send of the last value, not as every step.
func TestVideoWantedIsCoalesced(t *testing.T) {
	var mu sync.Mutex
	current, sends, lastSent := 0, 0, -1
	n := &wantedNotifier{send: func(*svc.Wanted) {
		mu.Lock()
		defer mu.Unlock()
		sends++
		lastSent = current
	}}

	for i := 1; i <= 50; i++ {
		mu.Lock()
		current = i
		mu.Unlock()
		n.poke()
		time.Sleep(time.Millisecond)
	}
	time.Sleep(2 * videoWantedMinInterval)

	mu.Lock()
	defer mu.Unlock()
	if sends > 2 || lastSent != 50 {
		t.Fatalf("%d sends, last %d: want at most 2, ending on 50", sends, lastSent)
	}
}
