package auth

import "sync"

// Live is one peer's claims: the join token's at first, then whatever the server last sent
// on the control socket (GRYT-1426). Safe for concurrent use.
type Live struct {
	mu      sync.Mutex
	claims  Claims
	changed chan struct{}
}

// NewLive starts from the claims a verified token carried.
func NewLive(claims Claims) *Live {
	return &Live{claims: claims, changed: make(chan struct{})}
}

// Snapshot returns the claims and a channel the next Set closes. Both under one lock, so a
// change landing between the read and the wait is never missed.
func (l *Live) Snapshot() (Claims, <-chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.claims, l.changed
}

// Set replaces the claims and wakes everything waiting on an earlier Snapshot.
func (l *Live) Set(claims Claims) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.claims = claims
	close(l.changed)
	l.changed = make(chan struct{})
}
