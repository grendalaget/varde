// Package gatewaytunnel multiplexes public gateway connections onto a local TCP service.
package gatewaytunnel

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

type Config struct {
	GatewayURL        string
	Token             string
	TargetAddr        string
	HTTPClient        *http.Client
	AllowInsecureHTTP bool // Loopback-only test mode.
}

// YamuxConfig is shared by both ends of the tunnel, including its heartbeat.
func YamuxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.KeepAliveInterval = 15 * time.Second
	c.ConnectionWriteTimeout = 10 * time.Second
	c.StreamOpenTimeout = 10 * time.Second
	c.StreamCloseTimeout = 5 * time.Second
	c.LogOutput = io.Discard
	return c
}

type Client struct {
	cfg     Config
	mu      sync.Mutex
	token   string
	updates chan struct{}
}

func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.GatewayURL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/v1/tunnel" {
		return nil, errors.New("gatewaytunnel: invalid gateway URL")
	}
	insecure := cfg.AllowInsecureHTTP && u.Scheme == "ws" && net.ParseIP(u.Hostname()).IsLoopback()
	if u.Scheme != "wss" && !insecure {
		return nil, errors.New("gatewaytunnel: gateway requires wss")
	}
	host, port, err := net.SplitHostPort(cfg.TargetAddr)
	if err != nil || !net.ParseIP(host).IsLoopback() || port == "" {
		return nil, errors.New("gatewaytunnel: target must be loopback TCP")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return nil, errors.New("gatewaytunnel: invalid target port")
	}
	if cfg.Token == "" {
		return nil, errors.New("gatewaytunnel: token required")
	}
	return &Client{cfg: cfg, token: cfg.Token, updates: make(chan struct{}, 1)}, nil
}

// UpdateToken reconnects immediately, including when the old token has expired.
func (c *Client) UpdateToken(token string) {
	c.mu.Lock()
	changed := c.token != token
	c.token = token
	c.mu.Unlock()
	if changed {
		select {
		case c.updates <- struct{}{}:
		default:
		}
	}
}

// Run keeps exactly one tunnel up until ctx is cancelled. Call once per client.
func (c *Client) Run(ctx context.Context) {
	delay := 250 * time.Millisecond
	for ctx.Err() == nil {
		c.mu.Lock()
		token := c.token
		c.mu.Unlock()
		attempt, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		started := time.Now()
		go func() { defer close(done); _ = c.connect(attempt, token) }()
		select {
		case <-ctx.Done():
			cancel()
			<-done
			return
		case <-c.updates:
			cancel()
			<-done
			delay = 250 * time.Millisecond
			continue
		case <-done:
			cancel()
		}
		if time.Since(started) > 30*time.Second {
			delay = 250 * time.Millisecond
		}
		timer := time.NewTimer(delay/2 + time.Duration(rand.Int64N(int64(delay/2))))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-c.updates:
			timer.Stop()
			delay = 250 * time.Millisecond
			continue
		case <-timer.C:
		}
		delay = min(delay*2, 30*time.Second)
	}
}

func (c *Client) connect(ctx context.Context, token string) error {
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	ws, resp, err := websocket.Dial(dialCtx, c.cfg.GatewayURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}}, HTTPClient: c.cfg.HTTPClient})
	cancel()
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return errors.New("gatewaytunnel: tunnel dial failed")
	}
	defer func() { _ = ws.CloseNow() }()
	conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	session, err := yamux.Server(conn, YamuxConfig())
	if err != nil {
		return err
	}
	defer func() { _ = session.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = session.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		stream, err := session.AcceptStream()
		if err != nil {
			return err
		}
		wg.Add(1)
		go func() { defer wg.Done(); c.pipe(ctx, stream) }()
	}
}

func (c *Client) pipe(ctx context.Context, stream *yamux.Stream) {
	defer func() { _ = stream.Close() }()
	local, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", c.cfg.TargetAddr)
	if err != nil {
		return
	}
	defer func() { _ = local.Close() }()
	closeBoth := func() { _ = local.Close(); _ = stream.Close() }
	stop := context.AfterFunc(ctx, closeBoth)
	defer stop()
	done := make(chan struct{})
	go func() { _, _ = io.Copy(local, stream); closeBoth(); close(done) }()
	_, _ = io.Copy(stream, local)
	closeBoth()
	<-done
}
