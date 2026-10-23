package api

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/grendalaget/varde/apps/controlplane/internal/secrets"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

type autoTournamentTestClock struct{}

func (autoTournamentTestClock) Now() time.Time { return time.UnixMilli(1000) }

func TestAutoTournamentDirectiveScopeAndConfigRedaction(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open("sqlite://"+t.TempDir()+"/cp.db", autoTournamentTestClock{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	user := &store.User{ID: "user", Email: "user@example.test", DisplayName: "User", PasswordHash: "test", CreatedAt: 1}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	settings, err := json.Marshal(store.DefaultGroupSettings())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateGroup(ctx, &store.Group{
		ID: "group", Name: "Group", Plan: "free", SettingsJSON: string(settings), CreatedAt: 1,
	}, user.ID); err != nil {
		t.Fatal(err)
	}
	manager, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := manager.Seal("fleet-secret", "group/autotournament/fleet_key")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertGroupIntegration(ctx, &store.GroupIntegration{
		GroupID: "group", Kind: autoTournamentKind, BaseURL: "https://at.example.test",
		APITokenSealed: "v1:unused", FleetKeySealed: sealed, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: st, Secrets: manager}
	serverModel := &store.Server{
		ID: "server", GroupID: "group", Name: "CS2", GameID: "cs2",
		DesiredState: "stopped", ObservedState: "stopped", ReplicationFactor: 1,
		MinCommitReplicas: 1, SnapshotIntervalS: 120, ConfigJSON: "{}",
		CreatedAt: 1, UpdatedAt: 1,
	}
	service := &store.Service{
		ID: "service", ServerID: serverModel.ID, GroupID: serverModel.GroupID,
		LoopbackIP: "127.0.0.1",
		PortsJSON:  `[{"port":27015,"protocol":"udp"},{"port":27016,"protocol":"tcp"}]`,
	}
	if err := st.CreateServer(ctx, serverModel, service); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{}
	server.addAutoTournamentDirective(ctx, serverModel, cfg)
	directive, ok := cfg["_autotournament"].(map[string]any)
	if !ok {
		t.Fatalf("CS2 linked directive missing from config: %#v", cfg)
	}
	if directive["fleet_url"] != "https://at.example.test" ||
		directive["fleet_enroll_key"] != "fleet-secret" ||
		directive["public_addr"] != "127.0.0.1:27015" {
		t.Errorf("CS2 linked directive = %#v", directive)
	}
	stored, err := st.GetServer(ctx, serverModel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ConfigJSON != "{}" {
		t.Errorf("directive was persisted in server ConfigJSON: %s", stored.ConfigJSON)
	}
	view, err := server.serverView(ctx, &store.Server{
		ID: serverModel.ID, GroupID: serverModel.GroupID, Name: serverModel.Name,
		GameID: serverModel.GameID, DesiredState: "stopped", ObservedState: "stopped",
		ReplicationFactor: 1, MinCommitReplicas: 1, SnapshotIntervalS: 120,
		ConfigJSON: `{"_autotournament":{"fleet_enroll_key":"secret"}}`,
		CreatedAt:  1, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := view.Config["_autotournament"]; leaked {
		t.Errorf("server config view included reserved directive: %#v", view.Config)
	}

	otherGame := map[string]any{}
	server.addAutoTournamentDirective(ctx, &store.Server{GameID: "csgo", GroupID: "group"}, otherGame)
	if _, present := otherGame["_autotournament"]; present {
		t.Errorf("non-CS2 config received directive: %#v", otherGame)
	}
	unlinked := map[string]any{}
	server.addAutoTournamentDirective(ctx, &store.Server{GameID: "cs2", GroupID: "missing"}, unlinked)
	if _, present := unlinked["_autotournament"]; present {
		t.Errorf("unlinked group config received directive: %#v", unlinked)
	}
}

func TestChunkTournamentTeams(t *testing.T) {
	players := []tournamentPlayer{
		{steamID: "76561198000000001", name: "One"},
		{steamID: "76561198000000002", name: "Two"},
		{steamID: "76561198000000003", name: "Three"},
		{steamID: "76561198000000004", name: "Four"},
	}
	teams, ids := chunkTournamentTeams(players, 2, "abcd")
	if len(teams) != 2 || len(ids) != 2 {
		t.Fatalf("chunkTournamentTeams() returned %d teams and %d IDs, want 2 and 2", len(teams), len(ids))
	}
	for i, team := range teams {
		n := i + 1
		wantID := fmt.Sprintf("varde-abcd-%d", n)
		if team.ID != wantID || ids[i] != wantID ||
			team.Name != fmt.Sprintf("Team %d", n) ||
			team.Tag != fmt.Sprintf("T%d", n) || len(team.Players) != 2 {
			t.Errorf("team %d = %+v, ids=%v", i+1, team, ids)
		}
	}
	if teams[0].Players[0].Name != "One" || teams[1].Players[1].Name != "Four" {
		t.Errorf("team rosters = %+v", teams)
	}
}
