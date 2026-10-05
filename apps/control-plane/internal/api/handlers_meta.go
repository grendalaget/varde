package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"

	"github.com/arnemolland/p2pgames/apps/control-plane/internal/api/gen"
)

func (s *Server) GetHealth(_ context.Context, _ gen.GetHealthRequestObject) (gen.GetHealthResponseObject, error) {
	return gen.GetHealth200JSONResponse{Status: gen.Ok}, nil
}

func (s *Server) GetReady(ctx context.Context, _ gen.GetReadyRequestObject) (gen.GetReadyResponseObject, error) {
	if err := s.Store.Ping(ctx); err != nil {
		return gen.GetReady503JSONResponse{
			Code: gen.Internal, Message: "db unreachable",
		}, nil
	}
	return gen.GetReady200JSONResponse{Status: gen.Ok}, nil
}

func (s *Server) GetVersion(_ context.Context, _ gen.GetVersionRequestObject) (gen.GetVersionResponseObject, error) {
	return gen.GetVersion200JSONResponse{Version: s.Cfg.Version}, nil
}

func (s *Server) GetRelayPublicKey(_ context.Context, _ gen.GetRelayPublicKeyRequestObject) (gen.GetRelayPublicKeyResponseObject, error) {
	pub := s.RelayKey.Public().(ed25519.PublicKey)
	return gen.GetRelayPublicKey200JSONResponse{
		PublicKey: base64.StdEncoding.EncodeToString(pub),
	}, nil
}
