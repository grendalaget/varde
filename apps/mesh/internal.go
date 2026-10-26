package main

import (
	"bytes"
	"context"
	"io"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	meshv1 "github.com/grendalaget/varde/go/gen/mesh/v1"
)

// SetInternalServices replaces the local internal-service table.
func (n *Node) SetInternalServices(req *meshv1.SetInternalServicesRequest) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.internal = map[string]uint32{}
	for _, s := range req.GetServices() {
		n.internal[s.GetName()] = s.GetTargetPort()
	}
}

func (s *meshServer) ExchangeInternal(req *meshv1.ExchangeInternalRequest, out meshv1.MeshService_ExchangeInternalServer) error {
	peer := s.n.peer(req.GetPeerNodeId())
	if peer == nil {
		return status.Error(codes.NotFound, "unknown peer")
	}
	ctx, cancel := context.WithTimeout(out.Context(), 10*time.Second)
	defer cancel()
	st, err := peer.openStream(ctx, &meshv1.StreamOpen{Kind: &meshv1.StreamOpen_Internal{
		Internal: &meshv1.InternalStream{Name: req.GetName()},
	}})
	if err != nil {
		return status.Errorf(codes.Unavailable, "open internal stream: %v", err)
	}
	defer st.CancelRead(0)
	defer st.CancelWrite(0)
	stop := context.AfterFunc(out.Context(), func() {
		st.CancelRead(0)
		st.CancelWrite(0)
	})
	defer stop()
	n, err := io.Copy(st, bytes.NewReader(req.GetRequest()))
	peer.bytesTx.Add(uint64(n))
	if err != nil {
		return status.Errorf(codes.Unavailable, "write internal request: %v", err)
	}
	if err := st.Close(); err != nil {
		return status.Errorf(codes.Unavailable, "finish internal request: %v", err)
	}
	buf := make([]byte, 64*1024)
	for {
		n, err := st.Read(buf)
		if n > 0 {
			peer.bytesRx.Add(uint64(n))
			if err := out.Send(&meshv1.ExchangeInternalResponse{Data: bytes.Clone(buf[:n])}); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return status.Errorf(codes.Unavailable, "read internal response: %v", err)
		}
	}
}
