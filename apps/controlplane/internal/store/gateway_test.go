package store

import (
	"context"
	"strings"
	"testing"

	"github.com/grendalaget/varde/go/gatewaytoken"
)

func TestGatewayLabelPersistenceAndMigration(t *testing.T) {
	for name, s := range testStores(t) {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { _ = s.Close() })
			ctx := context.Background()
			mkUser(t, s, "usr_label")
			g := &Group{ID: "grp_label", Name: "g", SettingsJSON: "{}", CreatedAt: 1}
			if err := s.CreateGroup(ctx, g, "usr_label"); err != nil {
				t.Fatal(err)
			}
			create := func(id, ip string) *Server {
				t.Helper()
				server := &Server{ID: id, GroupID: g.ID, Name: "Editable name", GameID: "autotournament", DesiredState: "stopped", ObservedState: "stopped", ConfigJSON: "{}", CreatedAt: 1, UpdatedAt: 1}
				if err := s.CreateServer(ctx, server, &Service{ID: "svc_" + id, ServerID: id, GroupID: g.ID, LoopbackIP: ip, PortsJSON: "[]"}); err != nil {
					t.Fatal(err)
				}
				return server
			}
			a, b := create("srv_abcdefghijklmnopqrst", "127.77.1.1"), create("srv_bcdefghijklmnopqrstu", "127.77.1.2")
			if a.GatewayLabel == nil || b.GatewayLabel == nil {
				t.Fatal("CreateServer gateway labels = nil, want persisted labels")
			}
			label := *a.GatewayLabel
			if !strings.HasPrefix(label, "srv-") {
				t.Errorf("CreateServer label = %q, want reserved srv- namespace", label)
			}
			for _, reserved := range []string{"gateway", "www", "cp", "api", "auth", "admin", "docs"} {
				a.Name = reserved
				if err := s.UpdateServerMeta(ctx, a); err != nil {
					t.Fatal(err)
				}
				got, err := s.GetServer(ctx, a.ID)
				if err != nil || got.GatewayLabel == nil || *got.GatewayLabel != label {
					t.Fatalf("UpdateServerMeta(name=%q) = %+v, %v, want label %q", reserved, got, err, label)
				}
			}
			if !gatewaytoken.ValidLabel(label) || label == *b.GatewayLabel {
				t.Errorf("CreateServer labels = %q, %q, want distinct DNS labels", label, *b.GatewayLabel)
			}
			a.Name = "Renamed tournament"
			if err := s.UpdateServerMeta(ctx, a); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB.Exec(s.Rebind(`UPDATE servers SET gateway_label=NULL WHERE id=?`), b.ID); err != nil {
				t.Fatal(err)
			}
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			got, err := s.GetServer(ctx, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.GatewayLabel == nil || *got.GatewayLabel != label {
				t.Errorf("GetServer(renamed).GatewayLabel = %v, want %q", got.GatewayLabel, label)
			}
			backfilled, err := s.GetServer(ctx, b.ID)
			if err != nil {
				t.Fatal(err)
			}
			if backfilled.GatewayLabel == nil || *backfilled.GatewayLabel != *b.GatewayLabel {
				t.Errorf("Migrate().GatewayLabel = %v, want %q", backfilled.GatewayLabel, *b.GatewayLabel)
			}
			if _, err := s.DB.Exec(s.Rebind(`UPDATE servers SET gateway_label=? WHERE id=?`), label, b.ID); err == nil {
				t.Error("UPDATE duplicate gateway_label error = nil, want uniqueness violation")
			}
		})
	}
}
