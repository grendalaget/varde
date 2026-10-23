package websocket

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"sfu-v2/internal/recovery"
	"sfu-v2/internal/svc"
	"sfu-v2/pkg/types"
)

// videoWantedMinInterval is how often one stream's sender may be told what is wanted. A
// change inside it is sent when it ends, so the last word always arrives.
const videoWantedMinInterval = 250 * time.Millisecond

// maxDemandPixels and maxDemandFPS bound what a viewer can claim, so one bad report can't
// ask a sender for more than any screen shows.
const (
	maxDemandPixels = 16384
	maxDemandFPS    = 480
)

// handleVideoDemand records how big a viewer draws one track. A viewer coming back from
// hidden has had nothing forwarded, so the sender is asked for a keyframe.
func (h *Handler) handleVideoDemand(data, clientID, roomID string, pc *webrtc.PeerConnection) error {
	var req types.VideoDemandData
	if err := recovery.SafeJSONUnmarshal([]byte(data), &req); err != nil {
		h.debugLog("❌ Error unmarshalling video_demand from %s: %v", clientID, err)
		return nil
	}

	lf, ok := h.trackManager.GetVideoForwarderByStream(roomID, req.StreamID)
	// A sender's own self-view is not a viewer, or it would keep its own video from pausing.
	if !ok || lf.GetSenderPC() == pc {
		h.debugLog("⚠️ video_demand: no video in stream %s in room %s", req.StreamID, roomID)
		return nil
	}

	demand := svc.Demand{
		Width:  min(max(req.Width, 0), maxDemandPixels),
		Height: min(max(req.Height, 0), maxDemandPixels),
		FPS:    min(max(req.FPS, 0), maxDemandFPS),
	}
	wanted, _, woke := lf.SetDemand(clientID, demand)
	if woke {
		lf.RequestKeyframe()
	}
	h.debugLog("📐 video_demand: %s draws %s at %dx%d@%d, wanted now %+v", clientID, req.StreamID, demand.Width, demand.Height, demand.FPS, wanted)
	return nil
}

// watchVideoDemand sends a sender video_wanted for the stream on m-line mid whenever what its
// viewers want changes, and once now so it knows this SFU reports at all.
func (h *Handler) watchVideoDemand(lf *svc.LayerForwarder, conn *ThreadSafeWriter, mid string) {
	n := &wantedNotifier{send: func(last *svc.Wanted) {
		w := lf.Wanted()
		if last != nil && *last == w {
			return
		}
		*last = w
		payload, err := json.Marshal(types.VideoWantedData{
			Mid: mid, Width: w.Width, Height: w.Height, FPS: w.FPS, Watchers: w.Watchers, Unknown: w.Unknown,
		})
		if err != nil {
			return
		}
		h.debugLog("📐 video_wanted: mid %s → %s", mid, payload)
		_ = conn.WriteJSON(&types.WebSocketMessage{Event: types.EventVideoWanted, Data: string(payload)})
	}}
	lf.OnWantedChange(n.poke)
	n.poke()
}

// wantedNotifier coalesces changes into at most one send per videoWantedMinInterval. send
// reads the forwarder itself, so it always sends the latest and never a stale one.
type wantedNotifier struct {
	mu        sync.Mutex
	scheduled bool
	lastSend  time.Time
	sent      *svc.Wanted
	send      func(last *svc.Wanted)
}

func (n *wantedNotifier) poke() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.scheduled {
		return
	}
	n.scheduled = true
	wait := max(videoWantedMinInterval-time.Since(n.lastSend), 0)
	time.AfterFunc(wait, func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		n.scheduled = false
		n.lastSend = time.Now()
		if n.sent == nil {
			n.sent = &svc.Wanted{Width: -1}
		}
		n.send(n.sent)
	})
}

// midOf finds the mid of the m-line a track arrived on, which is how the sender names it.
func midOf(pc *webrtc.PeerConnection, receiver *webrtc.RTPReceiver) string {
	for _, transceiver := range pc.GetTransceivers() {
		if transceiver.Receiver() == receiver {
			return transceiver.Mid()
		}
	}
	return ""
}
