package main

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	meshv1 "github.com/grendalaget/varde/go/gen/mesh/v1"
)

// internalForward is a 127.0.0.1 TCP listener tunnelling to a peer's
// internal service.
type internalForward struct {
	ln   net.Listener
	peer *peerState
	name string
	once sync.Once
}

func (f *internalForward) close() {
	f.once.Do(func() { _ = f.ln.Close() })
}

// SetInternalServices replaces the local internal-service table.
func (n *Node) SetInternalServices(req *meshv1.SetInternalServicesRequest) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.internal = map[string]uint32{}
	for _, s := range req.GetServices() {
		n.internal[s.GetName()] = s.GetTargetPort()
	}
}

func (n *Node) BindInternalForward(peerID, name string) (string, string, error) {
	peer := n.peer(peerID)
	if peer == nil {
		return "", "", fmt.Errorf("unknown peer %q", peerID)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", err
	}
	f := &internalForward{ln: ln, peer: peer, name: name}
	id := fmt.Sprintf("fwd_%d", time.Now().UnixNano())
	n.mu.Lock()
	n.forwards[id] = f
	n.mu.Unlock()
	go f.acceptLoop()
	return id, ln.Addr().String(), nil
}

func (f *internalForward) acceptLoop() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.serve(c)
	}
}

func (f *internalForward) serve(c net.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := f.peer.openStream(ctx, &meshv1.StreamOpen{Kind: &meshv1.StreamOpen_Internal{
		Internal: &meshv1.InternalStream{Name: f.name},
	}})
	if err != nil {
		_ = c.Close()
		return
	}
	f.peer.splice(st, c)
}

func (n *Node) UnbindInternalForward(id string) bool {
	n.mu.Lock()
	f, ok := n.forwards[id]
	delete(n.forwards, id)
	n.mu.Unlock()
	if ok {
		f.close()
	}
	return ok
}
