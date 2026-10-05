package store

import (
	"context"
	"os"
	"strings"
	"testing"
)

func postgresTestURL() string { return os.Getenv("VARDE_TEST_POSTGRES_URL") }

func testStores(t *testing.T) map[string]*Store {
	t.Helper()
	out := map[string]*Store{}
	s, err := Open("sqlite://"+t.TempDir()+"/t.db", nil)
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	out["sqlite"] = s
	if url := postgresTestURL(); url != "" {
		p, err := Open(url, nil)
		if err != nil {
			t.Fatalf("postgres open: %v", err)
		}
		// dedicated test DB: reset the schema so reruns don't collide
		if _, err := p.DB.Exec(`DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
			t.Fatalf("postgres reset: %v", err)
		}
		_ = p.Close()
		p, err = Open(url, nil)
		if err != nil {
			t.Fatalf("postgres reopen: %v", err)
		}
		out["postgres"] = p
	}
	return out
}

func mkUser(t *testing.T, s *Store, id string) {
	t.Helper()
	u := &User{ID: id, Email: id + "@x", DisplayName: id, PasswordHash: "h", CreatedAt: 1}
	if err := s.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
}

func TestOpenMigrate(t *testing.T) {
	for name, s := range testStores(t) {
		defer func() { _ = s.Close() }()
		ctx := context.Background()
		mkUser(t, s, "usr_t"+name)
		g := &Group{ID: "grp_t" + name, Name: "test", Plan: "free", SettingsJSON: "{}", CreatedAt: 1}
		if err := s.CreateGroup(ctx, g, "usr_t"+name); err != nil {
			t.Fatalf("%s create group: %v", name, err)
		}
		got, err := s.GetGroup(ctx, g.ID)
		if err != nil || got.Name != "test" {
			t.Fatalf("%s get group: %v", name, err)
		}
	}
}

func TestUniquePartialIndex(t *testing.T) {
	for name, s := range testStores(t) {
		defer func() { _ = s.Close() }()
		ctx := context.Background()
		mkUser(t, s, "usr_u"+name)
		g := &Group{ID: "grp_" + name, Name: "g", SettingsJSON: "{}", CreatedAt: 1}
		if err := s.CreateGroup(ctx, g, "usr_u"+name); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		srv := &Server{ID: "srv_" + name, GroupID: g.ID, Name: "s", GameID: "testgame",
			DesiredState: "running", ObservedState: "stopped", ConfigJSON: "{}", CreatedAt: 1, UpdatedAt: 1}
		svc := &Service{ID: "svc_" + name, ServerID: srv.ID, GroupID: g.ID, LoopbackIP: "127.77.1.1", PortsJSON: "[]"}
		if err := s.CreateServer(ctx, srv, svc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		n := &Node{ID: "node_" + name, GroupID: g.ID, Name: "n", PublicKey: "k",
			OS: "linux", Arch: "amd64", HostingEnabled: 1, AdminState: "active", CreatedAt: 1}
		if err := s.CreateNode(ctx, n); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// epoch bump + execution insert is atomic
		if err := s.BumpEpochAndCreateExecution(ctx, &Execution{
			ID: "exec_1" + name, ServerID: srv.ID, NodeID: n.ID, Epoch: 1,
			Action: "run", State: "preparing", LeaseExpiresAt: 100, CreatedAt: 1,
		}); err != nil {
			t.Fatalf("%s activate: %v", name, err)
		}
		// second active execution for same server violates one_active
		err := s.BumpEpochAndCreateExecution(ctx, &Execution{
			ID: "exec_2" + name, ServerID: srv.ID, NodeID: n.ID, Epoch: 2,
			Action: "run", State: "preparing", LeaseExpiresAt: 100, CreatedAt: 1,
		})
		if err == nil || !IsUniqueViolation(err) {
			t.Fatalf("%s: expected unique violation, got %v", name, err)
		}
		// stale epoch (expecting 1 while server is at 1) is rejected
		err = s.BumpEpochAndCreateExecution(ctx, &Execution{
			ID: "exec_3" + name, ServerID: srv.ID, NodeID: n.ID, Epoch: 1,
			Action: "run", State: "preparing", LeaseExpiresAt: 100, CreatedAt: 1,
		})
		if err == nil || !strings.Contains(err.Error(), "epoch bump failed") {
			t.Fatalf("%s: expected stale-epoch failure, got %v", name, err)
		}
	}
}

func TestKVRoundTrip(t *testing.T) {
	for name, s := range testStores(t) {
		defer func() { _ = s.Close() }()
		ctx := context.Background()
		if err := s.KVSet(ctx, "k1", "v1"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		v, err := s.KVGet(ctx, "k1")
		if err != nil || v != "v1" {
			t.Fatalf("%s: %v %q", name, err, v)
		}
	}
}
