package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/grendalaget/varde/apps/controlplane/internal/store"
	"github.com/grendalaget/varde/go/gatewaytoken"
)

func TestExecutionGatewayLeaseAndRefresh(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{RelayKey: priv, Cfg: Config{Gateway: &GatewayConf{URL: "wss://gateway.example.org/v1/tunnel", Domain: "example.site"}}}
	label := "srv-abcdefghijklmnopqrst"
	const window = int64(8 * 60 * 1000)
	now := window*123 + 1000
	srv := store.Server{ID: "srv_a", GroupID: "grp_a", GameID: "autotournament", GatewayLabel: &label, Epoch: 3, DesiredState: "running"}
	e := store.Execution{ID: "exec_a", ServerID: srv.ID, NodeID: "node_a", Epoch: 3, Role: "serving", Action: "run", LeaseExpiresAt: now + 20000}
	got, err := s.executionGateway(&srv, &e, now)
	if err != nil || got == nil {
		t.Fatalf("executionGateway(current lease) present = %t, error = %v, want true, nil", got != nil, err)
	}
	claims, err := gatewaytoken.Verify(pub, got.Token, "example.site", now)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Epoch != 3 || claims.Label != label || claims.ServerID != srv.ID || claims.GroupID != srv.GroupID || got.ServicePort != 3069 || claims.ExpUnixMs-now > 10*60*1000 {
		t.Error("executionGateway claims do not match current lease and catalog")
	}
	stable, err := s.executionGateway(&srv, &e, now+1000)
	if err != nil {
		t.Fatal(err)
	}
	if stable.Token != got.Token {
		t.Error("executionGateway token changed within its refresh window")
	}
	e.LeaseExpiresAt = now + window + 20000
	refresh, err := s.executionGateway(&srv, &e, now+window)
	if err != nil {
		t.Fatal(err)
	}
	if refresh.Token == got.Token || got.ExpiresAtUnixMs <= now+window || refresh.ExpiresAtUnixMs <= got.ExpiresAtUnixMs {
		t.Error("executionGateway did not refresh before expiry")
	}
	for _, tc := range []struct {
		name string
		edit func(*store.Server, *store.Execution)
	}{
		{"incoming", func(_ *store.Server, e *store.Execution) { e.Role = "incoming" }},
		{"draining", func(_ *store.Server, e *store.Execution) { e.Role = "draining" }},
		{"old epoch", func(_ *store.Server, e *store.Execution) { e.Epoch-- }},
		{"expired", func(_ *store.Server, e *store.Execution) { e.LeaseExpiresAt = now }},
		{"stopping", func(_ *store.Server, e *store.Execution) { e.Action = "stop" }},
		{"stopped server", func(s *store.Server, _ *store.Execution) { s.DesiredState = "stopped" }},
		{"other game", func(s *store.Server, _ *store.Execution) { s.GameID = "minecraft" }},
		{"no label", func(s *store.Server, _ *store.Execution) { s.GatewayLabel = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, exec := srv, e
			tc.edit(&server, &exec)
			got, err := s.executionGateway(&server, &exec, now)
			if err != nil || got != nil {
				t.Errorf("executionGateway(%s) present = %t, error = %v, want false, nil", tc.name, got != nil, err)
			}
		})
	}
	s.Cfg.Gateway = nil
	if got, err := s.executionGateway(&srv, &e, now); err != nil || got != nil {
		t.Errorf("executionGateway(no gateway) present = %t, error = %v, want false, nil", got != nil, err)
	}
}
