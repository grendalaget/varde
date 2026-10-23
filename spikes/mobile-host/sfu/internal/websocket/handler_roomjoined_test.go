package websocket

import (
	"encoding/json"
	"testing"
	"time"

	"sfu-v2/internal/config"
	"sfu-v2/pkg/types"
)

/*
What a client is told when it joins (GRYT-715). The client drew a countdown from its own copy
of the SFU's default, which was right until an operator changed theirs.
*/

func TestRoomJoinedCarriesTheCallAloneTimeout(t *testing.T) {
	h := &Handler{config: &config.Config{CallAloneTimeout: 5 * time.Minute}}
	serverConn, clientConn := newTestSocketPair(t)

	h.sendRoomJoined(serverConn, "Successfully joined room", nil)

	message, ok := readNextMessage(t, clientConn, time.Second)
	if !ok {
		t.Fatal("no room_joined arrived")
	}
	if message.Event != types.EventRoomJoined {
		t.Fatalf("expected %s, got %s", types.EventRoomJoined, message.Event)
	}

	var joined types.RoomJoinedData
	if err := json.Unmarshal([]byte(message.Data), &joined); err != nil {
		t.Fatalf("room_joined data is not the JSON a client parses: %v (%q)", err, message.Data)
	}
	if joined.CallAloneTimeoutSeconds != 300 {
		t.Fatalf("five minutes should reach the client as 300 seconds, got %d", joined.CallAloneTimeoutSeconds)
	}
	if joined.Message != "Successfully joined room" {
		t.Fatalf("the message a client used to read the whole payload as is gone: %q", joined.Message)
	}
}

func TestRoomJoinedSaysZeroWhenTheSweepIsOff(t *testing.T) {
	h := &Handler{config: &config.Config{CallAloneTimeout: 0}}
	serverConn, clientConn := newTestSocketPair(t)

	h.sendRoomJoined(serverConn, "Successfully joined room", nil)

	message, ok := readNextMessage(t, clientConn, time.Second)
	if !ok {
		t.Fatal("no room_joined arrived")
	}

	var joined types.RoomJoinedData
	if err := json.Unmarshal([]byte(message.Data), &joined); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The disagreement that matters. An operator who turns the sweep off and gets a client
	// still hanging up after two minutes has a call ending for no visible reason.
	if joined.CallAloneTimeoutSeconds != 0 {
		t.Fatalf("SFU_CALL_ALONE_TIMEOUT=0 has to reach the client as 0, got %d", joined.CallAloneTimeoutSeconds)
	}
}

func TestRoomJoinedCarriesTheIngestCap(t *testing.T) {
	for _, tc := range []struct {
		kbps    int
		present bool
	}{{1500, true}, {0, false}} {
		h := &Handler{config: &config.Config{MaxIngestKbps: tc.kbps}}
		serverConn, clientConn := newTestSocketPair(t)

		h.sendRoomJoined(serverConn, "Successfully joined room", nil)

		message, ok := readNextMessage(t, clientConn, time.Second)
		if !ok {
			t.Fatal("no room_joined arrived")
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(message.Data), &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		got, present := raw["max_ingest_kbps"]
		// No cap is no key: a client reading 0 as a cap would send nothing.
		if present != tc.present || (present && got != float64(tc.kbps)) {
			t.Fatalf("SFU_MAX_INGEST_KBPS=%d reached the client as %v (present %v)", tc.kbps, got, present)
		}
	}
}

// The own voice transport (GRYT-1818): an offer rides on room_joined, and its absence is how a
// client knows to stay on WebRTC.
func TestRoomJoinedCarriesTheAudioTransportOffer(t *testing.T) {
	for _, offer := range []*types.AudioTransportOffer{
		{URLs: []string{"https://192.0.2.1:4433/voice"}, CertHash: "aGFzaA==", Ticket: "dGlja2V0"},
		nil,
	} {
		h := &Handler{config: &config.Config{}}
		serverConn, clientConn := newTestSocketPair(t)

		h.sendRoomJoined(serverConn, "Successfully joined room", offer)

		message, ok := readNextMessage(t, clientConn, time.Second)
		if !ok {
			t.Fatal("no room_joined arrived")
		}
		var joined types.RoomJoinedData
		if err := json.Unmarshal([]byte(message.Data), &joined); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if offer == nil {
			if joined.AudioTransport != nil {
				t.Fatalf("no transport running, but room_joined offered one: %+v", joined.AudioTransport)
			}
			continue
		}
		if joined.AudioTransport == nil || joined.AudioTransport.Ticket != offer.Ticket ||
			joined.AudioTransport.CertHash != offer.CertHash || joined.AudioTransport.URLs[0] != offer.URLs[0] {
			t.Fatalf("offer did not reach the client intact: %+v", joined.AudioTransport)
		}
	}
}
