package api_test

import (
	"crypto/ed25519"
	"encoding/json"
	"testing"

	"github.com/grendalaget/varde/apps/controlplane/internal/api"
	"github.com/grendalaget/varde/apps/controlplane/internal/api/gen"
	"github.com/grendalaget/varde/go/gatewaytoken"
)

func tournamentCaps() map[string]any {
	c := caps()
	c["drivers"] = []string{"autotournament"}
	return c
}

func TestTournamentRequiresGatewayAndGroupPC(t *testing.T) {
	e := newEnv(t)
	body := map[string]any{"name": "Tournament", "game_id": "autotournament", "config": map[string]any{"licence": "noncommercial"}}
	r := e.do("POST", "/v1/groups/"+e.group+"/servers", body, e.token)
	if r.Status != 400 || r.Body["code"] != "gateway_unconfigured" {
		t.Fatalf("create without gateway status/code = %d/%v, want 400/gateway_unconfigured", r.Status, r.Body["code"])
	}
	e.srv.Cfg.Gateway = &api.GatewayConf{URL: "wss://gateway.example.org/v1/tunnel", Domain: "example.site"}
	anchor := e.newAgent("anchor")
	anchor.heartbeatCaps(nil, tournamentCaps())
	e.mustOK(e.do("PATCH", "/v1/nodes/"+anchor.nodeID, map[string]any{"anchor": true}, e.token))
	id := e.createServer("autotournament", "Tournament", map[string]any{"config": map[string]any{"licence": "noncommercial"}})
	r = e.do("POST", "/v1/servers/"+id+"/start", map[string]any{}, e.token)
	if r.Status != 400 || r.Body["code"] != "group_pc_required" {
		t.Errorf("start with only anchor status/code = %d/%v, want 400/group_pc_required", r.Status, r.Body["code"])
	}
	e.srv.Cfg.Gateway = nil
	r = e.do("POST", "/v1/servers/"+id+"/start", map[string]any{}, e.token)
	if r.Status != 400 || r.Body["code"] != "gateway_unconfigured" {
		t.Errorf("start without gateway status/code = %d/%v, want 400/gateway_unconfigured", r.Status, r.Body["code"])
	}
}

func TestTournamentGatewayFollowsLease(t *testing.T) {
	e := newEnv(t)
	e.srv.Cfg.Gateway = &api.GatewayConf{URL: "wss://gateway.example.org/v1/tunnel", Domain: "example.site"}
	a, b := e.newAgent("first"), e.newAgent("second")
	a.heartbeatCaps(nil, tournamentCaps())
	b.heartbeatCaps(nil, tournamentCaps())
	id := e.createServer("autotournament", "Tournament", map[string]any{"preferred_node_id": a.nodeID, "config": map[string]any{"licence": "noncommercial", "public_url": "https://wrong.example.org"}})
	before := e.mustOK(e.do("GET", "/v1/servers/"+id, nil, e.token))
	url := before["public_url"].(string)
	e.mustOK(e.do("POST", "/v1/servers/"+id+"/start", map[string]any{}, e.token))
	read := func(a *agent) gen.AgentDirectives {
		t.Helper()
		_, r := a.heartbeatCaps(nil, tournamentCaps())
		if r.Status != 200 {
			t.Fatalf("heartbeat status = %d, want 200", r.Status)
		}
		var d gen.AgentDirectives
		if err := json.Unmarshal(r.Raw, &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	find := func(d gen.AgentDirectives) *gen.ExecutionDirective {
		t.Helper()
		if d.Executions != nil {
			for _, ed := range *d.Executions {
				if ed.ServerId == id {
					return &ed
				}
			}
		}
		return nil
	}
	ed := find(read(a))
	if ed == nil || ed.Gateway == nil {
		t.Fatal("leaseholder gateway missing")
	}
	firstEpoch := ed.Epoch
	if (*ed.Config)["public_url"] != url {
		t.Errorf("directive public_url = %v, want %s", (*ed.Config)["public_url"], url)
	}
	if other := find(read(b)); other != nil && other.Gateway != nil {
		t.Error("non-leaseholder received gateway capability")
	}
	e.mustOK(a.execStatus(ed.ExecutionId, id, ed.Epoch, "running"))
	e.reconcile()
	e.clk.advance(testTimings.LeaseTTLMs + testTimings.OfflineAfterMs + 1000)
	read(b)
	e.reconcile()
	next := find(read(b))
	if next == nil || next.Gateway == nil {
		t.Fatal("failover leaseholder gateway missing")
	}
	if next.Epoch <= firstEpoch {
		t.Errorf("failover epoch = %d, want > %d", next.Epoch, firstEpoch)
	}
	claims, err := gatewaytoken.Verify(e.srv.RelayKey.Public().(ed25519.PublicKey), next.Gateway.Token, "example.site", e.clk.ms)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Epoch != uint64(next.Epoch) || claims.Label != before["gateway_label"] {
		t.Error("failover gateway claims do not match new lease and stable label")
	}
	if old := find(read(a)); old != nil && old.Gateway != nil {
		t.Error("old leaseholder retained gateway capability")
	}
	e.mustOK(e.do("PATCH", "/v1/servers/"+id, map[string]any{"name": "Renamed"}, e.token))
	after := e.mustOK(e.do("GET", "/v1/servers/"+id, nil, e.token))
	if after["public_url"] != url || after["gateway_label"] != before["gateway_label"] {
		t.Error("server rename or failover changed the public identity")
	}
}

func TestTournamentStandbyReceivesPublicConfig(t *testing.T) {
	e := newEnv(t)
	e.srv.Cfg.Gateway = &api.GatewayConf{URL: "wss://gateway.example.org/v1/tunnel", Domain: "example.site"}
	a, b := e.newAgent("host"), e.newAgent("standby")
	a.heartbeatCaps(nil, tournamentCaps())
	b.heartbeatCaps(nil, tournamentCaps())
	id := e.createServer("autotournament", "Tournament", map[string]any{"preferred_node_id": a.nodeID, "config": map[string]any{"licence": "noncommercial", "public_url": "https://wrong.example.org"}})
	e.startToRunning(id, a, b)
	e.reconcile()
	if e.standbyOf(id) != b.nodeID {
		t.Fatal("standby not assigned")
	}
	d, _ := b.heartbeatCaps(nil, tournamentCaps())
	view := e.mustOK(e.do("GET", "/v1/servers/"+id, nil, e.token))
	if len(d.Standby) != 1 || d.Standby[0].Config["public_url"] != view["public_url"] || d.Standby[0].Config["licence"] != "noncommercial" {
		t.Fatalf("standby config = %+v", d.Standby)
	}
}

func TestTournamentMoveRejectsAnchor(t *testing.T) {
	e := newEnv(t)
	e.srv.Cfg.Gateway = &api.GatewayConf{URL: "wss://gateway.example.org/v1/tunnel", Domain: "example.site"}
	a, b := e.newAgent("host"), e.newAgent("anchor")
	a.heartbeatCaps(nil, tournamentCaps())
	b.heartbeatCaps(nil, tournamentCaps())
	e.mustOK(e.do("PATCH", "/v1/nodes/"+b.nodeID, map[string]any{"anchor": true}, e.token))
	id := e.createServer("autotournament", "Tournament", map[string]any{"config": map[string]any{"licence": "noncommercial"}})
	e.startToRunning(id, a, b)
	response := e.do("POST", "/v1/servers/"+id+"/move", map[string]any{"target_node_id": b.nodeID}, e.token)
	if response.Status != 400 || response.Body["code"] != "group_pc_required" {
		t.Fatalf("anchor move = %d %s", response.Status, response.Raw)
	}
	if moves, err := e.st.OpenMigrations(t.Context()); err != nil || len(moves) != 0 {
		t.Fatalf("rejected move opened migrations: %+v %v", moves, err)
	}
	if active := e.activeExec(id); active == nil || active["node"] != a.nodeID {
		t.Fatal("move changed the serving execution")
	}
}

func TestTournamentMigrationRechecksAnchorEligibility(t *testing.T) {
	e := newEnv(t)
	e.srv.Cfg.Gateway = &api.GatewayConf{URL: "wss://gateway.example.org/v1/tunnel", Domain: "example.site"}
	a, b := e.newAgent("host"), e.newAgent("target")
	a.heartbeatCaps(nil, tournamentCaps())
	b.heartbeatCaps(nil, tournamentCaps())
	id := e.createServer("autotournament", "Tournament", map[string]any{"preferred_node_id": a.nodeID, "config": map[string]any{"licence": "noncommercial"}})
	_, execution, epoch := e.startToRunning(id, a, b)
	e.mustOK(e.do("POST", "/v1/servers/"+id+"/move", map[string]any{"target_node_id": b.nodeID}, e.token))
	e.mustOK(e.do("PATCH", "/v1/nodes/"+b.nodeID, map[string]any{"anchor": true}, e.token))
	srv, err := e.st.GetServer(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if r := a.createSnapshot(execution, id, *srv.DeploymentID, epoch, "final", "snap_at_move", "99"); r.Status != 201 {
		t.Fatalf("snapshot = %d %s", r.Status, r.Raw)
	}
	e.mustOK(a.execStatus(execution, id, epoch, "stopped"))
	e.reconcile()
	e.mustOK(b.replicaReady("snap_at_move"))
	e.reconcile()
	if active := e.activeExec(id); active != nil && active["node"] == b.nodeID {
		t.Fatal("migration assigned serving lease to anchor")
	}
	var state string
	if err := e.st.DB.Get(&state, `SELECT state FROM migrations WHERE server_id=?`, id); err != nil || state != "failed" {
		t.Fatalf("migration state = %q, error %v", state, err)
	}
}
