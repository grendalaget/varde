package api_test

import (
	"encoding/json"
	"testing"

	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.st.DB.Get(&n, e.st.Rebind(query), args...); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// restoreFor returns the restore snapshot a's directives carry for srv.
func restoreFor(a *agent, srv string) string {
	d, _ := a.heartbeat(nil)
	for _, ex := range d.Executions {
		if ex.ServerID == srv && ex.Restore != nil {
			return ex.Restore.SnapshotID
		}
	}
	return ""
}

func (a *agent) postLogs(execID string) apiResp {
	body, _ := json.Marshal(map[string]any{"lines": []map[string]any{
		{"at": a.e.clk.ms, "stream": "stdout", "line": "hello"},
	}})
	return a.signedDo("POST", "/v1/agent/executions/"+execID+"/logs", body, 0, nil)
}

// twoSaves runs srv on one of a/b and leaves it stopped with snap_old
// (committed, only on the other node) and snap_new (committed, only on the
// host). Returns host and other.
func (e *env) twoSaves(srv string, a, b *agent) (*agent, *agent) {
	e.t.Helper()
	host, execA, epA := e.startToRunning(srv, a, b)
	other := a
	if host == a {
		other = b
	}
	for _, id := range []string{"snap_old", "snap_new"} {
		if r := host.createSnapshot(execA, srv, "dep_x", epA, "scheduled", id, id); r.Status != 201 {
			e.t.Fatalf("%s %d %s", id, r.Status, r.Raw)
		}
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE snapshots SET state='committed', committed_at=? WHERE id IN ('snap_old','snap_new')`, []any{e.clk.ms}},
		{`DELETE FROM snapshot_replicas WHERE snapshot_id='snap_old' AND node_id!=?`, []any{other.nodeID}},
		{`DELETE FROM snapshot_replicas WHERE snapshot_id='snap_new' AND node_id!=?`, []any{host.nodeID}},
	} {
		if _, err := e.st.DB.Exec(e.st.Rebind(q.sql), q.args...); err != nil {
			e.t.Fatal(err)
		}
	}
	if r := other.replicaReady("snap_old"); r.Status != 204 {
		e.t.Fatalf("replica ready: %d %s", r.Status, r.Raw)
	}
	e.mustOK(e.do("POST", "/v1/servers/"+srv+"/stop", nil, e.token))
	e.reconcile()
	if r := host.execStatus(execA, srv, epA, "stopped"); r.Status != 204 {
		e.t.Fatalf("stopped %d %s", r.Status, r.Raw)
	}
	e.reconcile()
	return host, other
}

// /start writes desired=running only together with its execution, so a
// refused start leaves desired alone. And a pass that finds desired=running
// with no active execution and no lost host must apply the normal rules, not
// recovery's "fall back to an older committed save".
func TestDesiredRunningWithoutLostHostUsesNormalRules(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("nodeA")
	b := e.newAgent("nodeB")
	srv := e.createServer("testgame", "s1", nil)
	host, other := e.twoSaves(srv, a, b)

	// host offline: snap_new unreachable, snap_old reachable on other
	e.clk.advance(testTimings.OfflineAfterMs + 5000)
	other.heartbeat(nil)
	e.reconcile()

	r := e.do("POST", "/v1/servers/"+srv+"/start", map[string]any{}, e.token)
	if r.Status != 409 || r.Body["code"] != "latest_save_unavailable" {
		t.Fatalf("want 409 latest_save_unavailable, got %d %s", r.Status, r.Raw)
	}
	if n := e.count(`SELECT COUNT(*) FROM servers WHERE id=? AND desired_state='stopped'`, srv); n != 1 {
		t.Fatal("a refused start changed desired_state")
	}

	if err := e.st.UpdateServerDesired(t.Context(), srv, "running", e.clk.ms); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if ex := e.activeExec(srv); ex != nil {
		t.Fatalf("background pass activated from an older save: %+v (restore %s)",
			ex, restoreFor(other, srv))
	}
	if got := e.observed(srv); got != "stopped" {
		t.Fatalf("observed=%s, want stopped", got)
	}
	if n := e.count(`SELECT COUNT(*) FROM events WHERE server_id=? AND type='server.save_unavailable'`, srv); n != 0 {
		t.Fatalf("normal-rule wait emitted %d save_unavailable events", n)
	}

	// the newest save's machine returns: the loop starts it from snap_new
	e.clk.advance(1000)
	host.heartbeat(nil)
	other.heartbeat(nil)
	e.reconcile()
	ex := e.activeExec(srv)
	if ex == nil {
		t.Fatal("no execution once the newest save was reachable")
	}
	target := host
	if ex["node"] == other.nodeID {
		target = other
	}
	if got := restoreFor(target, srv); got != "snap_new" {
		t.Fatalf("restore=%q, want snap_new", got)
	}
}

func TestDeleteServer(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("nodeA")
	b := e.newAgent("nodeB")
	srv := e.createServer("testgame", "doomed", nil)
	host, execID, epoch := e.startToRunning(srv, a, b)
	other := a
	if host == a {
		other = b
	}
	if r := host.createSnapshot(execID, srv, "dep_x", epoch, "scheduled", "snap_d", "dd"); r.Status != 201 {
		t.Fatalf("snapshot %d %s", r.Status, r.Raw)
	}
	if r := other.replicaReady("snap_d"); r.Status != 204 {
		t.Fatalf("replica ready %d %s", r.Status, r.Raw)
	}
	if r := host.postLogs(execID); r.Status != 204 {
		t.Fatalf("logs %d %s", r.Status, r.Raw)
	}

	r := e.do("DELETE", "/v1/servers/"+srv, nil, e.token)
	if r.Status != 409 || r.Body["code"] != "conflict" {
		t.Fatalf("delete while running: want 409 conflict, got %d %s", r.Status, r.Raw)
	}
	e.mustOK(e.do("POST", "/v1/servers/"+srv+"/stop", nil, e.token))
	e.reconcile()
	if r := host.execStatus(execID, srv, epoch, "stopped"); r.Status != 204 {
		t.Fatalf("stopped %d %s", r.Status, r.Raw)
	}
	e.reconcile()

	if r := e.do("DELETE", "/v1/servers/"+srv, nil, e.token); r.Status != 204 {
		t.Fatalf("delete: %d %s", r.Status, r.Raw)
	}
	if r := e.do("GET", "/v1/servers/"+srv, nil, e.token); r.Status != 404 {
		t.Fatalf("get after delete: %d %s", r.Status, r.Raw)
	}
	list := e.mustOK(e.do("GET", "/v1/groups/"+e.group+"/servers", nil, e.token))
	if servers := list["servers"].([]any); len(servers) != 0 {
		t.Fatalf("deleted server still listed: %v", servers)
	}
	if r := e.do("POST", "/v1/servers/"+srv+"/start", map[string]any{}, e.token); r.Status != 404 {
		t.Fatalf("start after delete: %d %s", r.Status, r.Raw)
	}
	e.reconcile()
	if e.activeExecCount(srv) != 0 {
		t.Fatal("reconciler activated a deleted server")
	}

	// every machine is told to delete its copy; no route remains
	for _, ag := range []*agent{host, other} {
		d, _ := ag.heartbeat(nil)
		if len(d.DeleteSnapshots) != 1 || d.DeleteSnapshots[0] != "snap_d" {
			t.Fatalf("%s delete directives: %+v", ag.nodeID, d.DeleteSnapshots)
		}
		for _, rt := range d.Routes {
			if rt.ServerID == srv {
				t.Fatalf("route for deleted server sent to %s", ag.nodeID)
			}
		}
	}

	host.heartbeatWithDeletedSnapshots([]string{"snap_d"})
	e.reconcile()
	if n := e.count(`SELECT COUNT(*) FROM servers WHERE id=?`, srv); n != 1 {
		t.Fatal("server purged while a machine still holds a copy of its save")
	}
	other.heartbeatWithDeletedSnapshots([]string{"snap_d"})
	e.reconcile()
	for _, q := range []string{
		`SELECT COUNT(*) FROM servers WHERE id=?`,
		`SELECT COUNT(*) FROM services WHERE server_id=?`,
		`SELECT COUNT(*) FROM snapshots WHERE server_id=?`,
		`SELECT COUNT(*) FROM server_executions WHERE server_id=?`,
		`SELECT COUNT(*) FROM snapshot_replicas WHERE snapshot_id='snap_d' AND ? != ''`,
	} {
		if n := e.count(q, srv); n != 0 {
			t.Fatalf("%s: %d rows remain after the last ack", q, n)
		}
	}
	if n := e.count(`SELECT COUNT(*) FROM execution_logs WHERE execution_id=?`, execID); n != 0 {
		t.Fatalf("%d log lines remain", n)
	}
}

func TestDeleteNode(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("nodeA")
	b := e.newAgent("nodeB")
	srv := e.createServer("testgame", "s1", nil)
	host, execID, epoch := e.startToRunning(srv, a, b)
	other := a
	if host == a {
		other = b
	}
	// snap_both: ready on both. snap_host: only on host. snap_partial: ready
	// on host, other still copying.
	for _, id := range []string{"snap_both", "snap_host", "snap_partial"} {
		if r := host.createSnapshot(execID, srv, "dep_x", epoch, "scheduled", id, id); r.Status != 201 {
			t.Fatalf("%s %d %s", id, r.Status, r.Raw)
		}
	}
	if r := other.replicaReady("snap_both"); r.Status != 204 {
		t.Fatalf("replica ready %d %s", r.Status, r.Raw)
	}
	if _, err := e.st.DB.Exec(e.st.Rebind(
		`DELETE FROM snapshot_replicas WHERE snapshot_id='snap_host' AND node_id!=?`), host.nodeID); err != nil {
		t.Fatal(err)
	}

	if r := e.do("DELETE", "/v1/nodes/"+host.nodeID, nil, e.token); r.Status != 204 {
		t.Fatalf("delete node: %d %s", r.Status, r.Raw)
	}
	if r := e.do("GET", "/v1/nodes/"+host.nodeID, nil, e.token); r.Status != 404 {
		t.Fatalf("get deleted node: %d %s", r.Status, r.Raw)
	}
	list := e.mustOK(e.do("GET", "/v1/groups/"+e.group+"/nodes", nil, e.token))
	if nodes := list["nodes"].([]any); len(nodes) != 1 {
		t.Fatalf("nodes after delete: %v", nodes)
	}
	if _, r := host.heartbeat(nil); r.Status < 400 {
		t.Fatalf("deleted node's heartbeat accepted: %d", r.Status)
	}
	if n := e.count(`SELECT COUNT(*) FROM snapshot_replicas WHERE node_id=?`, host.nodeID); n != 0 {
		t.Fatalf("%d replica rows remain on the deleted node", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM node_status WHERE node_id=?`, host.nodeID); n != 0 {
		t.Fatal("node_status remains")
	}
	if n := e.count(`SELECT COUNT(*) FROM snapshots WHERE id='snap_host' AND ? != ''`, srv); n != 0 {
		t.Fatal("a save with no copy left is still listed")
	}
	var state string
	if err := e.st.DB.Get(&state, `SELECT state FROM snapshots WHERE id='snap_partial'`); err != nil {
		t.Fatal(err)
	}
	if state != "invalid" {
		t.Fatalf("snap_partial state=%s, want invalid", state)
	}
	d, hb := other.heartbeat(nil)
	if len(d.DeleteSnapshots) != 1 || d.DeleteSnapshots[0] != "snap_partial" {
		t.Fatalf("partial copy not deleted: %+v", d.DeleteSnapshots)
	}
	var peers struct {
		Peers []struct {
			NodeID string `json:"node_id"`
		} `json:"peers"`
	}
	if err := json.Unmarshal(hb.Raw, &peers); err != nil {
		t.Fatal(err)
	}
	for _, p := range peers.Peers {
		if p.NodeID == host.nodeID {
			t.Fatal("deleted node still sent as a peer")
		}
	}

	// the running execution keeps its lease (fencing) and is then lost;
	// recovery restarts on the remaining node from snap_both
	if e.activeExec(srv) == nil {
		t.Fatal("execution ended before its lease ran out")
	}
	e.clk.advance(testTimings.LeaseTTLMs + 1000)
	other.heartbeat(nil)
	e.reconcile()
	ex := e.activeExec(srv)
	if ex == nil || ex["node"] != other.nodeID {
		t.Fatalf("no recovery on the remaining node: %+v", ex)
	}
	if got := restoreFor(other, srv); got != "snap_both" {
		t.Fatalf("recovery restore=%q, want snap_both", got)
	}
}

func TestDeleteGroup(t *testing.T) {
	e := newEnv(t)
	g2 := e.createGroup("g2")
	a := e.newAgentInGroup("nodeA", g2)
	b := e.mustOK(e.do("POST", "/v1/groups/"+g2+"/servers",
		map[string]any{"game_id": "testgame", "name": "s"}, e.token))
	srv := b["id"].(string)
	if err := e.st.CreateInvite(t.Context(), &store.Invite{
		Code: "inv_g2", GroupID: g2, Role: "member", CreatedBy: "op",
	}); err != nil {
		t.Fatal(err)
	}
	host, execID, epoch := e.startToRunning(srv, a)
	if r := host.execStatusJoinCode(execID, srv, epoch, "running", "JOIN1"); r.Status != 204 {
		t.Fatalf("join code %d %s", r.Status, r.Raw)
	}
	if r := host.postLogs(execID); r.Status != 204 {
		t.Fatalf("logs %d %s", r.Status, r.Raw)
	}
	if r := host.createSnapshot(execID, srv, "dep_x", epoch, "scheduled", "snap_g", "gg"); r.Status != 201 {
		t.Fatalf("snapshot %d %s", r.Status, r.Raw)
	}

	refused := func(why string) {
		t.Helper()
		r := e.do("DELETE", "/v1/groups/"+g2, nil, e.token)
		if r.Status != 409 || r.Body["code"] != "conflict" {
			t.Fatalf("%s: want 409 conflict, got %d %s", why, r.Status, r.Raw)
		}
	}
	refused("servers and machines")
	e.mustOK(e.do("POST", "/v1/servers/"+srv+"/stop", nil, e.token))
	e.reconcile()
	if r := host.execStatus(execID, srv, epoch, "stopped"); r.Status != 204 {
		t.Fatalf("stopped %d %s", r.Status, r.Raw)
	}
	if r := e.do("DELETE", "/v1/servers/"+srv, nil, e.token); r.Status != 204 {
		t.Fatalf("delete server: %d %s", r.Status, r.Raw)
	}
	refused("machine")
	// the machine goes before acking: the deleted server's rows are still
	// there for the group delete to clear
	if r := e.do("DELETE", "/v1/nodes/"+a.nodeID, nil, e.token); r.Status != 204 {
		t.Fatalf("delete node: %d %s", r.Status, r.Raw)
	}
	if r := e.do("DELETE", "/v1/groups/"+g2, nil, e.token); r.Status != 204 {
		t.Fatalf("delete group: %d %s", r.Status, r.Raw)
	}
	for _, q := range []string{
		`SELECT COUNT(*) FROM groups WHERE id=?`,
		`SELECT COUNT(*) FROM group_members WHERE group_id=?`,
		`SELECT COUNT(*) FROM invites WHERE group_id=?`,
		`SELECT COUNT(*) FROM enrollment_tokens WHERE group_id=?`,
		`SELECT COUNT(*) FROM nodes WHERE group_id=?`,
		`SELECT COUNT(*) FROM servers WHERE group_id=?`,
		`SELECT COUNT(*) FROM snapshots WHERE group_id=?`,
		`SELECT COUNT(*) FROM deployments WHERE group_id=?`,
	} {
		if n := e.count(q, g2); n != 0 {
			t.Fatalf("%s: %d rows remain", q, n)
		}
	}
	if n := e.count(`SELECT COUNT(*) FROM server_executions WHERE id=?`, execID); n != 0 {
		t.Fatal("executions remain")
	}
	if r := e.do("DELETE", "/v1/groups/"+e.group, nil, e.token); r.Status != 204 {
		t.Fatalf("delete empty group: %d %s", r.Status, r.Raw)
	}
	e.reconcile()
}
