package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
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
			_ = s.Close()
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
	if err := s.CreateUserFirstOperator(context.Background(), u); err != nil {
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

func TestPruneSessions(t *testing.T) {
	for name, s := range testStores(t) {
		defer func() { _ = s.Close() }()
		ctx := context.Background()
		userID := "usr_sessions_" + name
		mkUser(t, s, userID)
		if err := s.CreateSession(ctx, &Session{
			TokenHash: "expired_" + name,
			UserID:    userID,
			CreatedAt: 1,
			ExpiresAt: 100,
		}); err != nil {
			t.Fatalf("%s create expired session: %v", name, err)
		}
		if err := s.CreateSession(ctx, &Session{
			TokenHash: "live_" + name,
			UserID:    userID,
			CreatedAt: 2,
			ExpiresAt: 101,
		}); err != nil {
			t.Fatalf("%s create live session: %v", name, err)
		}

		n, err := s.PruneSessions(ctx, 100)
		if err != nil || n != 1 {
			t.Fatalf("%s prune sessions: %d %v, want 1", name, n, err)
		}
		if _, err := s.SessionByTokenHash(ctx, "expired_"+name); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s expired session lookup: %v, want ErrNotFound", name, err)
		}
		live, err := s.SessionByTokenHash(ctx, "live_"+name)
		if err != nil || live.ExpiresAt != 101 {
			t.Fatalf("%s live session lookup: %+v %v, want session expiring at 101", name, live, err)
		}
	}
}

func TestPruneExpired(t *testing.T) {
	for name, s := range testStores(t) {
		defer func() { _ = s.Close() }()
		ctx := context.Background()
		userID := "usr_prune_expired_" + name
		groupID := "grp_prune_expired_" + name
		mkUser(t, s, userID)
		if err := s.CreateGroup(ctx, &Group{
			ID: groupID, Name: "test", Plan: "free", SettingsJSON: "{}", CreatedAt: 1,
		}, userID); err != nil {
			t.Fatalf("%s create group: %v", name, err)
		}
		exec := func(query string, args ...any) {
			t.Helper()
			if _, err := s.DB.Exec(s.Rebind(query), args...); err != nil {
				t.Fatalf("%s insert prune fixture: %v", name, err)
			}
		}
		rows := []struct {
			suffix  string
			expires any
		}{
			{suffix: "expired", expires: int64(99)},
			{suffix: "later", expires: int64(101)},
			{suffix: "never", expires: nil},
		}
		for _, row := range rows {
			exec(
				`INSERT INTO invites (code,group_id,created_by,expires_at) VALUES (?,?,?,?)`,
				"invite_"+row.suffix+"_"+name, groupID, userID, row.expires,
			)
			exec(
				`INSERT INTO enrollment_tokens (id,group_id,token_hash,created_by,expires_at) VALUES (?,?,?,?,?)`,
				"enrollment_"+row.suffix+"_"+name, groupID, "hash_"+row.suffix+"_"+name, userID, row.expires,
			)
		}
		exec(
			`INSERT INTO device_links (user_code,device_code_hash,public_key,expires_at,created_at) VALUES (?,?,?,?,?)`,
			"device_expired_"+name, "device_hash_expired_"+name, "public_expired_"+name, int64(99), int64(1),
		)
		exec(
			`INSERT INTO device_links (user_code,device_code_hash,public_key,expires_at,created_at) VALUES (?,?,?,?,?)`,
			"device_later_"+name, "device_hash_later_"+name, "public_later_"+name, int64(101), int64(1),
		)

		n, err := s.PruneExpired(ctx, 100)
		if err != nil || n != 3 {
			t.Fatalf("%s prune expired rows: %d %v, want 3", name, n, err)
		}
		assertRow := func(table, key, id string, want bool) {
			t.Helper()
			var count int
			if err := s.DB.Get(&count, s.Rebind(
				"SELECT COUNT(*) FROM "+table+" WHERE "+key+"=?"), id); err != nil {
				t.Fatalf("%s count %s %s: %v", name, table, id, err)
			}
			if (count == 1) != want {
				t.Fatalf("%s %s %s exists=%t, want %t", name, table, id, count == 1, want)
			}
		}
		for _, row := range rows {
			assertRow("invites", "code", "invite_"+row.suffix+"_"+name, row.suffix != "expired")
			assertRow("enrollment_tokens", "id", "enrollment_"+row.suffix+"_"+name, row.suffix != "expired")
		}
		assertRow("device_links", "user_code", "device_expired_"+name, false)
		assertRow("device_links", "user_code", "device_later_"+name, true)
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
		// an 'incoming' warm-migration execution coexists with the serving one
		if err := s.BumpEpochAndCreateExecution(ctx, &Execution{
			ID: "exec_inc" + name, ServerID: srv.ID, NodeID: n.ID, Epoch: 2,
			Action: "run", State: "preparing", LeaseExpiresAt: 100, CreatedAt: 1,
			Role: "incoming",
		}); err != nil {
			t.Fatalf("%s: incoming exec rejected: %v", name, err)
		}
		// active execution is still the serving one
		if a, err := s.ActiveExecution(ctx, srv.ID); err != nil || a.ID != "exec_1"+name {
			t.Fatalf("%s: active=%+v err=%v", name, a, err)
		}
		// promoting the incoming exec to serving violates one_active
		inc, err := s.GetExecution(ctx, "exec_inc"+name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		inc.Role = "serving"
		err = s.UpdateExecutionState(ctx, s.DB, inc)
		if err == nil || !IsUniqueViolation(err) {
			t.Fatalf("%s: expected unique violation promoting incoming, got %v", name, err)
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
		ok, err := s.KVSetNX(ctx, "k2", "first")
		if err != nil || !ok {
			t.Fatalf("%s: first KVSetNX: %v %v", name, err, ok)
		}
		ok, err = s.KVSetNX(ctx, "k2", "second")
		if err != nil || ok {
			t.Fatalf("%s: second KVSetNX: %v %v", name, err, ok)
		}
		v, err = s.KVGet(ctx, "k2")
		if err != nil || v != "first" {
			t.Fatalf("%s: KVSetNX kept %q", name, v)
		}
	}
}

func TestExtendLeaseNeverReopensOrShortens(t *testing.T) {
	for name, s := range testStores(t) {
		defer func() { _ = s.Close() }()
		ctx := context.Background()
		mkUser(t, s, "usr_l"+name)
		g := &Group{ID: "grp_l" + name, Name: "g", SettingsJSON: "{}", CreatedAt: 1}
		if err := s.CreateGroup(ctx, g, "usr_l"+name); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		srv := &Server{ID: "srv_l" + name, GroupID: g.ID, Name: "s", GameID: "testgame",
			DesiredState: "running", ObservedState: "stopped", ConfigJSON: "{}", CreatedAt: 1, UpdatedAt: 1}
		svc := &Service{ID: "svc_l" + name, ServerID: srv.ID, GroupID: g.ID, LoopbackIP: "127.77.1.2", PortsJSON: "[]"}
		if err := s.CreateServer(ctx, srv, svc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		n := &Node{ID: "node_l" + name, GroupID: g.ID, Name: "n", PublicKey: "k",
			OS: "linux", Arch: "amd64", HostingEnabled: 1, AdminState: "active", CreatedAt: 1}
		if err := s.CreateNode(ctx, n); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		id := "exec_l" + name
		if err := s.BumpEpochAndCreateExecution(ctx, &Execution{
			ID: id, ServerID: srv.ID, NodeID: n.ID, Epoch: 1,
			Action: "run", State: "running", LeaseExpiresAt: 100, CreatedAt: 1,
		}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		check := func(what string, epoch, now, exp int64, want bool, wantLease int64) {
			t.Helper()
			ok, err := s.ExtendLease(ctx, id, n.ID, epoch, now, exp)
			if err != nil || ok != want {
				t.Fatalf("%s %s: ok=%v err=%v, want %v", name, what, ok, err, want)
			}
			e, err := s.GetExecution(ctx, id)
			if err != nil || e.LeaseExpiresAt != wantLease {
				t.Fatalf("%s %s: lease %d (%v), want %d", name, what, e.LeaseExpiresAt, err, wantLease)
			}
		}
		check("extend", 1, 50, 150, true, 150)
		check("shorter", 1, 50, 120, false, 150)
		check("stale epoch", 2, 50, 200, false, 150)
		check("expired", 1, 150, 300, false, 150)
		if err := s.EndExecution(ctx, s.DB, id, "stopped", 60); err != nil {
			t.Fatal(err)
		}
		check("ended", 1, 60, 200, false, 150)
		if e, _ := s.GetExecution(ctx, id); e.EndedAt == nil || e.State != "running" {
			t.Fatalf("%s: renewal touched an ended execution: %+v", name, e)
		}
	}
}

func TestEventQueriesAndPrune(t *testing.T) {
	for name, s := range testStores(t) {
		defer func() { _ = s.Close() }()
		ctx := context.Background()
		insert := func(groupID string, createdAt int64) {
			t.Helper()
			if _, err := s.DB.ExecContext(ctx, s.Rebind(
				`INSERT INTO events (group_id,type,data_json,created_at) VALUES (?,?,?,?)`),
				groupID, "test", "{}", createdAt); err != nil {
				t.Fatalf("%s insert: %v", name, err)
			}
		}
		for i := int64(1); i <= 5; i++ {
			insert("grp_a", i*1000)
		}
		insert("grp_b", 500)

		latest, err := s.LatestEvents(ctx, "grp_a", 2)
		if err != nil || len(latest) != 2 || latest[0].CreatedAt != 4000 || latest[1].CreatedAt != 5000 {
			t.Fatalf("%s latest: %+v %v", name, latest, err)
		}
		maxA, err := s.MaxEventID(ctx, "grp_a")
		if err != nil || maxA != latest[1].ID {
			t.Fatalf("%s max: %d %v, want %d", name, maxA, err, latest[1].ID)
		}
		if id, err := s.MaxEventID(ctx, "grp_none"); err != nil || id != 0 {
			t.Fatalf("%s empty max: %d %v", name, id, err)
		}

		n, err := s.PruneEvents(ctx, 3000)
		if err != nil || n != 3 {
			t.Fatalf("%s prune: %d %v, want 3", name, n, err)
		}
		left, err := s.EventsAfter(ctx, "grp_a", 0, 100)
		if err != nil || len(left) != 3 || left[0].CreatedAt != 3000 {
			t.Fatalf("%s after prune: %+v %v", name, left, err)
		}
		if b, _ := s.EventsAfter(ctx, "grp_b", 0, 100); len(b) != 0 {
			t.Fatalf("%s: old grp_b event survived prune", name)
		}
	}
}

// Deletes run child-first so Postgres foreign keys (and sqlite's, enabled in
// Open) hold, and leave nothing that references the deleted rows.
func TestDeletesAreForeignKeySafe(t *testing.T) {
	for name, s := range testStores(t) {
		defer func() { _ = s.Close() }()
		ctx := context.Background()
		fail := func(what string, err error) {
			t.Helper()
			if err != nil {
				t.Fatalf("%s %s: %v", name, what, err)
			}
		}
		if name == "sqlite" {
			var fk int
			fail("pragma", s.DB.Get(&fk, `PRAGMA foreign_keys`))
			if fk != 1 {
				t.Fatalf("sqlite foreign_keys=%d", fk)
			}
		}
		mkUser(t, s, "usr_d"+name)
		g := &Group{ID: "grp_d" + name, Name: "g", SettingsJSON: "{}", CreatedAt: 1}
		fail("group", s.CreateGroup(ctx, g, "usr_d"+name))
		fail("limits", s.SetLimits(ctx, &GroupLimits{GroupID: g.ID}))
		fail("invite", s.CreateInvite(ctx, &Invite{Code: "inv_d" + name, GroupID: g.ID, Role: "member", CreatedBy: "x"}))
		mkNode := func(id string) {
			fail("node", s.CreateNode(ctx, &Node{ID: id + name, GroupID: g.ID, Name: id, PublicKey: "pk_" + id + name,
				HostingEnabled: 1, AdminState: "active", CreatedAt: 1}))
			fail("node status", s.PutNodeStatus(ctx, &NodeStatus{NodeID: id + name, StatusJSON: "{}", MeshJSON: "{}", UpdatedAt: 1}))
		}
		mkNode("n1")
		mkNode("n2")
		n1, n2 := "n1"+name, "n2"+name
		mkServer := func(id, ip string) {
			fail("server", s.CreateServer(ctx, &Server{ID: id + name, GroupID: g.ID, Name: id, GameID: "testgame",
				DesiredState: "stopped", ObservedState: "stopped", ConfigJSON: "{}", CreatedAt: 1, UpdatedAt: 1},
				&Service{ID: "svc_" + id + name, ServerID: id + name, GroupID: g.ID, LoopbackIP: ip, PortsJSON: "[]"}))
		}
		mkServer("s1", "127.77.2.1")
		mkServer("s2", "127.77.2.2")
		s1, s2 := "s1"+name, "s2"+name
		exec := func(id, srv, node string) {
			fail("exec", s.BumpEpochAndCreateExecution(ctx, &Execution{ID: id + name, ServerID: srv, NodeID: node,
				Epoch: 1, Action: "run", State: "running", LeaseExpiresAt: 100, CreatedAt: 1}))
		}
		exec("e1", s1, n1)
		exec("e2", s2, n1)
		e1 := "e1" + name
		fail("exec info", s.Tx(ctx, func(tx *sqlx.Tx) error {
			_, err := s.SetExecutionJoinCode(ctx, tx, e1, "JOIN", 1)
			return err
		}))
		fail("logs", s.AppendLogLines(ctx, e1, []LogLineRow{{ExecutionID: e1, At: 1, Stream: "stdout", Line: "x"}}))
		snap := func(id, srv string, holders ...string) {
			fail("snapshot", s.CreateSnapshotTx(ctx, s.DB, &Snapshot{ID: id + name, ServerID: srv, GroupID: g.ID,
				Epoch: 1, ExecutionID: "x", NodeID: n1, ManifestDigest: "d", Reason: "scheduled", State: "committed", CreatedAt: 1}))
			for _, h := range holders {
				fail("replica", s.UpsertReplica(ctx, s.DB, &Replica{SnapshotID: id + name, NodeID: h, State: "ready", AssignedAt: 1}))
			}
		}
		snap("p1", s1, n1, n2)
		snap("p2", s2, n1)
		fail("request", s.CreateSnapshotRequest(ctx, &SnapshotRequest{ID: "rq" + name, ServerID: s1, Reason: "manual", CreatedAt: 1}))
		m := &Migration{ID: "m" + name, ServerID: s1, FromExecutionID: e1, ToNodeID: n2, State: "stopping", Mode: "serial", CreatedAt: 1}
		fail("migration", s.CreateMigration(ctx, m))

		// s1: refused while its execution or migration is open
		if err := s.DeleteServer(ctx, s1, 2); !errors.Is(err, ErrConflict) {
			t.Fatalf("%s delete with open exec: %v", name, err)
		}
		fail("end", s.EndExecution(ctx, s.DB, e1, "stopped", 2))
		if err := s.DeleteServer(ctx, s1, 2); !errors.Is(err, ErrConflict) {
			t.Fatalf("%s delete with open migration: %v", name, err)
		}
		done := int64(2)
		m.State, m.FinishedAt = "failed", &done
		fail("finish migration", s.UpdateMigration(ctx, m))
		fail("delete s1", s.DeleteServer(ctx, s1, 2))
		if _, err := s.GetServer(ctx, s1); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s deleted server still found: %v", name, err)
		}
		if reps, _ := s.DeletingReplicasForNode(ctx, n2); len(reps) != 1 {
			t.Fatalf("%s n2 deleting replicas: %+v", name, reps)
		}
		fail("ack", s.DeleteReplica(ctx, s.DB, "p1"+name, n2))

		// n1 goes: s1's last copy with it (purge follows), p2 had no other copy
		fail("end e2", s.EndExecution(ctx, s.DB, "e2"+name, "stopped", 2))
		fail("delete n1", s.DeleteNode(ctx, n1, 3))
		if _, err := s.GetNode(ctx, n1); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s deleted node still found: %v", name, err)
		}
		if _, err := s.GetSnapshot(ctx, "p2"+name); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s save without copies survived: %v", name, err)
		}
		fail("purge", s.PurgeDeletedServers(ctx))
		var left int
		fail("count", s.DB.Get(&left, s.Rebind(`SELECT COUNT(*) FROM servers WHERE id=?`), s1))
		if left != 0 {
			t.Fatalf("%s s1 not purged", name)
		}
		// the machine can enroll again with the same key
		fail("re-enroll", s.CreateNode(ctx, &Node{ID: "n3" + name, GroupID: g.ID, Name: "n3",
			PublicKey: "pk_n1" + name, HostingEnabled: 1, AdminState: "active", CreatedAt: 1}))

		if err := s.DeleteGroup(ctx, g.ID); !errors.Is(err, ErrConflict) {
			t.Fatalf("%s delete non-empty group: %v", name, err)
		}
		fail("delete s2", s.DeleteServer(ctx, s2, 4))
		fail("delete n2", s.DeleteNode(ctx, n2, 4))
		fail("delete n3", s.DeleteNode(ctx, "n3"+name, 4))
		fail("delete group", s.DeleteGroup(ctx, g.ID))
		if _, err := s.GetGroup(ctx, g.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s group still found: %v", name, err)
		}
	}
}

func TestAssignReplicaSerializesAgainstDeleteServer(t *testing.T) {
	for name, s := range testStores(t) {
		defer func() { _ = s.Close() }()
		ctx := context.Background()
		userID := "usr_assign_" + name
		mkUser(t, s, userID)
		g := &Group{ID: "grp_assign_" + name, Name: "g", Plan: "free", SettingsJSON: "{}", CreatedAt: 1}
		if err := s.CreateGroup(ctx, g, userID); err != nil {
			t.Fatalf("%s create group: %v", name, err)
		}
		nodeA, nodeB := "node_assign_a_"+name, "node_assign_b_"+name
		for _, nodeID := range []string{nodeA, nodeB} {
			if err := s.CreateNode(ctx, &Node{
				ID: nodeID, GroupID: g.ID, Name: nodeID, PublicKey: "pk_" + nodeID,
				OS: "linux", Arch: "amd64", HostingEnabled: 1, AdminState: "active", CreatedAt: 1,
			}); err != nil {
				t.Fatalf("%s create node %s: %v", name, nodeID, err)
			}
		}
		serverID := "srv_assign_" + name
		srv := &Server{ID: serverID, GroupID: g.ID, Name: "s", GameID: "testgame",
			DesiredState: "stopped", ObservedState: "stopped", ConfigJSON: "{}", CreatedAt: 1, UpdatedAt: 1}
		svc := &Service{ID: "svc_assign_" + name, ServerID: serverID, GroupID: g.ID,
			LoopbackIP: "127.77.3.1", PortsJSON: "[]"}
		if err := s.CreateServer(ctx, srv, svc); err != nil {
			t.Fatalf("%s create server: %v", name, err)
		}
		makeSnapshot := func(snapshotID string) {
			t.Helper()
			if err := s.CreateSnapshotTx(ctx, s.DB, &Snapshot{
				ID: snapshotID, ServerID: serverID, GroupID: g.ID, Epoch: 1, ExecutionID: "exec_" + snapshotID,
				NodeID: nodeA, ManifestDigest: "digest", Reason: "scheduled", State: "committed", CreatedAt: 1,
			}); err != nil {
				t.Fatalf("%s create snapshot %s: %v", name, snapshotID, err)
			}
		}

		liveSnapshot := "snap_assign_" + name
		makeSnapshot(liveSnapshot)
		inserted, err := s.AssignReplica(ctx, serverID, liveSnapshot, nodeB, 100)
		if err != nil || !inserted {
			t.Fatalf("%s initial assignment: inserted=%t err=%v, want true", name, inserted, err)
		}
		var assignedState string
		if err := s.DB.GetContext(ctx, &assignedState, s.Rebind(
			`SELECT state FROM snapshot_replicas WHERE snapshot_id=? AND node_id=?`), liveSnapshot, nodeB); err != nil {
			t.Fatalf("%s read initial assignment: %v", name, err)
		}
		if assignedState != "assigned" {
			t.Fatalf("%s initial state=%q, want assigned", name, assignedState)
		}
		if _, err := s.DB.ExecContext(ctx, s.Rebind(
			`UPDATE snapshot_replicas SET state='ready', ready_at=150
			 WHERE snapshot_id=? AND node_id=?`), liveSnapshot, nodeB); err != nil {
			t.Fatalf("%s ready initial assignment: %v", name, err)
		}
		inserted, err = s.AssignReplica(ctx, serverID, liveSnapshot, nodeB, 200)
		if err != nil || inserted {
			t.Fatalf("%s duplicate assignment: inserted=%t err=%v, want false", name, inserted, err)
		}
		var existing struct {
			State      string        `db:"state"`
			AssignedAt int64         `db:"assigned_at"`
			ReadyAt    sql.NullInt64 `db:"ready_at"`
		}
		if err := s.DB.GetContext(ctx, &existing, s.Rebind(
			`SELECT state, assigned_at, ready_at FROM snapshot_replicas WHERE snapshot_id=? AND node_id=?`),
			liveSnapshot, nodeB); err != nil {
			t.Fatalf("%s read duplicate assignment: %v", name, err)
		}
		if existing.State != "ready" || existing.AssignedAt != 100 ||
			!existing.ReadyAt.Valid || existing.ReadyAt.Int64 != 150 {
			t.Fatalf("%s duplicate changed existing replica: %+v", name, existing)
		}

		deletingSnapshot := "snap_deleting_" + name
		makeSnapshot(deletingSnapshot)
		if err := s.UpsertReplica(ctx, s.DB, &Replica{
			SnapshotID: deletingSnapshot, NodeID: nodeB, State: "deleting", AssignedAt: 300,
		}); err != nil {
			t.Fatalf("%s create deleting replica: %v", name, err)
		}
		inserted, err = s.AssignReplica(ctx, serverID, deletingSnapshot, nodeB, 400)
		if err != nil || inserted {
			t.Fatalf("%s deleting assignment: inserted=%t err=%v, want false", name, inserted, err)
		}
		var deleting Replica
		if err := s.DB.GetContext(ctx, &deleting, s.Rebind(
			`SELECT * FROM snapshot_replicas WHERE snapshot_id=? AND node_id=?`), deletingSnapshot, nodeB); err != nil {
			t.Fatalf("%s read deleting replica: %v", name, err)
		}
		if deleting.State != "deleting" || deleting.AssignedAt != 300 {
			t.Fatalf("%s deleting replica changed: %+v", name, deleting)
		}

		deletedServerSnapshot := "snap_server_delete_" + name
		makeSnapshot(deletedServerSnapshot)
		if err := s.UpsertReplica(ctx, s.DB, &Replica{
			SnapshotID: deletedServerSnapshot, NodeID: nodeA, State: "ready", AssignedAt: 500,
		}); err != nil {
			t.Fatalf("%s create held replica: %v", name, err)
		}
		if err := s.DeleteServer(ctx, serverID, 600); err != nil {
			t.Fatalf("%s delete server: %v", name, err)
		}
		inserted, err = s.AssignReplica(ctx, serverID, deletedServerSnapshot, nodeB, 700)
		if err != nil || inserted {
			t.Fatalf("%s deleted-server assignment: inserted=%t err=%v, want false", name, inserted, err)
		}
		var targetRows int
		if err := s.DB.GetContext(ctx, &targetRows, s.Rebind(
			`SELECT COUNT(*) FROM snapshot_replicas WHERE snapshot_id=? AND node_id=?`),
			deletedServerSnapshot, nodeB); err != nil {
			t.Fatalf("%s count deleted-server target rows: %v", name, err)
		}
		if targetRows != 0 {
			t.Fatalf("%s deleted server got %d new replica rows", name, targetRows)
		}
		var serverRows int
		if err := s.DB.GetContext(ctx, &serverRows, s.Rebind(
			`SELECT COUNT(*) FROM servers WHERE id=?`), serverID); err != nil {
			t.Fatalf("%s count deleted server: %v", name, err)
		}
		if serverRows != 1 {
			t.Fatalf("%s deleted server purged before its replicas were acked: %d", name, serverRows)
		}
		for _, snapshotID := range []string{liveSnapshot, deletingSnapshot, deletedServerSnapshot} {
			rows, err := s.ReplicasForSnapshot(ctx, snapshotID)
			if err != nil {
				t.Fatalf("%s list replicas for %s: %v", name, snapshotID, err)
			}
			for _, row := range rows {
				if err := s.DeleteReplica(ctx, s.DB, snapshotID, row.NodeID); err != nil {
					t.Fatalf("%s ack replica for %s: %v", name, snapshotID, err)
				}
			}
		}
		if err := s.PurgeDeletedServers(ctx); err != nil {
			t.Fatalf("%s purge deleted servers: %v", name, err)
		}
		if err := s.DB.GetContext(ctx, &serverRows, s.Rebind(
			`SELECT COUNT(*) FROM servers WHERE id=?`), serverID); err != nil {
			t.Fatalf("%s recount purged server: %v", name, err)
		}
		if serverRows != 0 {
			t.Fatalf("%s server remains after purge: %d", name, serverRows)
		}
	}
}
