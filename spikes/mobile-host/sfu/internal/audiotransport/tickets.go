package audiotransport

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// A ticket is how a WebTransport session proves which peer it is. It is handed out over the
// already-authenticated signalling WebSocket after a join, used once, and gone after ticketTTL.
const ticketTTL = 30 * time.Second

type ticket struct {
	peerID  string
	roomID  string
	expires time.Time
}

type tickets struct {
	mu sync.Mutex
	m  map[string]ticket
}

func newTickets() *tickets { return &tickets{m: map[string]ticket{}} }

func (t *tickets) issue(peerID, roomID string, now time.Time) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, v := range t.m {
		if now.After(v.expires) {
			delete(t.m, k)
		}
	}
	t.m[token] = ticket{peerID: peerID, roomID: roomID, expires: now.Add(ticketTTL)}
	return token, nil
}

// redeem returns the ticket's peer and room once; a second try, or one after it expired, fails.
func (t *tickets) redeem(token string, now time.Time) (ticket, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	v, ok := t.m[token]
	delete(t.m, token)
	if !ok || now.After(v.expires) {
		return ticket{}, false
	}
	return v, true
}
