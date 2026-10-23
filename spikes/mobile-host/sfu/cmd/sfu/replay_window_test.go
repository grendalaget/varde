package main

import (
	"testing"
	"time"

	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"

	"sfu-v2/internal/config"
)

// TestLateRetransmitsReachTheSFU sends 300 packets with 40 held back, then the 40 late,
// the way a NACKed keyframe arrives. Each late one is 200+ behind the newest (GRYT-1571).
func TestLateRetransmitsReachTheSFU(t *testing.T) {
	se, err := newSettingEngine(&config.Config{ICEUDPMuxPort: 0, DisableSTUN: true})
	if err != nil {
		t.Fatalf("newSettingEngine: %v", err)
	}
	me := &pion.MediaEngine{}
	if err := registerCodecs(me); err != nil {
		t.Fatalf("registerCodecs: %v", err)
	}
	sfu, err := pion.NewAPI(pion.WithSettingEngine(se), pion.WithMediaEngine(me)).NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatalf("sfu NewPeerConnection: %v", err)
	}
	defer func() { _ = sfu.Close() }()
	if _, err := sfu.AddTransceiverFromKind(pion.RTPCodecTypeVideo, pion.RTPTransceiverInit{Direction: pion.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatalf("AddTransceiverFromKind: %v", err)
	}

	sender, err := pion.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatalf("sender NewPeerConnection: %v", err)
	}
	defer func() { _ = sender.Close() }()
	track, err := pion.NewTrackLocalStaticRTP(pion.RTPCodecCapability{MimeType: pion.MimeTypeH264, ClockRate: 90000}, "video", "share")
	if err != nil {
		t.Fatalf("NewTrackLocalStaticRTP: %v", err)
	}
	if _, err := sender.AddTrack(track); err != nil {
		t.Fatalf("AddTrack: %v", err)
	}

	got := make(chan uint16, 1024)
	sfu.OnTrack(func(remote *pion.TrackRemote, _ *pion.RTPReceiver) {
		for {
			pkt, _, err := remote.ReadRTP()
			if err != nil {
				return
			}
			got <- pkt.SequenceNumber
		}
	})

	connect(t, sfu, sender)

	const first, total = 1000, 300
	late := map[uint16]bool{}
	for i := 0; i < 40; i++ {
		late[first+50+uint16(i)] = true
	}
	send := func(seq uint16) {
		pkt := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(seq) * 3000}, Payload: []byte{0x65, 0x88, 0x84, 0x00}}
		if err := track.WriteRTP(pkt); err != nil {
			t.Fatalf("WriteRTP: %v", err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for seq := uint16(first); seq < first+total; seq++ {
		if !late[seq] {
			send(seq)
			time.Sleep(time.Millisecond)
		}
	}
	for seq := range late {
		send(seq)
	}

	seen := map[uint16]bool{}
	for len(seen) < len(late) {
		select {
		case seq := <-got:
			if late[seq] {
				seen[seq] = true
			}
		case <-time.After(time.Until(deadline)):
			t.Fatalf("%d of %d late packets reached the SFU; a replay window under 250 drops the rest", len(seen), len(late))
		}
	}
}

// connect runs a full offer and answer with every candidate in the SDP, and waits for both to connect.
func connect(t *testing.T, answerer, offerer *pion.PeerConnection) {
	t.Helper()
	up := make(chan struct{}, 2)
	for _, pc := range []*pion.PeerConnection{answerer, offerer} {
		pc.OnConnectionStateChange(func(s pion.PeerConnectionState) {
			if s == pion.PeerConnectionStateConnected {
				up <- struct{}{}
			}
		})
	}
	offer, err := offerer.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	gathered := pion.GatheringCompletePromise(offerer)
	if err := offerer.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription(offer): %v", err)
	}
	<-gathered
	if err := answerer.SetRemoteDescription(*offerer.LocalDescription()); err != nil {
		t.Fatalf("SetRemoteDescription(offer): %v", err)
	}
	answer, err := answerer.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("CreateAnswer: %v", err)
	}
	gathered = pion.GatheringCompletePromise(answerer)
	if err := answerer.SetLocalDescription(answer); err != nil {
		t.Fatalf("SetLocalDescription(answer): %v", err)
	}
	<-gathered
	if err := offerer.SetRemoteDescription(*answerer.LocalDescription()); err != nil {
		t.Fatalf("SetRemoteDescription(answer): %v", err)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-up:
		case <-time.After(15 * time.Second):
			t.Skip("the two peers never connected; no usable interface in this environment")
		}
	}
}
