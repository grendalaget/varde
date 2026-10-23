// Package audiotransport is the own voice transport's SFU side (GRYT-1818): WebTransport datagrams
// over QUIC, fanned out per room. Video and screen share stay on WebRTC.
package audiotransport

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/quic-go/quic-go/http3"
)

// The HTTP/3 settings a browser checks before it opens a WebTransport session (draft-ietf-webtrans-http3).
const (
	settingEnableWebTransportDraft06 = 0x2b603742
	settingWebTransportEnabled       = 0x2c7cf000
	settingWebTransportMaxSessions   = 0x14e9cd29
)

// Offer is what a client needs to open its audio session, sent over the signalling WebSocket.
type Offer struct {
	URLs     []string `json:"urls"`
	CertHash string   `json:"certHash"`
	Ticket   string   `json:"ticket"`
	// The native engine's plain UDP, when the listener runs (GRYT-1822).
	UDP *UDPOffer `json:"udp,omitempty"`
}

type Server struct {
	port    int
	hosts   []string
	certs   *certStore
	tickets *tickets
	hub     *hub
	h3      *http3.Server

	mu  sync.Mutex
	udp *udpListener
}

// New listens on UDP port and tells clients to reach it at each of hosts (the SFU's public addresses).
func New(port int, hosts []string) *Server {
	var ips []net.IP
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			ips = append(ips, ip)
		}
	}
	s := &Server{port: port, hosts: hosts, certs: newCertStore(ips), tickets: newTickets(), hub: newHub()}
	mux := http.NewServeMux()
	mux.HandleFunc("/audio", s.handle)
	s.h3 = &http3.Server{
		Addr:    ":" + strconv.Itoa(port),
		Handler: mux,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			NextProtos: []string{http3.NextProtoH3},
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				cert, _, err := s.certs.current(time.Now())
				return cert, err
			},
		},
		EnableDatagrams: true,
		AdditionalSettings: map[uint64]uint64{
			settingEnableWebTransportDraft06: 1,
			settingWebTransportEnabled:       1,
			settingWebTransportMaxSessions:   1,
		},
	}
	return s
}

// OnSenders calls fn with a room's sender ids, by peer, whenever someone's session joins or leaves
// it, so clients can tell whose packet is whose (GRYT-1830). Set it before serving.
func (s *Server) OnSenders(fn func(roomID string, senders map[string]uint32)) {
	s.hub.changed = func(roomID string) { fn(roomID, s.hub.senders(roomID)) }
}

// Senders is who sends on the transport in a room right now, by peer.
func (s *Server) Senders(roomID string) map[string]uint32 { return s.hub.senders(roomID) }

// ListenAndServe blocks; run it in a goroutine.
func (s *Server) ListenAndServe() error { return s.h3.ListenAndServe() }

func (s *Server) Close() error {
	s.mu.Lock()
	u := s.udp
	s.mu.Unlock()
	err := s.h3.Close()
	if u != nil {
		if udpErr := u.conn.Close(); udpErr != nil && err == nil {
			err = udpErr
		}
	}
	return err
}

// Offer hands a joined peer a ticket for its audio session.
func (s *Server) Offer(peerID, roomID string) (Offer, error) {
	_, hash, err := s.certs.current(time.Now())
	if err != nil {
		return Offer{}, err
	}
	ticket, err := s.tickets.issue(peerID, roomID, time.Now())
	if err != nil {
		return Offer{}, err
	}
	urls := make([]string, 0, len(s.hosts))
	for _, h := range s.hosts {
		urls = append(urls, fmt.Sprintf("https://%s/audio", net.JoinHostPort(h, strconv.Itoa(s.port))))
	}
	offer := Offer{URLs: urls, CertHash: hash, Ticket: ticket}
	s.mu.Lock()
	u := s.udp
	s.mu.Unlock()
	if u != nil {
		if offer.UDP, err = u.offer(peerID, roomID, time.Now()); err != nil {
			return Offer{}, err
		}
	}
	return offer, nil
}

// Drop ends a peer's audio session; called when it leaves voice.
func (s *Server) Drop(peerID string) {
	s.hub.drop(peerID)
	s.mu.Lock()
	u := s.udp
	s.mu.Unlock()
	if u != nil {
		u.forget(peerID)
	}
}

func isWebTransport(r *http.Request) bool {
	return r.Method == http.MethodConnect && (r.Proto == "webtransport-h3" || r.Proto == "webtransport")
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if !isWebTransport(r) {
		http.Error(w, "webtransport only", http.StatusBadRequest)
		return
	}
	t, ok := s.tickets.redeem(r.URL.Query().Get("ticket"), time.Now())
	if !ok {
		http.Error(w, "no such ticket", http.StatusUnauthorized)
		return
	}
	settingser, ok := w.(http3.Settingser)
	if !ok {
		http.Error(w, "not http/3", http.StatusBadRequest)
		return
	}
	select {
	case <-settingser.ReceivedSettings():
	case <-time.After(5 * time.Second):
		http.Error(w, "no settings", http.StatusBadRequest)
		return
	}
	if !settingser.Settings().EnableDatagrams {
		http.Error(w, "datagrams required", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	str := w.(http3.HTTPStreamer).HTTPStream()
	ctx, cancel := context.WithCancel(r.Context())
	m := s.hub.join(t.roomID, t.peerID, str, func() {
		cancel()
		str.CancelRead(0)
		_ = str.Close()
	})
	defer s.hub.leave(m)
	defer cancel()
	for {
		b, err := str.ReceiveDatagram(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("[AUDIO] session %s ended: %v", t.peerID, err)
			}
			return
		}
		s.hub.forward(m, b, time.Now())
	}
}
