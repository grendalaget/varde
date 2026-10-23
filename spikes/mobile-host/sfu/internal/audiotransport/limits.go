package audiotransport

import "time"

// The datagram the voice engine sends (@gryt/voice transport/packet.ts): an 8-byte header, version
// first, then one Opus packet. Only its size and version are checked here; the audio is not read.
const (
	packetVersion = 1
	headerBytes   = 8
	maxOpusBytes  = 1275
	// The sender id the hub puts in front of a forwarded packet.
	senderBytes = 4
)

// validDatagram is the only parsing the SFU does on audio: anything else is dropped unread.
func validDatagram(b []byte) bool {
	return len(b) > headerBytes && len(b) <= headerBytes+maxOpusBytes && b[0] == packetVersion
}

// One sender at 2.5 ms frames sends 400 a second. A little over that, and a short burst, are allowed;
// more is dropped, so one client can't flood a room that fans it out to everyone.
const (
	ratePerSecond = 500
	rateBurst     = 50
)

type bucket struct {
	tokens float64
	last   time.Time
}

func (b *bucket) allow(now time.Time) bool {
	if b.last.IsZero() {
		b.tokens = rateBurst
	} else {
		b.tokens += now.Sub(b.last).Seconds() * ratePerSecond
		if b.tokens > rateBurst {
			b.tokens = rateBurst
		}
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
