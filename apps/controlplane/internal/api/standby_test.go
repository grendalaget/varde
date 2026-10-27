package api_test

import (
	"strings"
	"testing"

	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

func TestCS2GameAddonPlacementRequiresLinux(t *testing.T) {
	e := newEnv(t)
	windows, linux := e.newAgent("windows"), e.newAgent("linux")
	configureCS2PlacementNode(t, e, windows, "windows")
	configureCS2PlacementNode(t, e, linux, "linux")
	serverID := e.createServer("cs2", "cs2 addon", map[string]any{
		"preferred_node_id": windows.nodeID,
		"config":            map[string]any{"server_name": "CS2 addon placement"},
	})
	addServerAddonTarget(t, e, serverID, "game")

	host, _, _ := e.startToRunning(serverID, windows, linux)

	if host != linux {
		t.Fatalf("CS2 game addon started on %s, want Linux node %s", host.nodeID, linux.nodeID)
	}
}

func TestCS2GameAddonIsUnschedulableOnWindowsOnly(t *testing.T) {
	e := newEnv(t)
	windows := e.newAgent("windows")
	configureCS2PlacementNode(t, e, windows, "windows")
	serverID := e.createServer("cs2", "cs2 addon", map[string]any{
		"config": map[string]any{"server_name": "CS2 addon placement"},
	})
	addServerAddonTarget(t, e, serverID, "game")

	resp := e.do("POST", "/v1/servers/"+serverID+"/start", map[string]any{}, e.token)

	if resp.Status != 400 || !strings.Contains(string(resp.Raw), "unsupported os windows") {
		t.Fatalf("start CS2 with game addon on Windows = %d %s, want unschedulable unsupported os windows", resp.Status, resp.Raw)
	}
	if got := e.activeExec(serverID); got != nil {
		t.Fatalf("Windows-only addon server got active execution %+v", got)
	}
}

func TestCS2WithoutAddonCanUseWindows(t *testing.T) {
	e := newEnv(t)
	windows := e.newAgent("windows")
	configureCS2PlacementNode(t, e, windows, "windows")
	serverID := e.createServer("cs2", "cs2", map[string]any{
		"config": map[string]any{"server_name": "CS2 without addon"},
	})

	host, _, _ := e.startToRunning(serverID, windows)

	if host != windows {
		t.Fatalf("CS2 without addon started on %s, want Windows node %s", host.nodeID, windows.nodeID)
	}
}

func TestCS2AddonStandbyAndPreinstallAvoidWindows(t *testing.T) {
	e := newEnv(t)
	host, windows, linuxStandby := e.newAgent("host"), e.newAgent("windows"), e.newAgent("linux-standby")
	configureCS2PlacementNode(t, e, host, "linux")
	configureCS2PlacementNode(t, e, windows, "windows")
	configureCS2PlacementNode(t, e, linuxStandby, "linux")
	for _, a := range []*agent{host, windows} {
		e.mustOK(e.do("PATCH", "/v1/nodes/"+a.nodeID, map[string]any{"anchor": true}, e.token))
	}
	serverID := e.createServer("cs2", "cs2 addon", map[string]any{
		"preferred_node_id": host.nodeID,
		"config":            map[string]any{"server_name": "CS2 addon standby"},
	})
	addServerAddonTarget(t, e, serverID, "game")

	actualHost, _, _ := e.startToRunning(serverID, host, windows, linuxStandby)
	if actualHost != host {
		t.Fatalf("CS2 game addon started on %s, want preferred Linux node %s", actualHost.nodeID, host.nodeID)
	}
	if got := e.standbyOf(serverID); got != linuxStandby.nodeID {
		t.Fatalf("CS2 game addon standby = %q, want Linux node %s", got, linuxStandby.nodeID)
	}

	if _, err := e.st.DB.Exec(`UPDATE servers SET standby_node_id=? WHERE id=?`, windows.nodeID, serverID); err != nil {
		t.Fatal(err)
	}
	d, _ := windows.heartbeatCaps(nil, cs2PlacementCaps("windows"))
	if len(d.Standby) != 0 {
		t.Fatalf("Windows agent got standby preinstall for CS2 game addon: %+v", d.Standby)
	}
	e.clk.advance(testTimings.LeaseTTLMs + testTimings.OfflineAfterMs + 1000)
	windows.heartbeatCaps(nil, cs2PlacementCaps("windows"))
	linuxStandby.heartbeatCaps(nil, cs2PlacementCaps("linux"))
	e.reconcile()
	if recovered := e.activeExec(serverID); recovered == nil || recovered["node"] == windows.nodeID {
		t.Fatalf("recovery with stale Windows standby = %+v, want Linux node", recovered)
	}
}

func configureCS2PlacementNode(t *testing.T, e *env, a *agent, os string) {
	t.Helper()
	if _, err := e.st.DB.Exec(`UPDATE nodes SET os=? WHERE id=?`, os, a.nodeID); err != nil {
		t.Fatal(err)
	}
	_, resp := a.heartbeatCaps(nil, cs2PlacementCaps(os))
	if resp.Status != 200 {
		t.Fatalf("heartbeatCaps(os=%s) = %d %s, want 200", os, resp.Status, resp.Raw)
	}
}

func cs2PlacementCaps(os string) map[string]any {
	c := caps()
	c["os"] = os
	c["runtimes"] = []string{"steamcmd"}
	c["steam_apps"] = []int64{730}
	return c
}

func addServerAddonTarget(t *testing.T, e *env, serverID, target string) {
	t.Helper()
	if err := e.st.UpsertServerAddon(t.Context(), store.ServerAddon{
		ServerID: serverID, PluginID: "test-plugin", AddonID: "readyup",
		Target: target, Spec: "sealed", CreatedAt: e.clk.ms, UpdatedAt: e.clk.ms,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCS2StandbyAndRecoveryUseInstalledSteamDiskPolicy(t *testing.T) {
	for _, installed := range []bool{false, true} {
		name := "fallback needs install space"
		if installed {
			name = "installed app bypasses install budget"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			a, b := e.newAgent("host"), e.newAgent("standby")
			hostCaps := caps()
			hostCaps["steam_apps"] = []int64{730}
			a.heartbeatCaps(nil, hostCaps)
			standbyCaps := caps()
			standbyCaps["disk_free_bytes"] = 1_000_000
			if installed {
				standbyCaps["steam_apps"] = []int64{730}
			} else {
				standbyCaps["runtimes"] = []string{"native", "steamcmd"}
			}
			b.heartbeatCaps(nil, standbyCaps)
			srv := e.createServer("cs2", "cs2", map[string]any{
				"preferred_node_id": a.nodeID,
				"config":            map[string]any{"server_name": "CS2 standby test"},
			})
			host, _, _ := e.startToRunning(srv, a, b)
			if host != a {
				t.Fatalf("host = %s, want %s", host.nodeID, a.nodeID)
			}
			e.reconcile()
			want := ""
			if installed {
				want = b.nodeID
			}
			if got := e.standbyOf(srv); got != want {
				t.Fatalf("standby = %q, want %q", got, want)
			}
			e.clk.advance(testTimings.LeaseTTLMs + testTimings.OfflineAfterMs + 1000)
			b.heartbeatCaps(nil, standbyCaps)
			e.reconcile()
			ex := e.activeExec(srv)
			if installed {
				if ex == nil || ex["node"] != b.nodeID {
					t.Fatalf("recovery = %+v, want installed standby %s", ex, b.nodeID)
				}
			} else if ex != nil {
				t.Fatalf("fallback without install space became eligible: %+v", ex)
			}
		})
	}
}

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

// A standby chosen after a save exists still gets that save.
func TestReplacementStandbyGetsExistingSave(t *testing.T) {
	e := newEnv(t)
	a, b, c := e.newAgent("nodeA"), e.newAgent("nodeB"), e.newAgent("nodeC")
	for _, ag := range []*agent{a, b, c} {
		ag.heartbeatCaps(nil, javaCaps())
	}
	// factor 1: only the standby gets a copy, so a later standby can only
	// get this save by being seeded
	srv := e.createServer("minecraft", "mc", map[string]any{
		"preferred_node_id": a.nodeID, "replication_factor": 1, "min_commit_replicas": 1,
	})
	_, execA, epA := e.startToRunning(srv, a, b, c)
	e.reconcile()
	first, next := b, c
	if e.standbyOf(srv) == c.nodeID {
		first, next = c, b
	}
	if r := a.createSnapshot(execA, srv, "dep_x", epA, "scheduled", "snap_old", "51"); r.Status != 201 {
		t.Fatalf("snap: %d %s", r.Status, r.Raw)
	}
	e.reconcile()
	if r := first.replicaReady("snap_old"); r.Status != 204 {
		t.Fatalf("replica on first standby: %d %s", r.Status, r.Raw)
	}
	e.reconcile()
	off := javaCaps()
	off["draining"] = true
	first.heartbeatCaps(nil, off)
	e.reconcile()
	if sb := e.standbyOf(srv); sb != next.nodeID {
		t.Fatalf("standby = %q, want %s", sb, next.nodeID)
	}
	d, _ := next.heartbeatCaps(nil, javaCaps())
	for _, rt := range d.ReplicationTasks {
		if rt.SnapshotID == "snap_old" {
			return
		}
	}
	t.Fatalf("new standby has no replication task for the existing save: %+v", d.ReplicationTasks)
}

// Recovery honors the standby even when another node now scores higher.
func TestFailoverPrefersStandbyOverHigherScore(t *testing.T) {
	e := newEnv(t)
	a, b, c := e.newAgent("nodeA"), e.newAgent("nodeB"), e.newAgent("nodeC")
	for _, ag := range []*agent{a, b, c} {
		ag.heartbeatCaps(nil, javaCaps())
	}
	srv := e.createServer("minecraft", "mc", map[string]any{"preferred_node_id": a.nodeID})
	e.startToRunning(srv, a, b, c)
	e.reconcile()
	sb := e.standbyOf(srv)
	other := b
	if sb == b.nodeID {
		other = c
	}
	// the other node becomes an anchor (+80): sticky standby stays put
	e.mustOK(e.do("PATCH", "/v1/nodes/"+other.nodeID, map[string]any{"anchor": true}, e.token))
	e.reconcile()
	if got := e.standbyOf(srv); got != sb {
		t.Fatalf("standby moved to %s", got)
	}
	e.clk.advance(testTimings.LeaseTTLMs + testTimings.OfflineAfterMs + 1000)
	for _, ag := range []*agent{b, c} {
		ag.heartbeatCaps(nil, javaCaps())
	}
	e.reconcile()
	if ex := e.activeExec(srv); ex == nil || ex["node"] != sb {
		t.Fatalf("recovery placed %+v, want the standby %s", ex, sb)
	}
}
