package audiotransport

import (
	"crypto/cipher"
	"encoding/binary"
)

// The native engine's encrypted datagram (@gryt/voice native/core/seal.c, GRYT-1822): session u32, counter u32,
// the clear part as associated data, the Opus payload sealed with ChaCha20-Poly1305 (RFC 8439), a 16-byte tag.
const (
	sealPrefix = 8
	tagBytes   = 16
	dirToSFU   = 0
	dirFromSFU = 1
)

// The nonce is the direction, three zero bytes, then the counter as a u64; a counter never repeats under one key.
func sealNonce(dir byte, counter uint32) []byte {
	n := make([]byte, 12)
	n[0] = dir
	binary.BigEndian.PutUint32(n[8:], counter)
	return n
}

func seal(aead cipher.AEAD, dir byte, session, counter uint32, clear, secret []byte) []byte {
	ad := make([]byte, sealPrefix+len(clear))
	binary.BigEndian.PutUint32(ad, session)
	binary.BigEndian.PutUint32(ad[4:], counter)
	copy(ad[sealPrefix:], clear)
	out := make([]byte, len(ad), len(ad)+len(secret)+tagBytes)
	copy(out, ad)
	return aead.Seal(out, sealNonce(dir, counter), secret, ad)
}

// replay refuses a counter it has seen, or one more than 64 behind the newest, as seal.c does.
type replay struct {
	top     uint32
	seen    uint64
	started bool
}

func (r *replay) fresh(c uint32) bool {
	if !r.started || c > r.top {
		return true
	}
	back := r.top - c
	return back < 64 && r.seen>>back&1 == 0
}

func (r *replay) mark(c uint32) {
	switch {
	case !r.started:
		r.started, r.top, r.seen = true, c, 1
	case c > r.top:
		if shift := c - r.top; shift >= 64 {
			r.seen = 1
		} else {
			r.seen = r.seen<<shift | 1
		}
		r.top = c
	default:
		r.seen |= 1 << (r.top - c)
	}
}

// open checks the tag, then the replay window, which only a genuine packet may move. It returns the clear part and
// the decrypted payload, or ok false for a forgery, a replay or garbage.
func open(aead cipher.AEAD, dir byte, b []byte, clearLen int, r *replay) (clear, secret []byte, ok bool) {
	ad := sealPrefix + clearLen
	if len(b) < ad+tagBytes {
		return nil, nil, false
	}
	counter := binary.BigEndian.Uint32(b[4:])
	if !r.fresh(counter) {
		return nil, nil, false
	}
	secret, err := aead.Open(nil, sealNonce(dir, counter), b[ad:], b[:ad])
	if err != nil {
		return nil, nil, false
	}
	r.mark(counter)
	return b[sealPrefix:ad], secret, true
}
