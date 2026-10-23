package api_test

import "testing"

func (e *env) standbyOf(serverID string) string {
	e.t.Helper()
	var sb *string
	if err := e.st.DB.Get(&sb, e.st.Rebind(`SELECT standby_node_id FROM servers WHERE id=?`), serverID); err != nil {
		e.t.Fatal(err)
	}
	if sb == nil {
		return ""
	}
	return *sb
}

func TestStandbyAssignedToTheFailoverTarget(t *testing.T) {
	e := newEnv(t)
	srv, host, other, _, _ := e.startMinecraft(e.newAgent("nodeA"), e.newAgent("nodeB"))
	e.reconcile()
	if sb := e.standbyOf(srv); sb != other.nodeID {
		t.Fatalf("standby = %q, want %s", sb, other.nodeID)
	}
	// the standby is told what to pre-install; the host is not
	d, _ := other.heartbeatCaps(nil, javaCaps())
	if len(d.Standby) != 1 || d.Standby[0].ServerID != srv ||
		d.Standby[0].Deployment.GameID != "minecraft" || d.Standby[0].Deployment.ID == "" {
		t.Fatalf("standby directive: %+v", d.Standby)
	}
	if d.Standby[0].Config["eula_accepted"] != true {
		t.Fatalf("standby directive carries no server config: %+v", d.Standby[0])
	}
	if d, _ := host.heartbeatCaps(nil, javaCaps()); len(d.Standby) != 0 {
		t.Fatalf("host got a standby directive for its own server: %+v", d.Standby)
	}
}

func TestStandbyAlwaysGetsAReplica(t *testing.T) {
	e := newEnv(t)
	a, b, n := e.newAgent("nodeA"), e.newAgent("nodeB"), e.newAgent("nas")
	// the anchor fills replication_factor 2 but can't host, so B is standby
	e.mustOK(e.do("PATCH", "/v1/nodes/"+n.nodeID, map[string]any{"anchor": true, "hosting_enabled": false}, e.token))
	for _, ag := range []*agent{a, b, n} {
		ag.heartbeatCaps(nil, javaCaps())
	}
	srv := e.createServer("minecraft", "mc", map[string]any{"preferred_node_id": a.nodeID})
	host, execA, epA := e.startToRunning(srv, a, b, n)
	if host != a {
		t.Fatalf("preferred A did not host (on %s)", host.nodeID)
	}
	e.reconcile()
	if sb := e.standbyOf(srv); sb != b.nodeID {
		t.Fatalf("standby = %q, want B", sb)
	}
	if r := a.createSnapshot(execA, srv, "dep_x", epA, "scheduled", "snap_s", "41"); r.Status != 201 {
		t.Fatalf("snap: %d %s", r.Status, r.Raw)
	}
	for name, ag := range map[string]*agent{"anchor": n, "standby": b} {
		d, _ := ag.heartbeatCaps(nil, javaCaps())
		found := false
		for _, rt := range d.ReplicationTasks {
			if rt.SnapshotID == "snap_s" {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s has no replication task for snap_s: %+v", name, d.ReplicationTasks)
		}
	}
}

func TestStandbyIsStickyAndClearedOnStop(t *testing.T) {
	e := newEnv(t)
	a, b, c := e.newAgent("nodeA"), e.newAgent("nodeB"), e.newAgent("nodeC")
	for _, ag := range []*agent{a, b, c} {
		ag.heartbeatCaps(nil, javaCaps())
	}
	srv := e.createServer("minecraft", "mc", map[string]any{"preferred_node_id": a.nodeID})
	e.startToRunning(srv, a, b, c)
	e.reconcile()
	first := e.standbyOf(srv)
	if first == "" || first == a.nodeID {
		t.Fatalf("standby = %q", first)
	}
	// the other candidate now outscores it: the standby must not move
	other := b
	if first == b.nodeID {
		other = c
	}
	better := javaCaps()
	better["uptime_s"] = 3 * 24 * 3600
	other.heartbeatCaps(nil, better)
	e.reconcile()
	if sb := e.standbyOf(srv); sb != first {
		t.Fatalf("standby moved from %s to %s on a score change", first, sb)
	}
	// ...but it does once the standby stops being eligible
	off := javaCaps()
	off["draining"] = true
	for _, ag := range []*agent{b, c} {
		if ag.nodeID == first {
			ag.heartbeatCaps(nil, off)
		}
	}
	e.reconcile()
	if sb := e.standbyOf(srv); sb != other.nodeID {
		t.Fatalf("standby = %q after %s began draining, want %s", sb, first, other.nodeID)
	}

	e.mustOK(e.do("POST", "/v1/servers/"+srv+"/stop", map[string]any{}, e.token))
	e.reconcile()
	if sb := e.standbyOf(srv); sb != "" {
		t.Fatalf("stopped server kept standby %q", sb)
	}
}

// After the host vanishes, recovery lands on the standby.
func TestFailoverLandsOnStandby(t *testing.T) {
	e := newEnv(t)
	a, b, c := e.newAgent("nodeA"), e.newAgent("nodeB"), e.newAgent("nodeC")
	for _, ag := range []*agent{a, b, c} {
		ag.heartbeatCaps(nil, javaCaps())
	}
	srv := e.createServer("minecraft", "mc", map[string]any{"preferred_node_id": a.nodeID})
	e.startToRunning(srv, a, b, c)
	e.reconcile()
	sb := e.standbyOf(srv)
	e.clk.advance(testTimings.LeaseTTLMs + testTimings.OfflineAfterMs + 1000)
	for _, ag := range []*agent{b, c} {
		ag.heartbeatCaps(nil, javaCaps())
	}
	e.reconcile()
	ex := e.activeExec(srv)
	if ex == nil || ex["node"] != sb {
		t.Fatalf("recovery placed %+v, want the standby %s", ex, sb)
	}
}
