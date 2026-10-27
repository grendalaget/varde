package api_test

import (
	"database/sql"
	"testing"
)

func committedAnchorSave(t *testing.T, e *env, snapshotIDs ...string) (string, *agent) {
	t.Helper()
	host := e.newAgent("save-host")
	serverID := e.createServer("testgame", "anchor seed", map[string]any{
		"replication_factor":  1,
		"min_commit_replicas": 1,
	})
	host, execID, epoch := e.startToRunning(serverID, host)
	for i, snapshotID := range snapshotIDs {
		if i > 0 {
			e.clk.advance(1000)
		}
		if r := host.createSnapshot(execID, serverID, "dep_x", epoch, "scheduled", snapshotID, snapshotID); r.Status != 201 {
			t.Fatalf("create snapshot %s: %d %s", snapshotID, r.Status, r.Raw)
		}
		var state string
		if err := e.st.DB.Get(&state, e.st.Rebind(`SELECT state FROM snapshots WHERE id=?`), snapshotID); err != nil {
			t.Fatal(err)
		}
		if state != "committed" {
			t.Fatalf("snapshot %s state=%q, want committed", snapshotID, state)
		}
	}
	e.mustOK(e.do("POST", "/v1/servers/"+serverID+"/stop", map[string]any{}, e.token))
	e.reconcile()
	return serverID, host
}

func makeAnchor(t *testing.T, e *env, a *agent) {
	t.Helper()
	e.mustOK(e.do("PATCH", "/v1/nodes/"+a.nodeID, map[string]any{"anchor": true}, e.token))
	if _, r := a.heartbeatCaps(nil, caps()); r.Status != 200 {
		t.Fatalf("anchor heartbeat: %d %s", r.Status, r.Raw)
	}
}

func hasReplicationTask(d directives, snapshotID, serverID string) bool {
	for _, task := range d.ReplicationTasks {
		if task.SnapshotID == snapshotID && task.ServerID == serverID {
			return true
		}
	}
	return false
}

func anchorReplica(e *env, snapshotID, nodeID string) (string, int64, sql.NullInt64, bool) {
	e.t.Helper()
	var state string
	var assignedAt int64
	var readyAt sql.NullInt64
	err := e.st.DB.QueryRow(e.st.Rebind(
		`SELECT state, assigned_at, ready_at FROM snapshot_replicas WHERE snapshot_id=? AND node_id=?`),
		snapshotID, nodeID).Scan(&state, &assignedAt, &readyAt)
	if err == sql.ErrNoRows {
		return "", 0, sql.NullInt64{}, false
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return state, assignedAt, readyAt, true
}

func TestAnchorGetsExistingSaveAndReadyReplicaIsNotReassigned(t *testing.T) {
	e := newEnv(t)
	serverID, _ := committedAnchorSave(t, e, "snap_before_anchor")
	anchor := e.newAgent("anchor")
	makeAnchor(t, e, anchor)

	e.reconcile()
	d, response := anchor.heartbeat(nil)
	if response.Status != 200 {
		t.Fatalf("heartbeat: %d %s", response.Status, response.Raw)
	}
	if !hasReplicationTask(d, "snap_before_anchor", serverID) {
		t.Fatalf("anchor got no task for the existing save: %+v", d.ReplicationTasks)
	}
	state, assignedAt, _, ok := anchorReplica(e, "snap_before_anchor", anchor.nodeID)
	if !ok || state != "assigned" {
		t.Fatalf("replica = %q, exists=%t, want assigned", state, ok)
	}
	if r := anchor.replicaReady("snap_before_anchor"); r.Status != 204 {
		t.Fatalf("replica ready: %d %s", r.Status, r.Raw)
	}
	state, assignedAtReady, readyAt, ok := anchorReplica(e, "snap_before_anchor", anchor.nodeID)
	if !ok || state != "ready" || !readyAt.Valid {
		t.Fatalf("ready replica = %q, assigned_at=%d, ready_at=%+v, exists=%t",
			state, assignedAtReady, readyAt, ok)
	}

	e.reconcile()
	state, assignedAtAgain, readyAtAgain, ok := anchorReplica(e, "snap_before_anchor", anchor.nodeID)
	if !ok || state != "ready" || assignedAtAgain != assignedAt || readyAtAgain != readyAt {
		t.Fatalf("second reconcile changed replica: state=%q assigned_at=%d ready_at=%+v exists=%t",
			state, assignedAtAgain, readyAtAgain, ok)
	}
	if n := e.count(`SELECT COUNT(*) FROM snapshot_replicas WHERE snapshot_id=? AND node_id=?`,
		"snap_before_anchor", anchor.nodeID); n != 1 {
		t.Fatalf("replica count=%d, want 1", n)
	}
}

func TestReenrolledAnchorGetsCurrentSaveAfterDeleteNode(t *testing.T) {
	e := newEnv(t)
	serverID, _ := committedAnchorSave(t, e, "snap_anchor_reenroll")
	oldAnchor := e.newAgent("old-anchor")
	makeAnchor(t, e, oldAnchor)
	e.reconcile()
	d, _ := oldAnchor.heartbeat(nil)
	if !hasReplicationTask(d, "snap_anchor_reenroll", serverID) {
		t.Fatalf("old anchor got no task: %+v", d.ReplicationTasks)
	}
	if r := oldAnchor.replicaReady("snap_anchor_reenroll"); r.Status != 204 {
		t.Fatalf("old replica ready: %d %s", r.Status, r.Raw)
	}

	if r := e.do("DELETE", "/v1/nodes/"+oldAnchor.nodeID, nil, e.token); r.Status != 204 {
		t.Fatalf("delete old anchor: %d %s", r.Status, r.Raw)
	}
	newAnchor := e.newAgent("new-anchor")
	makeAnchor(t, e, newAnchor)
	e.reconcile()
	d, response := newAnchor.heartbeat(nil)
	if response.Status != 200 {
		t.Fatalf("new anchor heartbeat: %d %s", response.Status, response.Raw)
	}
	if !hasReplicationTask(d, "snap_anchor_reenroll", serverID) {
		t.Fatalf("re-enrolled anchor got no task: %+v", d.ReplicationTasks)
	}
	if r := newAnchor.replicaReady("snap_anchor_reenroll"); r.Status != 204 {
		t.Fatalf("new replica ready: %d %s", r.Status, r.Raw)
	}
	if state, _, _, ok := anchorReplica(e, "snap_anchor_reenroll", newAnchor.nodeID); !ok || state != "ready" {
		t.Fatalf("new anchor replica = %q, exists=%t, want ready", state, ok)
	}
}

func TestAnchorSeedingSkipsOfflineNonAnchorDeletingAndOlderSnapshots(t *testing.T) {
	e := newEnv(t)
	serverID, host := committedAnchorSave(t, e, "snap_anchor_old", "snap_anchor_new")

	offline := e.newAgent("offline-anchor")
	makeAnchor(t, e, offline)
	e.clk.advance(testTimings.OfflineAfterMs + 1)
	host.heartbeat(nil)
	nonAnchor := e.newAgent("non-anchor")
	deleting := e.newAgent("deleting-anchor")
	makeAnchor(t, e, deleting)
	online := e.newAgent("online-anchor")
	makeAnchor(t, e, online)

	if _, err := e.st.DB.Exec(e.st.Rebind(
		`INSERT INTO snapshot_replicas (snapshot_id, node_id, state, assigned_at)
		 VALUES (?, ?, 'deleting', ?)`), "snap_anchor_new", deleting.nodeID, e.clk.ms); err != nil {
		t.Fatal(err)
	}
	e.reconcile()

	for name, a := range map[string]*agent{"offline anchor": offline, "non-anchor": nonAnchor} {
		if n := e.count(`SELECT COUNT(*) FROM snapshot_replicas WHERE snapshot_id=? AND node_id=?`,
			"snap_anchor_new", a.nodeID); n != 0 {
			t.Fatalf("%s received %d replica rows", name, n)
		}
	}
	if state, _, _, ok := anchorReplica(e, "snap_anchor_new", deleting.nodeID); !ok || state != "deleting" {
		t.Fatalf("deleting replica = %q, exists=%t, want deleting", state, ok)
	}
	if state, _, _, ok := anchorReplica(e, "snap_anchor_new", online.nodeID); !ok || state != "assigned" {
		t.Fatalf("online anchor replica = %q, exists=%t, want assigned", state, ok)
	}
	if n := e.count(`SELECT COUNT(*) FROM snapshot_replicas WHERE snapshot_id=? AND node_id=?`,
		"snap_anchor_old", online.nodeID); n != 0 {
		t.Fatalf("older snapshot was seeded %d times", n)
	}
	d, response := online.heartbeat(nil)
	if response.Status != 200 {
		t.Fatalf("online anchor heartbeat: %d %s", response.Status, response.Raw)
	}
	if !hasReplicationTask(d, "snap_anchor_new", serverID) ||
		hasReplicationTask(d, "snap_anchor_old", serverID) {
		t.Fatalf("online anchor tasks = %+v, want only the newest committed snapshot", d.ReplicationTasks)
	}
}
