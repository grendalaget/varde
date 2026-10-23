package audiotransport

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

// UDPOffer is the native engine's way in (GRYT-1822): plain UDP, every packet sealed with a key that only the
// signalling WebSocket ever carried. No handshake: a packet that opens with the key is the proof.
type UDPOffer struct {
	Addrs   []string `json:"addrs"`
	Session uint32   `json:"session"`
	Key     string   `json:"key"`
}

// An offered session that never sends a valid packet is forgotten after this, like a ticket.
const udpSessionTTL = 30 * time.Second

type udpSession struct {
	id             uint32
	aead           cipher.AEAD
	peerID, roomID string
	created        time.Time
	in             replay
	conn           *net.UDPConn

	mu     sync.Mutex
	addr   *net.UDPAddr
	out    uint32
	member *member
}

// SendDatagram seals a forwarded packet (sender id, header, Opus) for this receiver and sends it to the address its
// own last genuine packet came from, so a phone that changes network is followed.
func (s *udpSession) SendDatagram(b []byte) error {
	s.mu.Lock()
	addr := s.addr
	s.out++
	counter := s.out
	s.mu.Unlock()
	if addr == nil || len(b) < senderBytes+headerBytes {
		return errors.New("no address yet")
	}
	_, err := s.conn.WriteToUDP(seal(s.aead, dirFromSFU, s.id, counter, b[:senderBytes+headerBytes], b[senderBytes+headerBytes:]), addr)
	return err
}

func (s *udpSession) joined() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.member != nil
}

type udpListener struct {
	conn  *net.UDPConn
	hosts []string
	port  int
	hub   *hub

	mu       sync.Mutex
	sessions map[uint32]*udpSession
	// Counters, for metrics and tests.
	dropped struct{ unknown, forged uint64 }
}

// ListenUDP opens the native engine's listener on port next to WebTransport, sharing its rooms. Blocks.
func (s *Server) ListenUDP(port int) error {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
	if err != nil {
		return err
	}
	u := &udpListener{conn: conn, hosts: s.hosts, port: port, hub: s.hub, sessions: map[uint32]*udpSession{}}
	s.mu.Lock()
	s.udp = u
	s.mu.Unlock()
	return u.serve()
}

func (u *udpListener) offer(peerID, roomID string, now time.Time) (*UDPOffer, error) {
	key := make([]byte, chacha20poly1305.KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for id, s := range u.sessions {
		if !s.joined() && now.Sub(s.created) > udpSessionTTL {
			delete(u.sessions, id)
		}
	}
	var id uint32
	for id == 0 || u.sessions[id] != nil {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		id = binary.BigEndian.Uint32(b[:])
	}
	u.sessions[id] = &udpSession{id: id, aead: aead, peerID: peerID, roomID: roomID, created: now, conn: u.conn}
	addrs := make([]string, 0, len(u.hosts))
	for _, h := range u.hosts {
		addrs = append(addrs, net.JoinHostPort(h, strconv.Itoa(u.port)))
	}
	return &UDPOffer{Addrs: addrs, Session: id, Key: base64.StdEncoding.EncodeToString(key)}, nil
}

// forget removes a peer's sessions that never joined; joined ones go with hub.drop.
func (u *udpListener) forget(peerID string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for id, s := range u.sessions {
		if s.peerID == peerID && !s.joined() {
			delete(u.sessions, id)
		}
	}
}

func (u *udpListener) serve() error {
	buf := make([]byte, 2048)
	for {
		n, addr, err := u.conn.ReadFromUDP(buf)
		if err != nil {
			return err
		}
		u.receive(buf[:n], addr, time.Now())
	}
}

// receive opens one datagram and forwards it. Nothing is read before the tag holds except the session id.
func (u *udpListener) receive(b []byte, addr *net.UDPAddr, now time.Time) {
	if len(b) < sealPrefix+headerBytes+tagBytes {
		return
	}
	u.mu.Lock()
	s := u.sessions[binary.BigEndian.Uint32(b)]
	u.mu.Unlock()
	if s == nil {
		u.mu.Lock()
		u.dropped.unknown++
		u.mu.Unlock()
		return
	}
	s.mu.Lock()
	clear, secret, ok := open(s.aead, dirToSFU, b, headerBytes, &s.in)
	if !ok {
		s.mu.Unlock()
		u.mu.Lock()
		u.dropped.forged++
		u.mu.Unlock()
		return
	}
	s.addr = addr
	m := s.member
	s.mu.Unlock()
	if m == nil {
		m = u.hub.join(s.roomID, s.peerID, s, func() {
			u.mu.Lock()
			delete(u.sessions, s.id)
			u.mu.Unlock()
		})
		s.mu.Lock()
		s.member = m
		s.mu.Unlock()
	}
	u.hub.forward(m, append(append([]byte{}, clear...), secret...), now)
}
