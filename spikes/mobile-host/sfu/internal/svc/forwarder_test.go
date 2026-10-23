package svc

import (
	"testing"

	"github.com/pion/webrtc/v4"
)

// A forwarder with no remote track and no goroutine: the demand bookkeeping needs neither.
func newTestForwarder() *LayerForwarder {
	return &LayerForwarder{
		codec:     webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
		trackID:   "camera",
		streamID:  "sender",
		receivers: make(map[string]*ReceiverState),
		stopped:   make(chan struct{}),
	}
}

func TestWantedIsTheLargestReport(t *testing.T) {
	lf := newTestForwarder()
	lf.AddReceiver("a", -1)
	lf.AddReceiver("b", -1)
	lf.SetDemand("a", Demand{Width: 640, Height: 360, FPS: 60})
	w, _, _ := lf.SetDemand("b", Demand{Width: 1920, Height: 800, FPS: 144})

	want := Wanted{Width: 1920, Height: 800, FPS: 144, Watchers: 2}
	if w != want {
		t.Fatalf("wanted = %+v, want %+v", w, want)
	}
}

// An old client never reports. It has to count as unknown, never as hidden, or a sender
// would pause the stream an old client is watching.
func TestAViewerThatNeverReportsIsUnknown(t *testing.T) {
	lf := newTestForwarder()
	lf.AddReceiver("old", -1)
	lf.AddReceiver("new", -1)
	w, _, _ := lf.SetDemand("new", Demand{})

	want := Wanted{Unknown: 1}
	if w != want {
		t.Fatalf("wanted = %+v, want %+v", w, want)
	}
}

func TestALeavingViewerLowersWanted(t *testing.T) {
	lf := newTestForwarder()
	lf.AddReceiver("big", -1)
	lf.AddReceiver("small", -1)
	lf.SetDemand("big", Demand{Width: 1920, Height: 1080, FPS: 60})
	lf.SetDemand("small", Demand{Width: 320, Height: 180, FPS: 60})

	w, changed := lf.RemoveReceiver("big")
	if !changed || w != (Wanted{Width: 320, Height: 180, FPS: 60, Watchers: 1}) {
		t.Fatalf("after the big viewer left: wanted = %+v changed = %v", w, changed)
	}
	w, changed = lf.RemoveReceiver("small")
	if !changed || w != (Wanted{}) {
		t.Fatalf("after everybody left: wanted = %+v changed = %v", w, changed)
	}
}

// woke asks the sender for a keyframe, so it has to fire only for a viewer the gate held back.
func TestWokeOnlyFromHidden(t *testing.T) {
	lf := newTestForwarder()
	lf.AddReceiver("a", -1)
	size := Demand{Width: 640, Height: 360, FPS: 60}

	if _, _, woke := lf.SetDemand("a", size); woke {
		t.Error("unknown to watching woke, but nothing was held back")
	}
	if _, _, woke := lf.SetDemand("a", Demand{Width: 1280, Height: 720, FPS: 60}); woke {
		t.Error("a resize woke")
	}
	if _, _, woke := lf.SetDemand("a", Demand{}); woke {
		t.Error("hiding woke")
	}
	if _, _, woke := lf.SetDemand("a", size); !woke {
		t.Error("hidden to watching did not wake")
	}
}

// Right after a reconnect the demand can beat the coordinator adding the track.
func TestDemandBeforeTheTrackIsKept(t *testing.T) {
	lf := newTestForwarder()
	lf.SetDemand("a", Demand{})
	if lf.AddReceiver("a", -1) == nil {
		t.Fatal("no track for a receiver that reported first")
	}
	if w := lf.Wanted(); w != (Wanted{}) {
		t.Fatalf("wanted = %+v, want the hidden report kept", w)
	}
}

func TestTheCallbackFiresOnlyOnChange(t *testing.T) {
	lf := newTestForwarder()
	calls := 0
	lf.OnWantedChange(func() { calls++ })

	lf.AddReceiver("a", -1) // unknown 0 → 1
	lf.SetDemand("a", Demand{Width: 640, Height: 360, FPS: 60})
	lf.SetDemand("a", Demand{Width: 640, Height: 360, FPS: 60})
	lf.AddReceiver("a", -1) // already there
	if calls != 2 {
		t.Fatalf("callback ran %d times, want 2", calls)
	}
}
