package api_test

import (
	"encoding/json"
	"testing"
)

type renewed struct {
	ServerTime int64 `json:"server_time_unix_ms"`
	Leases     []struct {
		ExecutionID string `json:"execution_id"`
		ExpiresAt   int64  `json:"lease_expires_at_unix_ms"`
	} `json:"leases"`
}

func (a *agent) renew(execID, serverID string, epoch int64) renewed {
	a.e.t.Helper()
	raw, _ := json.Marshal(map[string]any{"executions": []map[string]any{
		{"execution_id": execID, "server_id": serverID, "epoch": epoch},
	}})
	r := a.signedDo("POST", "/v1/agent/leases", raw, 0, nil)
	if r.Status != 200 {
		a.e.t.Fatalf("renew: %d %s", r.Status, r.Raw)
	}
	var out renewed
	_ = json.Unmarshal(r.Raw, &out)
	return out
}

func newFastEnv(t *testing.T) *env {
	e := newEnv(t)
	e.srv.Cfg.Timings.FastLeaseTTLMs = 8000
	e.srv.Cfg.Timings.LeaseRenewIntervalMs = 1000
	return e
}

func (e *env) newFastAgent(hostname string) *agent {
	a := e.newAgent(hostname)
	a.fast = true
	a.heartbeat(nil)
	return a
}

func TestFastLeaseOnlyForAgentsThatRenew(t *testing.T) {
	e := newFastEnv(t)
	old := e.newAgent("old")
	fast := e.newFastAgent("fast")
	d, _ := old.heartbeat(nil)
	if d.LeaseTtlMs != 20000 || d.LeaseRenewIntervalMs != nil {
		t.Fatalf("old agent: ttl=%d renew=%v, want 20000/nil", d.LeaseTtlMs, d.LeaseRenewIntervalMs)
	}
	d, _ = fast.heartbeat(nil)
	if d.LeaseTtlMs != 8000 || d.LeaseRenewIntervalMs == nil || *d.LeaseRenewIntervalMs != 1000 {
		t.Fatalf("fast agent: ttl=%d renew=%v, want 8000/1000", d.LeaseTtlMs, d.LeaseRenewIntervalMs)
	}
	// a classic lease already shorter than the fast one wins
	e.srv.Cfg.Timings.LeaseTTLMs = 4000
	if d, _ = fast.heartbeat(nil); d.LeaseTtlMs != 4000 {
		t.Fatalf("fast agent with 4 s classic lease: ttl=%d, want 4000", d.LeaseTtlMs)
	}
}

func TestFastLeaseRenewalKeepsServerAndSilenceLosesIt(t *testing.T) {
	e := newFastEnv(t)
	a := e.newFastAgent("host")
	srv := e.createServer("testgame", "s", nil)
	_, execID, epoch := e.startToRunning(srv, a)
	if _, r := a.heartbeat([]map[string]any{{"execution_id": execID, "server_id": srv, "epoch": epoch, "state": "running"}}); r.Status != 200 {
		t.Fatalf("heartbeat: %d", r.Status)
	}

	// renewals alone (no full heartbeat) carry it well past one 8 s lease
	for i := 0; i < 12; i++ {
		e.clk.advance(1000)
		got := a.renew(execID, srv, epoch)
		if len(got.Leases) != 1 || got.Leases[0].ExpiresAt != got.ServerTime+8000 {
			t.Fatalf("renew %d: %+v", i, got)
		}
		e.reconcile()
	}
	if e.observed(srv) != "running" || e.activeExecCount(srv) != 1 {
		t.Fatalf("observed=%s after renewals", e.observed(srv))
	}

	// stale epoch and other nodes are not renewed
	if got := a.renew(execID, srv, epoch+1); len(got.Leases) != 0 {
		t.Fatalf("stale epoch renewed: %+v", got)
	}
	other := e.newFastAgent("other")
	if got := other.renew(execID, srv, epoch); len(got.Leases) != 0 {
		t.Fatalf("foreign node renewed: %+v", got)
	}

	// silence: lost after 8 s, not the classic 20 s
	e.clk.advance(8001)
	e.reconcile()
	if got := e.observed(srv); got == "running" {
		t.Fatalf("still running 8 s after the last renewal")
	}
	// an expired lease can't be revived
	if got := a.renew(execID, srv, epoch); len(got.Leases) != 0 {
		t.Fatalf("expired lease renewed: %+v", got)
	}
}

func TestRecoverySkipsTheSilentLostHost(t *testing.T) {
	e := newFastEnv(t)
	a := e.newFastAgent("a")
	b := e.newFastAgent("b")
	srv := e.createServer("testgame", "s", nil)
	host, _, _ := e.startToRunning(srv, a, b)
	other := b
	if host == b {
		other = a
	}
	// make the dead host the scheduler's favourite
	if _, err := e.st.DB.Exec(e.st.Rebind(`UPDATE servers SET preferred_node_id=? WHERE id=?`), host.nodeID, srv); err != nil {
		t.Fatal(err)
	}
	// host dies; the other keeps heartbeating. 9 s later the host still
	// looks online (suspect after 15 s) but its lease is gone.
	for i := 0; i < 3; i++ {
		e.clk.advance(3000)
		other.heartbeat(nil)
	}
	// no standby (e.g. it just went ineligible): plain placement must still
	// skip the dead host
	if _, err := e.st.DB.Exec(e.st.Rebind(`UPDATE servers SET standby_node_id=NULL WHERE id=?`), srv); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	ex := e.activeExec(srv)
	if ex == nil {
		t.Fatal("no recovery execution")
	}
	if ex["node"] != other.nodeID {
		t.Fatalf("recovered onto %v, want the surviving node %s", ex["node"], other.nodeID)
	}
}
