package main

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	meshv1 "github.com/arnemolland/p2pgames/go/gen/mesh/v1"
)

// meshServer is the MeshService gRPC surface on top of Node.
type meshServer struct {
	meshv1.UnimplementedMeshServiceServer

	n   *Node
	log *slog.Logger
}

func newMeshServer(log *slog.Logger, n *Node) *meshServer {
	return &meshServer{log: log, n: n}
}

func (s *meshServer) GetStatus(context.Context, *meshv1.GetStatusRequest) (*meshv1.MeshStatus, error) {
	return s.n.status(), nil
}

func (s *meshServer) Configure(_ context.Context, req *meshv1.ConfigureRequest) (*meshv1.ConfigureResponse, error) {
	if err := s.n.Configure(req); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	s.log.Debug("configured", "node_id", req.GetNodeId(),
		"relays", len(req.GetRelays()), "force_relay", req.GetForceRelay())
	return &meshv1.ConfigureResponse{}, nil
}

func (s *meshServer) SetPeers(_ context.Context, req *meshv1.SetPeersRequest) (*meshv1.SetPeersResponse, error) {
	s.n.SetPeers(req)
	return &meshv1.SetPeersResponse{}, nil
}

func (s *meshServer) ListPeers(context.Context, *meshv1.ListPeersRequest) (*meshv1.ListPeersResponse, error) {
	var out []*meshv1.PeerState
	s.n.mu.RLock()
	for _, p := range s.n.peers {
		out = append(out, p.state())
	}
	s.n.mu.RUnlock()
	return &meshv1.ListPeersResponse{Peers: out}, nil
}

func (s *meshServer) SetRoutes(_ context.Context, req *meshv1.SetRoutesRequest) (*meshv1.SetRoutesResponse, error) {
	return &meshv1.SetRoutesResponse{Errors: s.n.SetRoutes(req)}, nil
}

func (s *meshServer) SetHostedServices(_ context.Context, req *meshv1.SetHostedServicesRequest) (*meshv1.SetHostedServicesResponse, error) {
	s.n.SetHostedServices(req)
	return &meshv1.SetHostedServicesResponse{}, nil
}

func (s *meshServer) SetInternalServices(_ context.Context, req *meshv1.SetInternalServicesRequest) (*meshv1.SetInternalServicesResponse, error) {
	s.n.SetInternalServices(req)
	return &meshv1.SetInternalServicesResponse{}, nil
}

func (s *meshServer) BindInternalForward(_ context.Context, req *meshv1.BindInternalForwardRequest) (*meshv1.BindInternalForwardResponse, error) {
	id, addr, err := s.n.BindInternalForward(req.GetPeerNodeId(), req.GetName())
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return &meshv1.BindInternalForwardResponse{ForwardId: id, LocalAddr: addr}, nil
}

func (s *meshServer) UnbindInternalForward(_ context.Context, req *meshv1.UnbindInternalForwardRequest) (*meshv1.UnbindInternalForwardResponse, error) {
	if !s.n.UnbindInternalForward(req.GetForwardId()) {
		return nil, status.Error(codes.NotFound, "unknown forward")
	}
	return &meshv1.UnbindInternalForwardResponse{}, nil
}

func (s *meshServer) WatchEvents(_ *meshv1.WatchEventsRequest, stream meshv1.MeshService_WatchEventsServer) error {
	ch := s.n.events.subscribe()
	defer s.n.events.unsubscribe(ch)
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case ev := <-ch:
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
	}
}

// SetPeers applies a full replacement of the peer set.
func (n *Node) SetPeers(req *meshv1.SetPeersRequest) {
	want := map[string]*meshv1.Peer{}
	for _, p := range req.GetPeers() {
		want[p.GetNodeId()] = p
	}
	var removed []*peerState
	var added []*peerState
	n.mu.Lock()
	for id, p := range n.peers {
		if _, ok := want[id]; !ok {
			removed = append(removed, p)
			delete(n.peers, id)
			delete(n.peersByPub, p.pubB64)
		}
	}
	for _, pp := range req.GetPeers() {
		if pp.GetNodeId() == n.nodeID {
			continue // don't dial ourselves
		}
		if existing, ok := n.peers[pp.GetNodeId()]; ok {
			existing.update(pp)
			continue
		}
		p := newPeerState(n, pp)
		n.peers[p.id] = p
		n.peersByPub[p.pubB64] = p
		added = append(added, p)
	}
	n.mu.Unlock()
	// stop/start outside the lock: peer goroutines take n.mu.RLock
	for _, p := range removed {
		p.stop()
	}
	for _, p := range added {
		p.start()
	}
}

var _ = time.Second // reserved
