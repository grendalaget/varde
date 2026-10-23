package api_test

import "testing"

// drainingCaps is the heartbeat body of an agent that began a clean shutdown.
func drainingCaps(runtimes ...string) map[string]any {
	c := caps()
	c["runtimes"] = runtimes
	c["draining"] = true
	return c
}

func javaCaps() map[string]any {
	c := caps()
	c["runtimes"] = []string{"native", "java"}
	return c
}

func (e *env) openMigration(serverID string) map[string]any {
	e.t.Helper()
	var m struct {
		ID       string `db:"id"`
		ToNodeID string `db:"to_node_id"`
		Mode     string `db:"mode"`
		State    string `db:"state"`
	}
	if err := e.st.DB.Get(&m, e.st.Rebind(
		`SELECT id,to_node_id,mode,state FROM migrations WHERE server_id=? AND finished_at IS NULL`), serverID); err != nil {
		return nil
	}
	return map[string]any{"id": m.ID, "to": m.ToNodeID, "mode": m.Mode, "state": m.State}
}

func (e *env) migrationState(id string) string {
	e.t.Helper()
	var st string
	if err := e.st.DB.Get(&st, e.st.Rebind(`SELECT state FROM migrations WHERE id=?`), id); err != nil {
		e.t.Fatal(err)
	}
	return st
}

// startMinecraft brings a Minecraft server up on one of two java-capable
// agents and returns (host, other, exec id, epoch).
func (e *env) startMinecraft(a, b *agent) (string, *agent, *agent, string, int64) {
	e.t.Helper()
	a.heartbeatCaps(nil, javaCaps())
	b.heartbeatCaps(nil, javaCaps())
	srv := e.createServer("minecraft", "mc", nil)
	host, execID, epoch := e.startToRunning(srv, a, b)
	other := b
	if host == b {
		other = a
	}
	return srv, host, other, execID, epoch
}

func TestShutdownHandoffWarmMigrates(t *testing.T) {
	e := newEnv(t)
	srv, host, other, execA, epA := e.startMinecraft(e.newAgent("nodeA"), e.newAgent("nodeB"))

	// the host starts a clean shutdown: draining, server still running
	host.heartbeatCaps(nil, drainingCaps("native", "java"))
	e.reconcile()
	m := e.openMigration(srv)
	if m == nil || m["mode"] != "warm" || m["to"] != other.nodeID {
		t.Fatalf("expected warm migration to %s, got %+v", other.nodeID, m)
	}
	// a second pass must not open another migration
	e.reconcile()
	if again := e.openMigration(srv); again == nil || again["id"] != m["id"] {
		t.Fatalf("migration changed on second pass: %+v", again)
	}

	// the source is frozen (the agent's ack), not stopped
	d, _ := host.heartbeatCaps(nil, drainingCaps("native", "java"))
	frozen := false
	for _, ex := range d.Executions {
		if ex.ExecutionID == execA {
			if ex.Action == "stop" {
				t.Fatal("shutdown handoff stopped the source before the flip")
			}
			frozen = ex.Freeze
		}
	}
	if !frozen {
		t.Fatalf("source exec not frozen: %+v", d.Executions)
	}

	// migration snapshot → replica on target → incoming running → flip
	if r := host.createSnapshot(execA, srv, "dep_x", epA, "migration", "snap_h", "11"); r.Status != 201 {
		t.Fatalf("migration snap: %d %s", r.Status, r.Raw)
	}
	e.reconcile()
	if r := other.replicaReady("snap_h"); r.Status != 204 {
		t.Fatalf("replica ready: %d %s", r.Status, r.Raw)
	}
	e.reconcile()
	d, _ = other.heartbeatCaps(nil, javaCaps())
	if len(d.Executions) != 1 || d.Executions[0].Restore == nil || d.Executions[0].Restore.SnapshotID != "snap_h" {
		t.Fatalf("incoming exec on target: %+v", d.Executions)
	}
	execB, epB := d.Executions[0].ExecutionID, d.Executions[0].Epoch
	if r := other.execStatus(execB, srv, epB, "running"); r.Status != 204 {
		t.Fatalf("running: %d %s", r.Status, r.Raw)
	}
	e.reconcile()
	if ex := e.activeExec(srv); ex == nil || ex["id"] != execB {
		t.Fatalf("target should serve after the flip: %+v", ex)
	}
	d, _ = host.heartbeatCaps(nil, drainingCaps("native", "java"))
	handoff := false
	for _, ex := range d.Executions {
		if ex.ExecutionID == execA && ex.Action == "stop" && ex.StopReason == "handoff" {
			handoff = true
		}
	}
	if !handoff {
		t.Fatalf("source did not get a handoff stop: %+v", d.Executions)
	}
	if st := e.migrationState(m["id"].(string)); st != "done" {
		t.Fatalf("migration state %q", st)
	}
}

func TestShutdownHandoffWithoutTargetLeavesServerRunning(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("nodeA")
	a.heartbeatCaps(nil, javaCaps())
	srv := e.createServer("minecraft", "mc", nil)
	_, execA, _ := e.startToRunning(srv, a)

	a.heartbeatCaps(nil, drainingCaps("native", "java"))
	e.reconcile()
	if m := e.openMigration(srv); m != nil {
		t.Fatalf("no other node can host, but a migration opened: %+v", m)
	}
	// the CP leaves the stop to the agent's fallback
	d, _ := a.heartbeatCaps(nil, drainingCaps("native", "java"))
	for _, ex := range d.Executions {
		if ex.ExecutionID == execA && (ex.Action != "run" || ex.Freeze) {
			t.Fatalf("source exec changed without a target: %+v", ex)
		}
	}
}

func TestShutdownHandoffSkipsGamesWithoutHandoff(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("nodeA")
	b := e.newAgent("nodeB")
	srv := e.createServer("testgame", "tg", nil)
	host, _, _ := e.startToRunning(srv, a, b)

	host.heartbeatCaps(nil, drainingCaps("native"))
	e.reconcile()
	if m := e.openMigration(srv); m != nil {
		t.Fatalf("migration opened for a game without handoff: %+v", m)
	}
}

// The host gives up waiting and starts its graceful stop while the target is
// already up: the final save and replication hold keep the source alive, so
// the CP must abort on 'stopping' rather than flip to the older migration
// snapshot. Recovery then restarts from the final save once the source ends.
func TestShutdownHandoffAbortsWhenSourceStartsStopping(t *testing.T) {
	e := newEnv(t)
	srv, host, other, execA, epA := e.startMinecraft(e.newAgent("nodeA"), e.newAgent("nodeB"))

	host.heartbeatCaps(nil, drainingCaps("native", "java"))
	e.reconcile()
	m := e.openMigration(srv)
	if m == nil {
		t.Fatal("no shutdown migration")
	}
	if r := host.createSnapshot(execA, srv, "dep_x", epA, "migration", "snap_m", "21"); r.Status != 201 {
		t.Fatalf("migration snap: %d %s", r.Status, r.Raw)
	}
	e.reconcile()
	if r := other.replicaReady("snap_m"); r.Status != 204 {
		t.Fatalf("replica ready: %d %s", r.Status, r.Raw)
	}
	e.reconcile()
	d, _ := other.heartbeatCaps(nil, javaCaps())
	if len(d.Executions) != 1 {
		t.Fatalf("incoming exec on target: %+v", d.Executions)
	}
	execB, epB := d.Executions[0].ExecutionID, d.Executions[0].Epoch

	// fallback begins: source reports stopping; the target comes up in the
	// same window — the flip must not happen
	if r := host.execStatus(execA, srv, epA, "stopping"); r.Status != 204 {
		t.Fatalf("stopping: %d %s", r.Status, r.Raw)
	}
	if r := other.execStatus(execB, srv, epB, "running"); r.Status != 204 {
		t.Fatalf("incoming running: %d %s", r.Status, r.Raw)
	}
	e.reconcile()
	if st := e.migrationState(m["id"].(string)); st != "failed" {
		t.Fatalf("migration state %q, want failed", st)
	}
	if ex := e.activeExec(srv); ex == nil || ex["id"] != execA {
		t.Fatalf("source must stay the serving exec until it ends: %+v", ex)
	}
	var ended int
	if err := e.st.DB.Get(&ended, e.st.Rebind(
		`SELECT COUNT(*) FROM server_executions WHERE id=? AND ended_at IS NOT NULL`), execB); err != nil {
		t.Fatal(err)
	}
	if ended != 1 {
		t.Fatal("staged incoming exec was not ended")
	}

	// final save replicates (the agent's hold), then the source ends
	e.clk.advance(2000)
	if r := host.createSnapshot(execA, srv, "dep_x", epA, "final", "snap_f", "22"); r.Status != 201 {
		t.Fatalf("final snap: %d %s", r.Status, r.Raw)
	}
	e.reconcile()
	if r := other.replicaReady("snap_f"); r.Status != 204 {
		t.Fatalf("final replica ready: %d %s", r.Status, r.Raw)
	}
	if r := host.execStatus(execA, srv, epA, "stopped"); r.Status != 204 {
		t.Fatalf("stopped: %d %s", r.Status, r.Raw)
	}
	e.reconcile()
	ex := e.activeExec(srv)
	if ex == nil || ex["node"] != other.nodeID {
		t.Fatalf("recovery should re-place on %s: %+v", other.nodeID, ex)
	}
	d, _ = other.heartbeatCaps(nil, javaCaps())
	var restore string
	for _, ed := range d.Executions {
		if ed.ExecutionID == ex["id"] && ed.Restore != nil {
			restore = ed.Restore.SnapshotID
		}
	}
	if restore != "snap_f" {
		t.Fatalf("recovery restores %q, want the final save snap_f", restore)
	}
}

// A target without room for the save is skipped for one that has it.
func TestShutdownHandoffSkipsTargetWithoutDiskForTheSave(t *testing.T) {
	e := newEnv(t)
	a, b, c := e.newAgent("nodeA"), e.newAgent("nodeB"), e.newAgent("nodeC")
	for _, ag := range []*agent{a, b, c} {
		ag.heartbeatCaps(nil, javaCaps())
	}
	srv := e.createServer("minecraft", "mc", map[string]any{"preferred_node_id": a.nodeID})
	host, execA, epA := e.startToRunning(srv, a, b, c)
	if host != a {
		t.Fatalf("preferred node A did not host (on %s)", host.nodeID)
	}
	// a committed save on A + C (anchors/peers: C gets the replica task)
	if r := a.createSnapshot(execA, srv, "dep_x", epA, "scheduled", "snap_big", "31"); r.Status != 201 {
		t.Fatalf("snap: %d %s", r.Status, r.Raw)
	}
	e.reconcile()
	if r := c.replicaReady("snap_big"); r.Status != 204 {
		t.Fatalf("replica on C: %d %s", r.Status, r.Raw)
	}
	e.reconcile()
	var stored int64
	if err := e.st.DB.Get(&stored, e.st.Rebind(`SELECT stored_bytes FROM snapshots WHERE id=?`), "snap_big"); err != nil {
		t.Fatal(err)
	}
	// B outscores C (long uptime vs. a user gaming on C) but can't fit the
	// save: without the restore-size check the handoff would pick B
	bigUp := javaCaps()
	bigUp["disk_free_bytes"] = stored / 4
	bigUp["uptime_s"] = 2 * 24 * 3600
	b.heartbeatCaps(nil, bigUp)
	busy := javaCaps()
	busy["user_active"] = true
	c.heartbeatCaps(nil, busy)
	a.heartbeatCaps(nil, drainingCaps("native", "java"))
	e.reconcile()
	m := e.openMigration(srv)
	if m == nil || m["to"] != c.nodeID {
		t.Fatalf("handoff target = %+v, want %s (B lacks disk for the save)", m, c.nodeID)
	}
}
