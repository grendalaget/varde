package api

import (
	"context"
	"encoding/json"

	"github.com/grendalaget/varde/apps/controlplane/internal/api/gen"
	"github.com/grendalaget/varde/apps/controlplane/internal/catalog"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
	"github.com/grendalaget/varde/go/gatewaytoken"
)

func (s *Server) serverDirectiveConfig(ctx context.Context, srv *store.Server) (map[string]any, error) {
	var cfg map[string]any
	_ = json.Unmarshal([]byte(srv.ConfigJSON), &cfg)
	if err := s.injectManagedSecrets(ctx, srv.ID, &cfg); err != nil {
		return nil, err
	}
	if publicURL := s.gatewayPublicURL(srv); publicURL != "" {
		if cfg == nil {
			cfg = map[string]any{}
		}
		cfg["public_url"] = publicURL
	}
	return cfg, nil
}

func (s *Server) requireGateway(game *catalog.Game) error {
	if game != nil && game.GatewayPort != 0 && s.Cfg.Gateway == nil {
		return errResp(gen.GatewayUnconfigured, "No gateway is configured; this server cannot have a public URL", nil)
	}
	return nil
}

func (s *Server) gatewayPublicURL(srv *store.Server) string {
	game := catalog.Get(srv.GameID)
	if game == nil || game.GatewayPort == 0 || s.Cfg.Gateway == nil || srv.GatewayLabel == nil {
		return ""
	}
	return "https://" + *srv.GatewayLabel + "." + s.Cfg.Gateway.Domain
}

func (s *Server) executionGateway(srv *store.Server, e *store.Execution, now int64) (*gen.ExecutionGateway, error) {
	game := catalog.Get(srv.GameID)
	if game == nil || game.GatewayPort == 0 || s.Cfg.Gateway == nil || srv.GatewayLabel == nil || e.Role != "serving" || e.Epoch != srv.Epoch || e.Action != "run" || e.LeaseExpiresAt <= now || srv.DesiredState != "running" {
		return nil, nil
	}
	// Stable eight-minute refresh windows avoid reconnecting on every heartbeat.
	const window = int64(8 * 60 * 1000)
	expires := (now/window+1)*window + 2*60*1000
	token, err := gatewaytoken.Sign(s.RelayKey, gatewaytoken.Claims{
		Audience: gatewaytoken.Audience, Domain: s.Cfg.Gateway.Domain,
		Label: *srv.GatewayLabel, GroupID: srv.GroupID, ServerID: srv.ID,
		Epoch: uint64(e.Epoch), ExpUnixMs: expires,
	})
	if err != nil {
		return nil, err
	}
	return &gen.ExecutionGateway{GatewayUrl: s.Cfg.Gateway.URL, Token: token, ServicePort: game.GatewayPort, ExpiresAtUnixMs: expires}, nil
}
