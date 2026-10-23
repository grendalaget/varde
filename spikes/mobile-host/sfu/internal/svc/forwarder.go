package svc

import (
	"encoding/binary"
	"log"
	"sync"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// ReceiverState tracks a single downstream receiver's track and layer subscription.
type ReceiverState struct {
	Track            *webrtc.TrackLocalStaticRTP
	MaxTemporalLayer int // packets with temporal_id > this are dropped
	active           bool

	demand   Demand
	reported bool // false for a viewer that never sent video_demand, which counts as full size

	// Owned by run(). The gate only opens or closes on a frame's first packet, and every
	// packet it held back comes off later sequence numbers, so the viewer sees no gap.
	hidden   bool
	seqShift uint16
}

// Demand is how big one viewer draws a video track, in device pixels. Zero means hidden.
type Demand struct {
	Width, Height, FPS int
}

func (d Demand) hidden() bool { return d.Width <= 0 || d.Height <= 0 }

// Wanted is what the sender is told: the largest demand across viewers, how many are
// watching, and how many never said.
type Wanted struct {
	Width, Height, FPS int
	Watchers, Unknown  int
}

// LayerForwarder reads RTP from a remote track, parses the Dependency Descriptor, and
// forwards per receiver by subscribed temporal layer. Without a DD it blindly relays.
type LayerForwarder struct {
	mu sync.RWMutex

	remoteTrack *webrtc.TrackRemote
	senderPC    *webrtc.PeerConnection
	remoteSSRC  uint32

	codec    webrtc.RTPCodecCapability
	trackID  string
	streamID string

	ddParser *DDParser
	ddExtID  uint8 // negotiated RTP extension ID for DD; 0 = not yet detected
	hasSVC   bool  // true once a DD extension has been seen

	receivers map[string]*ReceiverState
	wanted    Wanted
	onWanted  func()

	stopped chan struct{}
	debug   bool
}

// NewLayerForwarder creates a forwarder for the given remote track and starts its goroutine,
// which runs until the remote track ends or Stop is called.
func NewLayerForwarder(remote *webrtc.TrackRemote, senderPC *webrtc.PeerConnection, debug bool) *LayerForwarder {
	lf := &LayerForwarder{
		remoteTrack: remote,
		senderPC:    senderPC,
		remoteSSRC:  uint32(remote.SSRC()),
		codec:       remote.Codec().RTPCodecCapability,
		trackID:     remote.ID(),
		streamID:    remote.StreamID(),
		ddParser:    NewDDParser(),
		receivers:   make(map[string]*ReceiverState),
		stopped:     make(chan struct{}),
		debug:       debug,
	}
	go lf.run()
	return lf
}

// GetSenderPC returns the original sender's peer connection.
func (lf *LayerForwarder) GetSenderPC() *webrtc.PeerConnection {
	return lf.senderPC
}

// GetRemoteSSRC returns the SSRC of the remote track.
func (lf *LayerForwarder) GetRemoteSSRC() uint32 {
	return lf.remoteSSRC
}

// AddReceiver creates a per-receiver TrackLocalStaticRTP and registers it.
// maxTemporalLayer = -1 means forward all layers (no filtering).
func (lf *LayerForwarder) AddReceiver(receiverID string, maxTemporalLayer int) *webrtc.TrackLocalStaticRTP {
	lf.mu.Lock()
	if r, ok := lf.receivers[receiverID]; ok && r.Track != nil {
		r.MaxTemporalLayer = maxTemporalLayer
		r.active = true
		lf.mu.Unlock()
		return r.Track
	}

	track, err := webrtc.NewTrackLocalStaticRTP(lf.codec, lf.trackID, lf.streamID)
	if err != nil {
		lf.debugLog("failed to create per-receiver track for %s: %v", receiverID, err)
		lf.mu.Unlock()
		return nil
	}

	// A demand can arrive before the track is added, right after a reconnect. Keep it.
	rs := lf.receivers[receiverID]
	if rs == nil {
		rs = &ReceiverState{}
		lf.receivers[receiverID] = rs
	}
	rs.Track = track
	rs.MaxTemporalLayer = maxTemporalLayer
	rs.active = true
	changed := lf.recomputeWanted()
	lf.debugLog("added receiver %s (maxTemporal=%d, total=%d)", receiverID, maxTemporalLayer, len(lf.receivers))
	lf.mu.Unlock()

	lf.notifyIf(changed)
	return track
}

// RemoveReceiver removes a receiver from the fanout. Its demand goes with it, which can
// lower what the sender is asked for.
func (lf *LayerForwarder) RemoveReceiver(receiverID string) (Wanted, bool) {
	lf.mu.Lock()
	delete(lf.receivers, receiverID)
	changed := lf.recomputeWanted()
	wanted := lf.wanted
	lf.debugLog("removed receiver %s (remaining=%d)", receiverID, len(lf.receivers))
	lf.mu.Unlock()

	lf.notifyIf(changed)
	return wanted, changed
}

// SetDemand records how big a receiver draws this track. It returns the new wanted size,
// whether that changed, and whether this receiver just went from hidden to watching.
func (lf *LayerForwarder) SetDemand(receiverID string, d Demand) (wanted Wanted, changed, woke bool) {
	lf.mu.Lock()
	rs := lf.receivers[receiverID]
	if rs == nil {
		rs = &ReceiverState{}
		lf.receivers[receiverID] = rs
	}
	woke = rs.reported && rs.demand.hidden() && !d.hidden()
	rs.demand = d
	rs.reported = true
	changed = lf.recomputeWanted()
	wanted = lf.wanted
	lf.mu.Unlock()

	lf.notifyIf(changed)
	return wanted, changed, woke
}

// Wanted returns the current largest demand across receivers.
func (lf *LayerForwarder) Wanted() Wanted {
	lf.mu.RLock()
	defer lf.mu.RUnlock()
	return lf.wanted
}

// OnWantedChange registers the one callback told when Wanted changes. It runs outside the
// forwarder's lock, so it should hand off rather than write to a socket.
func (lf *LayerForwarder) OnWantedChange(f func()) {
	lf.mu.Lock()
	lf.onWanted = f
	lf.mu.Unlock()
}

// RequestKeyframe asks the sender for a keyframe, for a viewer that has nothing to decode from.
func (lf *LayerForwarder) RequestKeyframe() {
	if lf.senderPC == nil {
		return
	}
	_ = lf.senderPC.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: lf.remoteSSRC}})
}

// recomputeWanted must be called with mu held for writing.
func (lf *LayerForwarder) recomputeWanted() bool {
	var w Wanted
	for _, rs := range lf.receivers {
		switch {
		case !rs.reported:
			w.Unknown++
		case !rs.demand.hidden():
			w.Watchers++
			w.Width = max(w.Width, rs.demand.Width)
			w.Height = max(w.Height, rs.demand.Height)
			w.FPS = max(w.FPS, rs.demand.FPS)
		}
	}
	changed := w != lf.wanted
	lf.wanted = w
	return changed
}

func (lf *LayerForwarder) notifyIf(changed bool) {
	if !changed {
		return
	}
	lf.mu.RLock()
	f := lf.onWanted
	lf.mu.RUnlock()
	if f != nil {
		f()
	}
}

// SetMaxTemporalLayer updates the temporal layer cap for a receiver.
func (lf *LayerForwarder) SetMaxTemporalLayer(receiverID string, layer int) {
	lf.mu.Lock()
	defer lf.mu.Unlock()
	if r, ok := lf.receivers[receiverID]; ok {
		r.MaxTemporalLayer = layer
		lf.debugLog("receiver %s maxTemporal → %d", receiverID, layer)
	}
}

// GetReceiverTrack returns the local track for a receiver, or nil if not found.
func (lf *LayerForwarder) GetReceiverTrack(receiverID string) *webrtc.TrackLocalStaticRTP {
	lf.mu.RLock()
	defer lf.mu.RUnlock()
	if r, ok := lf.receivers[receiverID]; ok {
		return r.Track
	}
	return nil
}

// HasReceiver returns true if the given receiver ID is registered.
func (lf *LayerForwarder) HasReceiver(receiverID string) bool {
	lf.mu.RLock()
	defer lf.mu.RUnlock()
	_, ok := lf.receivers[receiverID]
	return ok
}

// TrackID returns the logical track ID (same as the remote track).
func (lf *LayerForwarder) TrackID() string {
	return lf.trackID
}

// StreamID returns the stream ID (same as the remote track).
func (lf *LayerForwarder) StreamID() string {
	return lf.streamID
}

// Kind returns the track kind (audio or video).
func (lf *LayerForwarder) Kind() webrtc.RTPCodecType {
	return lf.remoteTrack.Kind()
}

// Stop terminates the forwarding goroutine.
func (lf *LayerForwarder) Stop() {
	select {
	case <-lf.stopped:
	default:
		close(lf.stopped)
	}
}

// run is the main forwarding loop. It reads raw RTP bytes from the remote track,
// parses the RTP header to check for a DD extension, and fans out to receivers.
func (lf *LayerForwarder) run() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[SVC-FORWARDER] panic in forwarding loop for track %s: %v", lf.trackID, r)
		}
	}()

	buf := make([]byte, 1500)
	shifted := make([]byte, 1500)
	var header rtp.Header
	var lastTimestamp uint32

	for {
		select {
		case <-lf.stopped:
			return
		default:
		}

		n, _, readErr := lf.remoteTrack.Read(buf)
		if readErr != nil {
			lf.debugLog("track read ended: %v", readErr)
			return
		}

		raw := buf[:n]

		// Parse the RTP header to extract DD extension.
		headerLen, unmarshalErr := header.Unmarshal(raw)
		_ = headerLen
		temporalID := -1

		if unmarshalErr == nil {
			temporalID = lf.extractTemporalID(&header)
		}

		frameStart := unmarshalErr == nil && header.Timestamp != lastTimestamp
		if unmarshalErr == nil {
			lastTimestamp = header.Timestamp
		}

		// Only run() touches hidden and seqShift, and writers of the rest take the full lock.
		lf.mu.RLock()
		for _, rs := range lf.receivers {
			if !rs.active || rs.Track == nil {
				continue
			}

			if frameStart {
				rs.hidden = rs.reported && rs.demand.hidden()
			}
			if rs.hidden && unmarshalErr == nil {
				rs.seqShift++
				continue
			}

			if lf.hasSVC && temporalID >= 0 && rs.MaxTemporalLayer >= 0 && temporalID > rs.MaxTemporalLayer {
				continue
			}

			out := raw
			if rs.seqShift != 0 && unmarshalErr == nil {
				out = shifted[:n]
				copy(out, raw)
				binary.BigEndian.PutUint16(out[2:4], header.SequenceNumber-rs.seqShift)
			}
			if _, writeErr := rs.Track.Write(out); writeErr != nil {
				continue
			}
		}
		lf.mu.RUnlock()
	}
}

// extractTemporalID tries to find and parse the DD extension from the RTP header.
// Returns the temporal layer ID, or -1 if no DD extension is present.
func (lf *LayerForwarder) extractTemporalID(h *rtp.Header) int {
	// If we already know the extension ID, use it directly.
	if lf.ddExtID > 0 {
		ext := h.GetExtension(lf.ddExtID)
		if ext == nil {
			return -1
		}
		fi, err := lf.ddParser.Parse(ext)
		if err != nil {
			return -1
		}
		if !lf.hasSVC && fi.TemporalID >= 0 {
			lf.hasSVC = true
			lf.debugLog("SVC detected (temporal_id=%d)", fi.TemporalID)
		}
		return fi.TemporalID
	}

	// Auto-detect: scan all extensions for one that looks like a DD header.
	for _, id := range h.GetExtensionIDs() {
		ext := h.GetExtension(id)
		if len(ext) < 3 {
			continue
		}
		fi, err := lf.ddParser.Parse(ext)
		if err != nil {
			continue
		}
		if fi.TemporalID >= 0 {
			lf.ddExtID = id
			lf.hasSVC = true
			lf.debugLog("auto-detected DD extension ID=%d (temporal=%d)", id, fi.TemporalID)
			return fi.TemporalID
		}
		// Even if temporalID is -1 (no template yet), the parse succeeded which
		// means the extension is likely DD. Cache the ID for future packets.
		lf.ddExtID = id
		lf.debugLog("auto-detected DD extension ID=%d (template pending)", id)
		return -1
	}

	return -1
}

func (lf *LayerForwarder) debugLog(format string, args ...interface{}) {
	if lf.debug {
		log.Printf("[SVC-FORWARDER] "+format, args...)
	}
}
