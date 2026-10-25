package api

import "sync"

// Device link user codes (WORD-NN-WORD) have ~1.1M combinations, so failed
// lookups are capped per user and per client IP to make guessing a stranger's
// pending code impractical.
const (
	deviceLinkFailLimit    = 10
	deviceLinkFailWindowMs = 60_000
)

// failLimiter counts failures per key in fixed windows. A key at the limit is
// refused until its window ends.
//
// shortcut: in-memory and per-process (10 failed device-link lookups per
// minute per user and per IP). On multi-replica deployments each replica
// counts separately, so the effective limit is replicas × 10.
type failLimiter struct {
	limit    int
	windowMs int64

	mu        sync.Mutex
	windows   map[string]failWindow
	lastSweep int64
}

type failWindow struct {
	start int64
	fails int
}

func newFailLimiter(limit int, windowMs int64) *failLimiter {
	return &failLimiter{limit: limit, windowMs: windowMs, windows: map[string]failWindow{}}
}

// Allowed reports whether none of keys is at the limit.
func (l *failLimiter) Allowed(now int64, keys ...string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		if w, ok := l.windows[k]; ok && now-w.start < l.windowMs && w.fails >= l.limit {
			return false
		}
	}
	return true
}

// Fail records one failure for each key.
func (l *failLimiter) Fail(now int64, keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// drop expired windows at most once per window so the map only holds
	// keys that failed recently
	if now-l.lastSweep >= l.windowMs {
		for k, w := range l.windows {
			if now-w.start >= l.windowMs {
				delete(l.windows, k)
			}
		}
		l.lastSweep = now
	}
	for _, k := range keys {
		w, ok := l.windows[k]
		if !ok || now-w.start >= l.windowMs {
			w = failWindow{start: now}
		}
		w.fails++
		l.windows[k] = w
	}
}

func (l *failLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.windows)
}
