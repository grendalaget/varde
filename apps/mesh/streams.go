package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/quic-go/quic-go"

	meshv1 "github.com/grendalaget/varde/go/gen/mesh/v1"
	"github.com/grendalaget/varde/go/meshproto"
)

// openStream opens a stream to the peer and performs the
// StreamOpen/StreamAccept handshake.
func (p *peerState) openStream(ctx context.Context, open *meshv1.StreamOpen) (*quic.Stream, error) {
	conn := p.current()
	if conn == nil {
		return nil, errors.New("no path to peer")
	}
	st, err := conn.OpenStreamSync(ctx)
	if err != nil {
		p.n.log.Debug("open stream failed", "peer", p.id, "error", err)
		return nil, err
	}
	if err := meshproto.WriteFrame(st, open); err != nil {
		_ = st.Close()
		return nil, err
	}
	var acc meshv1.StreamAccept
	if err := meshproto.ReadFrame(st, &acc); err != nil {
		_ = st.Close()
		return nil, err
	}
	if !acc.GetOk() {
		_ = st.Close()
		p.n.log.Debug("stream rejected", "peer", p.id, "reason", acc.GetReason(), "msg", acc.GetMessage())
		return nil, fmt.Errorf("stream rejected: %s (%s)", acc.GetReason(), acc.GetMessage())
	}
	return st, nil
}

// streamAcceptLoop dispatches inbound streams on one conn.
func (p *peerState) streamAcceptLoop(conn *quic.Conn) {
	defer p.wg.Done()
	for {
		st, err := conn.AcceptStream(conn.Context())
		if err != nil {
			return
		}
		go p.handleStream(conn, st)
	}
}

func (p *peerState) handleStream(conn *quic.Conn, st *quic.Stream) {
	var open meshv1.StreamOpen
	if err := meshproto.ReadFrame(st, &open); err != nil {
		p.n.log.Debug("read stream open failed", "peer", p.id, "error", err)
		_ = st.Close()
		return
	}
	_ = conn
	switch k := open.GetKind().(type) {
	case *meshv1.StreamOpen_Service:
		p.handleServiceStream(st, k.Service)
	case *meshv1.StreamOpen_UdpFlow:
		p.handleUDPFlowStream(st, k.UdpFlow)
	case *meshv1.StreamOpen_Internal:
		p.handleInternalStream(st, k.Internal)
	case *meshv1.StreamOpen_Control:
		if err := acceptStream(st); err != nil {
			_ = st.Close()
			return
		}
		p.serveControl(st)
	default:
		rejectStream(st, meshv1.RejectReason_REJECT_REASON_UNSPECIFIED, "unknown stream kind")
	}
}

func rejectStream(st *quic.Stream, reason meshv1.RejectReason, msg string) {
	_ = meshproto.WriteFrame(st, &meshv1.StreamAccept{Ok: false, Reason: reason, Message: msg})
	_ = st.Close()
}

func acceptStream(st *quic.Stream) error {
	return meshproto.WriteFrame(st, &meshv1.StreamAccept{Ok: true})
}

// serveControl answers ping/pong on a ControlStream.
func (p *peerState) serveControl(st *quic.Stream) {
	for {
		var cf meshv1.ControlFrame
		if err := meshproto.ReadFrame(st, &cf); err != nil {
			_ = st.Close()
			return
		}
		if ping := cf.GetPing(); ping != nil {
			_ = meshproto.WriteFrame(st, &meshv1.ControlFrame{Frame: &meshv1.ControlFrame_Pong{
				Pong: &meshv1.Pong{Nonce: ping.GetNonce(), SentUnixNanos: ping.GetSentUnixNanos()},
			}})
		}
	}
}

// controlLoop maintains one ControlStream per conn for RTT measurement.
func (p *peerState) controlLoop(conn *quic.Conn) {
	ctx := conn.Context()
	for {
		st, err := conn.OpenStreamSync(ctx)
		if err != nil {
			return
		}
		if err := meshproto.WriteFrame(st, &meshv1.StreamOpen{
			Kind: &meshv1.StreamOpen_Control{Control: &meshv1.ControlStream{}},
		}); err != nil {
			_ = st.Close()
			return
		}
		var acc meshv1.StreamAccept
		if err := meshproto.ReadFrame(st, &acc); err != nil || !acc.GetOk() {
			_ = st.Close()
			return
		}
		p.controlPingLoop(st)
		// stream ended — reopen while the conn is alive
	}
}

func (p *peerState) controlPingLoop(st *quic.Stream) {
	var nonce uint64
	for {
		nonce++
		sent := time.Now()
		err := meshproto.WriteFrame(st, &meshv1.ControlFrame{Frame: &meshv1.ControlFrame_Ping{
			Ping: &meshv1.Ping{Nonce: nonce, SentUnixNanos: sent.UnixNano()},
		}})
		if err != nil {
			_ = st.Close()
			return
		}
		var cf meshv1.ControlFrame
		if err := meshproto.ReadFrame(st, &cf); err != nil {
			_ = st.Close()
			return
		}
		if pong := cf.GetPong(); pong != nil && pong.GetNonce() == nonce {
			rtt := rttMicros(sent)
			p.mu.Lock()
			if p.rttUS == 0 {
				p.rttUS = rtt
			} else {
				p.rttUS = max(p.rttUS*7/8+rtt/8, 1)
			}
			p.mu.Unlock()
			p.lastSeen.Store(time.Now().UnixMilli())
		}
		timer := time.NewTimer(p.n.timings.ControlPingEvery)
		select {
		case <-p.stopCh:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// splice proxies bytes both ways, counting per-peer counters.
func (p *peerState) splice(a *quic.Stream, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		n, _ := io.Copy(b, a)
		p.bytesRx.Add(uint64(n))
		if tc, ok := b.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		a.CancelRead(0)
		done <- struct{}{}
	}()
	go func() {
		n, _ := io.Copy(a, b)
		p.bytesTx.Add(uint64(n))
		_ = a.Close()
		if tc, ok := b.(*net.TCPConn); ok {
			_ = tc.CloseRead()
		}
		done <- struct{}{}
	}()
	<-done
}

// rttMicros is the monotonic time since sent, at least 1us. Windows clocks
// step in tens of microseconds or more, so a loopback round trip can read as
// 0, and 0 means "not measured" to callers.
func rttMicros(sent time.Time) int64 {
	return max(time.Since(sent).Microseconds(), 1)
}
