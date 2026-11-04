package gateway

import (
	"net"
	"sync"
	"time"
)

// WithReadIdleTimeout bounds network reads without limiting the total upload
// duration. Explicit earlier deadlines, including HTTP header deadlines, win.
// A nonpositive timeout leaves the listener unchanged.
func WithReadIdleTimeout(listener net.Listener, timeout time.Duration) net.Listener {
	if timeout <= 0 {
		return listener
	}
	return &readIdleListener{Listener: listener, timeout: timeout}
}

type readIdleListener struct {
	net.Listener
	timeout time.Duration
}

func (l *readIdleListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &readIdleConn{Conn: conn, timeout: l.timeout}, nil
}

type readIdleConn struct {
	net.Conn
	timeout  time.Duration
	mu       sync.Mutex
	deadline time.Time
}

func (c *readIdleConn) setDeadline() error {
	deadline := time.Now().Add(c.timeout)
	if !c.deadline.IsZero() && c.deadline.Before(deadline) {
		deadline = c.deadline
	}
	return c.Conn.SetReadDeadline(deadline)
}

func (c *readIdleConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	err := c.setDeadline()
	c.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return c.Conn.Read(b)
}

func (c *readIdleConn) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = deadline
	return c.setDeadline()
}

func (c *readIdleConn) SetDeadline(deadline time.Time) error {
	if err := c.SetReadDeadline(deadline); err != nil {
		return err
	}
	return c.SetWriteDeadline(deadline)
}
