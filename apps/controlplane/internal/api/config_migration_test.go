package api_test

import (
	"encoding/json"
	"testing"

	"github.com/grendalaget/varde/apps/controlplane/internal/api/gen"
)

func TestConfigRedeployWaitsForWarmMigration(t *testing.T) {
	e := newEnv(t)
	a, b := e.newAgent("source"), e.newAgent("target")
	javaCaps := caps()
	javaCaps["runtimes"] = []string{"native", "java"}
	a.heartbeatCaps(nil, javaCaps)
	b.heartbeatCaps(nil, javaCaps)
	id := e.createServer("minecraft", "Config migration", nil)
	source, execution, epoch := e.startToRunning(id, a, b)
	target := b
	if source == b {
		target = a
	}
	e.mustOK(e.do("POST", "/v1/servers/"+id+"/move", map[string]any{"target_node_id": target.nodeID}, e.token))
	edit := func(value string) {
		t.Helper()
		e.mustOK(e.do("PATCH", "/v1/servers/"+id, map[string]any{"config": map[string]any{"motd": value}}, e.token))
		e.reconcile()
		for _, host := range []*agent{source, target} {
			d, _ := host.heartbeat(nil)
			for _, ed := range d.Executions {
				if ed.Action != "run" {
					t.Fatalf("config edit interrupted migration: %+v", ed)
				}
			}
		}
	}
	edit("first")
	e.mustOK(source.createSnapshot(execution, id, "dep_x", epoch, "migration", "snap_config", "77"))
	e.reconcile()
	e.mustOK(target.replicaReady("snap_config"))
	e.reconcile()
	d, _ := target.heartbeat(nil)
	if len(d.Executions) != 1 {
		t.Fatalf("incoming directives: %+v", d.Executions)
	}
	incoming := d.Executions[0]
	edit("latest")
	var pending int
	if err := e.st.DB.Get(&pending, "SELECT COUNT(*) FROM server_config_redeployments WHERE server_id=?", id); err != nil || pending != 1 {
		t.Fatalf("durable coalesced redeploy count = %d: %v", pending, err)
	}
	e.mustOK(target.execStatus(incoming.ExecutionID, id, incoming.Epoch, "running"))
	e.reconcile()
	if active := e.activeExec(id); active == nil || active["id"] != incoming.ExecutionID {
		t.Fatalf("handoff did not complete: %+v", active)
	}
	d, _ = source.heartbeat(nil)
	if len(d.Executions) != 1 || d.Executions[0].StopReason != "handoff" {
		t.Fatalf("source lost its handoff stop: %+v", d.Executions)
	}
	e.mustOK(source.execStatus(execution, id, epoch, "stopped"))
	e.reconcile()
	e.reconcile()
	d, _ = target.heartbeat(nil)
	if len(d.Executions) != 1 || d.Executions[0].ExecutionID != incoming.ExecutionID || d.Executions[0].Action != "stop" || d.Executions[0].StopReason != "user" {
		t.Fatalf("settled target did not receive config redeploy: %+v", d.Executions)
	}
	e.mustOK(target.execStatus(incoming.ExecutionID, id, incoming.Epoch, "stopped"))
	a.heartbeatCaps(nil, javaCaps)
	b.heartbeatCaps(nil, javaCaps)
	e.reconcile()
	var replacement gen.ExecutionDirective
	for _, host := range []*agent{a, b} {
		_, response := host.heartbeat(nil)
		var directives gen.AgentDirectives
		if err := json.Unmarshal(response.Raw, &directives); err != nil {
			t.Fatal(err)
		}
		if directives.Executions != nil {
			for _, ed := range *directives.Executions {
				if ed.ExecutionId != execution && ed.ExecutionId != incoming.ExecutionID {
					replacement = ed
				}
			}
		}
	}
	if replacement.ExecutionId == "" || replacement.Config == nil || (*replacement.Config)["motd"] != "latest" {
		t.Fatalf("replacement did not receive the latest config: %+v", replacement)
	}
	if err := e.st.DB.Get(&pending, "SELECT COUNT(*) FROM server_config_redeployments WHERE server_id=?", id); err != nil || pending != 0 {
		t.Fatalf("redeploy request not consumed: %d: %v", pending, err)
	}
}
