package websocket

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gorilla "github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"

	"sfu-v2/internal/config"
	"sfu-v2/pkg/types"
)

// newTestSocketPair gives back the two ends of a real WebSocket: the ThreadSafeWriter the
// handler writes offers to, and the client end the test reads them from.
func newTestSocketPair(t *testing.T) (*ThreadSafeWriter, *gorilla.Conn) {
	t.Helper()

	serverConnCh := make(chan *gorilla.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		serverConnCh <- conn
	}))
	t.Cleanup(srv.Close)

	clientConn, _, err := gorilla.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })

	select {
	case serverConn := <-serverConnCh:
		t.Cleanup(func() { _ = serverConn.Close() })
		return NewThreadSafeWriter(serverConn), clientConn
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the server side of the WebSocket")
		return nil, nil
	}
}

// newTestPeer gives back a peer connection with one video transceiver, so CreateOffer has
// something to describe. No ICE: every assertion here is about signaling state.
func newTestPeer(t *testing.T) *webrtc.PeerConnection {
	t.Helper()

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("new peer connection: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
		t.Fatalf("add transceiver: %v", err)
	}
	return pc
}

func newTestHandler() *Handler {
	return &Handler{config: &config.Config{}}
}

// readNextMessage reads one message off the client end, or reports that none
// arrived within the deadline.
func readNextMessage(t *testing.T, conn *gorilla.Conn, within time.Duration) (*types.WebSocketMessage, bool) {
	t.Helper()

	if err := conn.SetReadDeadline(time.Now().Add(within)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}

	message := &types.WebSocketMessage{}
	if err := conn.ReadJSON(message); err != nil {
		return nil, false
	}
	return message, true
}

func TestRenegotiateOrDeferOffersWhenStable(t *testing.T) {
	h := newTestHandler()
	pc := newTestPeer(t)
	serverConn, clientConn := newTestSocketPair(t)

	if pc.SignalingState() != webrtc.SignalingStateStable {
		t.Fatalf("expected a fresh peer to be stable, got %s", pc.SignalingState())
	}

	if stillPending := h.renegotiateOrDefer(pc, serverConn, "client-a", "room-1"); stillPending {
		t.Fatal("a renegotiation on a stable peer should be served, not left pending")
	}

	message, ok := readNextMessage(t, clientConn, 5*time.Second)
	if !ok {
		t.Fatal("no offer reached the client")
	}
	if message.Event != types.EventOffer {
		t.Fatalf("expected an offer, got event %q", message.Event)
	}
}

func TestRenegotiateOrDeferKeepsRequestWhenNotStable(t *testing.T) {
	h := newTestHandler()
	pc := newTestPeer(t)
	serverConn, clientConn := newTestSocketPair(t)

	// An offer the SFU has sent but the client has not answered yet — the
	// state a client's renegotiate request lands in when it arrives mid-flight.
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local description: %v", err)
	}
	if pc.SignalingState() != webrtc.SignalingStateHaveLocalOffer {
		t.Fatalf("expected have-local-offer, got %s", pc.SignalingState())
	}

	if stillPending := h.renegotiateOrDefer(pc, serverConn, "client-a", "room-1"); !stillPending {
		t.Fatal("a renegotiation refused for signaling state must stay pending — dropping it is GRYT-32")
	}

	if _, ok := readNextMessage(t, clientConn, 250*time.Millisecond); ok {
		t.Fatal("nothing should have been offered while the peer was not stable")
	}
}

func TestRenegotiateOrDeferKeepsRequestWhenSendFails(t *testing.T) {
	h := newTestHandler()
	pc := newTestPeer(t)
	serverConn, clientConn := newTestSocketPair(t)

	// The peer is perfectly stable; the failure is in delivering the offer.
	// The request is still owed either way.
	_ = clientConn.Close()
	_ = serverConn.Close()

	if stillPending := h.renegotiateOrDefer(pc, serverConn, "client-a", "room-1"); !stillPending {
		t.Fatal("a renegotiation whose offer never went out must stay pending")
	}
}
