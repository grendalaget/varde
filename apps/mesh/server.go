package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	meshv1 "github.com/arnemolland/p2pgames/go/gen/mesh/v1"
)

type meshServer struct {
	meshv1.UnimplementedMeshServiceServer

	log       *slog.Logger
	startedAt time.Time

	mu         sync.Mutex
	configured bool
	nodeID     string
	listenAddr string
}

func newMeshServer(log *slog.Logger) *meshServer {
	return &meshServer{log: log, startedAt: time.Now()}
}

func (s *meshServer) GetStatus(_ context.Context, _ *meshv1.GetStatusRequest) (*meshv1.MeshStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &meshv1.MeshStatus{
		NodeId:          s.nodeID,
		Version:         "dev",
		Configured:      s.configured,
		StartedAtUnixMs: s.startedAt.UnixMilli(),
	}
	if s.listenAddr != "" {
		st.ListenAddrs = []string{s.listenAddr}
	}
	return st, nil
}

func (s *meshServer) Configure(_ context.Context, req *meshv1.ConfigureRequest) (*meshv1.ConfigureResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodeID = req.GetNodeId()
	s.configured = true
	// Transport isn't bound yet in M0; report the configured port as a
	// placeholder listen address once networking lands.
	s.log.Info("configured",
		"node_id", req.GetNodeId(),
		"listen_port", req.GetListenPort(),
		"relays", len(req.GetRelays()),
		"force_relay", req.GetForceRelay(),
		"identity_key_path", req.GetIdentityKeyPath(),
	)
	return &meshv1.ConfigureResponse{}, nil
}

func (s *meshServer) SetPeers(context.Context, *meshv1.SetPeersRequest) (*meshv1.SetPeersResponse, error) {
	return nil, status.Error(codes.Unimplemented, "SetPeers not implemented yet")
}

func (s *meshServer) ListPeers(context.Context, *meshv1.ListPeersRequest) (*meshv1.ListPeersResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ListPeers not implemented yet")
}

func (s *meshServer) SetRoutes(context.Context, *meshv1.SetRoutesRequest) (*meshv1.SetRoutesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "SetRoutes not implemented yet")
}

func (s *meshServer) SetHostedServices(context.Context, *meshv1.SetHostedServicesRequest) (*meshv1.SetHostedServicesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "SetHostedServices not implemented yet")
}

func (s *meshServer) SetInternalServices(context.Context, *meshv1.SetInternalServicesRequest) (*meshv1.SetInternalServicesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "SetInternalServices not implemented yet")
}

func (s *meshServer) BindInternalForward(context.Context, *meshv1.BindInternalForwardRequest) (*meshv1.BindInternalForwardResponse, error) {
	return nil, status.Error(codes.Unimplemented, "BindInternalForward not implemented yet")
}

func (s *meshServer) UnbindInternalForward(context.Context, *meshv1.UnbindInternalForwardRequest) (*meshv1.UnbindInternalForwardResponse, error) {
	return nil, status.Error(codes.Unimplemented, "UnbindInternalForward not implemented yet")
}

func (s *meshServer) WatchEvents(*meshv1.WatchEventsRequest, meshv1.MeshService_WatchEventsServer) error {
	return status.Error(codes.Unimplemented, "WatchEvents not implemented yet")
}
