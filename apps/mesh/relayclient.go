package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"

	meshv1 "github.com/grendalaget/varde/go/gen/mesh/v1"
	"github.com/grendalaget/varde/go/meshproto"
)

// relayPipe is a virtual net.PacketConn carrying a whole relayed QUIC
// transport for one relay: outbound writes get wrapped in DATA frames to the
// relay; inbound relay DATA payloads land on in.
type relayPipe struct {
	n     *Node
	relay *relayClient
	in    chan pipePacket
	done  chan struct{}
	once  sync.Once
}

type pipePacket struct {
	data []byte
	src  virtualAddr
}

// virtualAddr identifies a remote node on the relay pipe.
type virtualAddr string

func (v virtualAddr) Network() string { return "varde-relay" }
func (v virtualAddr) String() string  { return string(v) }

func (p *relayPipe) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case pkt := <-p.in:
		return copy(b, pkt.data), pkt.src, nil
	case <-p.done:
		return 0, nil, net.ErrClosed
	}
}

func (p *relayPipe) WriteTo(b []byte, addr net.Addr) (int, error) {
	dst, ok := addr.(virtualAddr)
	if !ok {
		return 0, fmt.Errorf("relayPipe: bad addr %T", addr)
	}
	dgram := meshproto.EncodeDataBytes(string(dst), b)
	if _, err := p.relay.n.sock.WriteToUDP(dgram, p.relay.addr); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (p *relayPipe) Close() error {
	p.once.Do(func() { close(p.done) })
	return nil
}

func (p *relayPipe) LocalAddr() net.Addr              { return p.relay.addr }
func (p *relayPipe) SetDeadline(time.Time) error      { return nil }
func (p *relayPipe) SetReadDeadline(time.Time) error  { return nil }
func (p *relayPipe) SetWriteDeadline(time.Time) error { return nil }

// relayClient keeps a registration alive and hosts the relayed QUIC
// transport for this relay.
type relayClient struct {
	n    *Node
	id   string
	addr *net.UDPAddr

	mu       sync.RWMutex
	token    string
	reg      bool
	expires  int64
	rttUS    int64
	observed []string

	pipe *relayPipe
	tr   *quic.Transport
	ln   *quic.Listener

	pingNonce atomic.Uint64
	pingSent  sync.Map // nonce -> sent time.Time
	stopCh    chan struct{}
	stopped   atomic.Bool
	wg        sync.WaitGroup
}

func (n *Node) newRelayClientLocked(id, addr, token string) (*relayClient, error) {
	ua, err := resolveUDP(addr)
	if err != nil {
		return nil, err
	}
	rc := &relayClient{
		n: n, id: id, addr: ua, token: token,
		stopCh: make(chan struct{}),
	}
	rc.pipe = &relayPipe{n: n, relay: rc, in: make(chan pipePacket, 256), done: make(chan struct{})}
	rc.tr = &quic.Transport{Conn: rc.pipe}
	rc.ln, err = rc.tr.Listen(n.tlsConfigFor(""), quicConfig())
	if err != nil {
		return nil, err
	}
	rc.wg.Add(3)
	go rc.registerLoop()
	go rc.pingLoop()
	go rc.acceptLoop()
	return rc, nil
}

func (rc *relayClient) setToken(tok string) {
	rc.mu.Lock()
	rc.token = tok
	rc.mu.Unlock()
}

func (rc *relayClient) registered() bool {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.reg && rc.expires > time.Now().UnixMilli()
}

func (rc *relayClient) rtt() int64 {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	if rc.rttUS == 0 {
		return 1 << 30 // unmeasured sorts last
	}
	return rc.rttUS
}

func (rc *relayClient) state() (*meshv1.RelayState, []string) {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return &meshv1.RelayState{
		RelayId: rc.id, Addr: rc.addr.String(),
		Registered: rc.reg && rc.expires > time.Now().UnixMilli(), RttUs: rc.rttUS,
	}, append([]string(nil), rc.observed...)
}

func (rc *relayClient) stop() {
	if !rc.stopped.CompareAndSwap(false, true) {
		return
	}
	close(rc.stopCh)
	// the pipe must die first: the transport's readers/writers block on it
	_ = rc.pipe.Close()
	_ = rc.ln.Close()
	_ = rc.tr.Close()
	rc.wg.Wait()
}

func (rc *relayClient) registerLoop() {
	defer rc.wg.Done()
	for {
		rc.sendRegister()
		select {
		case <-rc.stopCh:
			return
		case <-time.After(rc.n.timings.RelayRegisterEvery):
		}
	}
}

func (rc *relayClient) sendRegister() {
	rc.mu.RLock()
	tok := rc.token
	rc.mu.RUnlock()
	now := time.Now().UnixMilli()
	reg := &meshv1.RelayRegister{
		Token: tok, NodeId: rc.n.nodeID, TimestampUnixMs: now,
		Signature: meshproto.SignRegister(rc.n.priv, rc.id, rc.n.nodeID, now),
		RelayId:   rc.id,
	}
	d, err := meshproto.EncodeProtoDgram(meshproto.DgramRegister, reg)
	if err != nil {
		return
	}
	_, _ = rc.n.sock.WriteToUDP(d, rc.addr)
}

func (rc *relayClient) pingLoop() {
	defer rc.wg.Done()
	for {
		select {
		case <-rc.stopCh:
			return
		case <-time.After(rc.n.timings.RelayPingEvery):
		}
		nonce := rc.pingNonce.Add(1)
		rc.pingSent.Store(nonce, time.Now())
		_, _ = rc.n.sock.WriteToUDP(meshproto.EncodePing(nonce), rc.addr)
	}
}

func (rc *relayClient) acceptLoop() {
	defer rc.wg.Done()
	for {
		conn, err := rc.ln.Accept(context.Background())
		if err != nil {
			return
		}
		go rc.n.registerConn(conn, meshv1.PathKind_PATH_KIND_RELAYED)
	}
}

// dial opens a QUIC conn through this relay to dstNodeID.
func (rc *relayClient) dial(ctx context.Context, dstNodeID string, tlsConf *tls.Config) (*quic.Conn, error) {
	return rc.tr.Dial(ctx, virtualAddr(dstNodeID), tlsConf, quicConfig())
}

// handleFrame processes REGISTERED / PONG / ERROR / DATA arriving on the
// shared socket from this relay.
func (rc *relayClient) handleFrame(data []byte) {
	switch data[0] {
	case meshproto.DgramRegistered:
		var rr meshv1.RelayRegistered
		if _, err := meshproto.DecodeProtoDgram(data, &rr); err != nil {
			return
		}
		rc.mu.Lock()
		wasReg := rc.reg
		rc.reg = true
		rc.expires = rr.GetExpiresUnixMs()
		rc.observed = dedupe(append(rc.observed, rr.GetObservedAddr()))
		rc.mu.Unlock()
		if !wasReg {
			rc.n.events.emit(&meshv1.MeshEvent{
				AtUnixMs: time.Now().UnixMilli(),
				Event:    &meshv1.MeshEvent_RelayChanged{RelayChanged: mustState(rc)},
			})
			rc.n.events.emit(&meshv1.MeshEvent{
				AtUnixMs: time.Now().UnixMilli(),
				Event: &meshv1.MeshEvent_ObservedEndpointsChanged{
					ObservedEndpointsChanged: rr.GetObservedAddr(),
				},
			})
		}
	case meshproto.DgramPong:
		_, nonce, err := meshproto.DecodeNonce(data)
		if err != nil {
			return
		}
		if sentV, ok := rc.pingSent.LoadAndDelete(nonce); ok {
			rtt := rttMicros(sentV.(time.Time))
			rc.mu.Lock()
			if rc.rttUS == 0 {
				rc.rttUS = rtt
			} else {
				rc.rttUS = max(rc.rttUS*7/8+rtt/8, 1)
			}
			rc.mu.Unlock()
		}
	case meshproto.DgramData:
		src, payload, err := meshproto.DecodeData(data)
		if err != nil {
			return
		}
		rc.n.log.Debug("relay data in", "relay", rc.id, "src", src, "bytes", len(payload))
		select {
		case rc.pipe.in <- pipePacket{data: payload, src: virtualAddr(src)}:
		default:
		}
	case meshproto.DgramError:
		var re meshv1.RelayError
		if _, err := meshproto.DecodeProtoDgram(data, &re); err == nil {
			rc.n.log.Warn("relay error", "relay", rc.id, "code", re.GetCode(), "msg", re.GetMessage())
		}
	}
}

func mustState(rc *relayClient) *meshv1.RelayState {
	st, _ := rc.state()
	return st
}
