package api_test

// Integration tests: httptest server + simulated agents (ed25519, signed
// requests) + fake clock + manually-driven reconciler passes.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/grendalaget/varde/apps/controlplane/internal/api"
	"github.com/grendalaget/varde/apps/controlplane/internal/auth"
	"github.com/grendalaget/varde/apps/controlplane/internal/reconciler"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
	"github.com/grendalaget/varde/go/identity"
)

// ---- fake clock ----

type fakeClock struct{ ms int64 }

func (c *fakeClock) Now() time.Time { return time.UnixMilli(c.ms) }
func (c *fakeClock) advance(ms int64) {
	c.ms += ms
}

// ---- environment ----

type env struct {
	t     *testing.T
	clk   *fakeClock
	st    *store.Store
	recon *reconciler.Reconciler
	http  *httptest.Server
	srv   *api.Server
	token string // operator session
	group string
}

var testTimings = reconciler.Timings{
	HeartbeatIntervalMs: 5000,
	LeaseTTLMs:          20000,
	StartGraceMs:        600000,
	SuspectAfterMs:      15000,
	OfflineAfterMs:      30000,
	UnschedulableRetry:  60000,
	TickInterval:        time.Second,
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := &fakeClock{ms: 1735689600000}
	st, err := store.Open("sqlite://"+t.TempDir()+"/cp.db", clk)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	recon := reconciler.New(st, testTimings, log)
	_, relayPriv, _ := ed25519.GenerateKey(rand.Reader)
	srv := &api.Server{
		Store: st,
		Auth:  &auth.Local{Store: st, Policy: auth.SignupOpen},
		Cfg:   api.Config{Timings: testTimings, Version: "test"},
		Recon: recon, RelayKey: relayPriv, Log: log,
	}
	e := &env{t: t, clk: clk, st: st, recon: recon, srv: srv}
	e.http = httptest.NewServer(api.NewHandler(srv, nil))
	t.Cleanup(func() { e.http.Close(); _ = st.Close() })
	e.token = e.signup("op@example.com", "password123")
	e.group = e.createGroup("g1")
	return e
}

// ---- HTTP helpers ----

type apiResp struct {
	Status int
	Body   map[string]any
	Raw    []byte
}

func (e *env) do(method, path string, body any, token string) apiResp {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.http.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	out := apiResp{Status: resp.StatusCode, Raw: raw}
	_ = json.Unmarshal(raw, &out.Body)
	return out
}

func (e *env) mustOK(r apiResp) map[string]any {
	e.t.Helper()
	if r.Status >= 300 {
		e.t.Fatalf("status %d: %s", r.Status, r.Raw)
	}
	return r.Body
}

func (e *env) signup(email, pw string) string {
	e.t.Helper()
	r := e.do("POST", "/v1/auth/signup",
		map[string]any{"email": email, "password": pw, "display_name": email}, "")
	b := e.mustOK(r)
	return b["token"].(string)
}

func (e *env) createGroup(name string) string {
	e.t.Helper()
	b := e.mustOK(e.do("POST", "/v1/groups", map[string]any{"name": name}, e.token))
	return b["id"].(string)
}

func (e *env) createServer(game, name string, extra map[string]any) string {
	e.t.Helper()
	body := map[string]any{"game_id": game, "name": name}
	for k, v := range extra {
		body[k] = v
	}
	b := e.mustOK(e.do("POST", "/v1/groups/"+e.group+"/servers", body, e.token))
	return b["id"].(string)
}

func crossplayConfig() map[string]any {
	return map[string]any{
		"server_name": "Varde Crossplay",
		"world_name":  "Crossplay",
		"password":    "hunter22",
		"crossplay":   true,
	}
}

func (e *env) enrollToken() string {
	e.t.Helper()
	b := e.mustOK(e.do("POST", "/v1/groups/"+e.group+"/enrollment-tokens",
		map[string]any{}, e.token))
	return b["token"].(string)
}

func (e *env) reconcile() {
	e.t.Helper()
	if err := e.recon.Pass(context.Background()); err != nil {
		e.t.Fatalf("reconcile pass: %v", err)
	}
}

func (e *env) activeExecCount(serverID string) int {
	e.t.Helper()
	var n int
	if err := e.st.DB.Get(&n, e.st.Rebind(
		`SELECT COUNT(*) FROM server_executions WHERE server_id=? AND ended_at IS NULL`), serverID); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) activeExec(serverID string) map[string]any {
	e.t.Helper()
	var ex struct {
		ID    string `db:"id"`
		Node  string `db:"node_id"`
		Epoch int64  `db:"epoch"`
		State string `db:"state"`
		Act   string `db:"action"`
	}
	if err := e.st.DB.Get(&ex, e.st.Rebind(
		`SELECT id,node_id,epoch,state,action FROM server_executions WHERE server_id=? AND ended_at IS NULL`), serverID); err != nil {
		return nil
	}
	return map[string]any{"id": ex.ID, "node": ex.Node, "epoch": ex.Epoch, "state": ex.State, "action": ex.Act}
}

// ---- simulated agent ----

type agent struct {
	e      *env
	priv   ed25519.PrivateKey
	pub    ed25519.PublicKey
	nodeID string
}

func (e *env) newAgent(hostname string) *agent {
	e.t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	a := &agent{e: e, priv: priv, pub: pub}
	// enroll via token (unsigned endpoint)
	r := e.do("POST", "/v1/agent/enroll/token", map[string]any{
		"token":      e.enrollToken(),
		"public_key": base64.StdEncoding.EncodeToString(pub),
		"hostname":   hostname, "os": "linux", "arch": "x86_64",
	}, "")
	b := e.mustOK(r)
	a.nodeID = b["node_id"].(string)
	// first heartbeat: node becomes online and schedulable
	a.heartbeat(nil)
	return a
}

// signedDo performs a signed agent API call. If tamper != nil it may modify
// the signature headers to simulate attack cases.
func (a *agent) signedDo(method, path string, body []byte, tsDelta int64, tamper func(h http.Header)) apiResp {
	ts := a.e.clk.ms + tsDelta
	sig := identity.SignRequest(a.priv, method, path, ts, body)
	req, _ := http.NewRequest(method, a.e.http.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Varde-Node", a.nodeID)
	req.Header.Set("X-Varde-Timestamp", fmt.Sprint(ts))
	req.Header.Set("X-Varde-Signature", sig)
	if tamper != nil {
		tamper(req.Header)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	out := apiResp{Status: resp.StatusCode, Raw: raw}
	_ = json.Unmarshal(raw, &out.Body)
	return out
}

type directives struct {
	Executions []struct {
		ExecutionID string `json:"execution_id"`
		ServerID    string `json:"server_id"`
		Epoch       int64  `json:"epoch"`
		Action      string `json:"action"`
		StopReason  string `json:"stop_reason"`
		Restore     *struct {
			SnapshotID    string   `json:"snapshot_id"`
			SourceNodeIDs []string `json:"source_node_ids"`
		} `json:"restore"`
	} `json:"executions"`
	Routes []struct {
		ServerID string `json:"server_id"`
	} `json:"routes"`
	ReplicationTasks []struct {
		SnapshotID string `json:"snapshot_id"`
		ServerID   string `json:"server_id"`
	} `json:"replication_tasks"`
	DeleteSnapshots []string `json:"delete_snapshots"`
}

func caps() map[string]any {
	return map[string]any{
		"drivers": []string{"testgame", "minecraft", "valheim"},
		"os":      "linux", "arch": "x86_64",
		"memory_available_mb": 8192, "disk_free_bytes": 1 << 40,
		"runtimes": []string{"native"},
	}
}

// heartbeat sends a heartbeat with optional execution reports; returns the
// parsed directives.
func (a *agent) heartbeat(execReports []map[string]any) (directives, apiResp) {
	return a.heartbeatCaps(execReports, caps())
}

// heartbeatCaps is heartbeat with a caller-supplied capabilities body.
func (a *agent) heartbeatCaps(execReports []map[string]any, c map[string]any) (directives, apiResp) {
	a.e.t.Helper()
	body := map[string]any{"capabilities": c, "executions": execReports}
	raw, _ := json.Marshal(body)
	r := a.signedDo("POST", "/v1/agent/heartbeat", raw, 0, nil)
	var d directives
	if r.Status == 200 {
		_ = json.Unmarshal(r.Raw, &d)
	}
	return d, r
}

func (a *agent) heartbeatWithDeletedSnapshots(snapshotIDs []string) (directives, apiResp) {
	a.e.t.Helper()
	body := map[string]any{
		"capabilities":      caps(),
		"executions":        []map[string]any{},
		"deleted_snapshots": snapshotIDs,
	}
	raw, _ := json.Marshal(body)
	r := a.signedDo("POST", "/v1/agent/heartbeat", raw, 0, nil)
	var d directives
	if r.Status == 200 {
		_ = json.Unmarshal(r.Raw, &d)
	}
	return d, r
}

func (a *agent) execStatus(execID, serverID string, epoch int64, state string) apiResp {
	body, _ := json.Marshal(map[string]any{
		"server_id": serverID, "epoch": epoch, "state": state,
	})
	return a.signedDo("POST", "/v1/agent/executions/"+execID+"/status", body, 0, nil)
}

func (a *agent) execStatusWithMessage(execID, serverID string, epoch int64, state, message string) apiResp {
	body, _ := json.Marshal(map[string]any{
		"server_id": serverID, "epoch": epoch, "state": state, "message": message,
	})
	return a.signedDo("POST", "/v1/agent/executions/"+execID+"/status", body, 0, nil)
}

func (a *agent) execStatusJoinCode(execID, serverID string, epoch int64, state, joinCode string) apiResp {
	body, _ := json.Marshal(map[string]any{
		"server_id": serverID, "epoch": epoch, "state": state, "join_code": joinCode,
	})
	return a.signedDo("POST", "/v1/agent/executions/"+execID+"/status", body, 0, nil)
}

func (a *agent) createSnapshot(execID, serverID, depID string, epoch int64, reason, snapID, digest string) apiResp {
	body, _ := json.Marshal(map[string]any{
		"snapshot_id": snapID, "server_id": serverID, "execution_id": execID,
		"deployment_id": depID, "epoch": epoch, "reason": reason,
		"manifest_digest": digest, "size_bytes": 1024, "stored_bytes": 512,
		"file_count": 4, "chunk_count": 2,
	})
	return a.signedDo("POST", "/v1/agent/snapshots", body, 0, nil)
}

func (a *agent) replicaReady(snapID string) apiResp {
	body, _ := json.Marshal(map[string]any{"state": "ready"})
	return a.signedDo("POST", "/v1/agent/snapshots/"+snapID+"/replicas", body, 0, nil)
}

func (e *env) serverEpoch(serverID string) int64 {
	var ep int64
	if err := e.st.DB.Get(&ep, e.st.Rebind(`SELECT epoch FROM servers WHERE id=?`), serverID); err != nil {
		e.t.Fatal(err)
	}
	return ep
}

func (e *env) observed(serverID string) string {
	var s string
	if err := e.st.DB.Get(&s, e.st.Rebind(`SELECT observed_state FROM servers WHERE id=?`), serverID); err != nil {
		e.t.Fatal(err)
	}
	return s
}

// startToRunning starts a server, finds the agent the scheduler picked, and
// brings the execution to running. Returns host agent, exec id, epoch.
func (e *env) startToRunning(srvID string, agents ...*agent) (*agent, string, int64) {
	e.t.Helper()
	r := e.do("POST", "/v1/servers/"+srvID+"/start", map[string]any{}, e.token)
	e.mustOK(r)
	ex := e.activeExec(srvID)
	if ex == nil {
		e.t.Fatal("no active execution after start")
	}
	execID := ex["id"].(string)
	epoch := ex["epoch"].(int64)
	var host *agent
	for _, a := range agents {
		if a.nodeID == ex["node"] {
			host = a
		}
	}
	if host == nil {
		e.t.Fatalf("exec on unknown node %s", ex["node"])
	}
	d, _ := host.heartbeat(nil)
	var found bool
	for _, ed := range d.Executions {
		if ed.ExecutionID == execID {
			found = true
		}
	}
	if !found {
		e.t.Fatalf("expected run directive for %s, got %+v", execID, d.Executions)
	}
	if r := host.execStatus(execID, srvID, epoch, "running"); r.Status != 204 {
		e.t.Fatalf("status running: %d %s", r.Status, r.Raw)
	}
	e.reconcile()
	if got := e.observed(srvID); got != "running" {
		e.t.Fatalf("observed=%s", got)
	}
	return host, execID, epoch
}

func TestDrainingNodeSkipped(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("nodeA")
	b := e.newAgent("nodeB")
	srv := e.createServer("testgame", "s1", map[string]any{"preferred_node_id": a.nodeID})
	host, execID, epoch := e.startToRunning(srv, a, b)
	if host != a {
		t.Fatalf("preferred node a did not host (on %s)", host.nodeID)
	}

	// A begins draining: heartbeat advertises draining, exec reports stopped.
	dc := caps()
	dc["draining"] = true
	_, _ = a.heartbeatCaps(nil, dc)
	if r := a.execStatus(execID, srv, epoch, "stopped"); r.Status != 204 {
		t.Fatalf("stopped: %d %s", r.Status, r.Raw)
	}
	e.reconcile()

	// the next execution must land on B, not on still-online-but-draining A
	ex := e.activeExec(srv)
	if ex == nil {
		t.Fatal("no re-activated execution")
	}
	if ex["node"] == a.nodeID {
		t.Fatalf("re-placed on draining node A: %+v", ex)
	}
	if ex["node"] != b.nodeID {
		t.Fatalf("expected B, got %+v", ex)
	}
	d, _ := b.heartbeat(nil)
	found := false
	for _, ed := range d.Executions {
		if ed.ExecutionID == ex["id"] {
			found = true
		}
	}
	if !found {
		t.Fatalf("B has no run directive: %+v", d.Executions)
	}
}

// ===================== tests =====================

func TestCrossplayServersAreExcludedFromDirectiveRoutes(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("nodeA")
	crossplay := e.createServer("valheim", "crossplay", map[string]any{
		"config": crossplayConfig(),
	})
	regular := e.createServer("testgame", "regular", map[string]any{
		"config": map[string]any{"crossplay": true},
	})

	d, r := a.heartbeat(nil)
	if r.Status != 200 {
		t.Fatalf("heartbeat: %d %s", r.Status, r.Raw)
	}
	routes := map[string]bool{}
	for _, route := range d.Routes {
		routes[route.ServerID] = true
	}
	if routes[crossplay] {
		t.Fatalf("crossplay server %s has a directive route: %+v", crossplay, d.Routes)
	}
	if !routes[regular] {
		t.Fatalf("regular server %s is missing its directive route: %+v", regular, d.Routes)
	}
	body := e.mustOK(e.do("GET", "/v1/servers/"+regular, nil, e.token))
	summary := body["summary"].(map[string]any)
	if address, ok := summary["address"].(string); !ok || address == "" {
		t.Fatalf("non-Crossplay game with crossplay:true has no address: %+v", summary)
	}
	if _, ok := summary["join_code"]; ok {
		t.Fatalf("non-Crossplay game with crossplay:true exposes a join code: %+v", summary)
	}
}

func TestCrossplayJoinCodeSummaryRequiresRunningExecution(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("nodeA")
	b := e.newAgent("nodeB")
	for _, node := range []*agent{a, b} {
		capabilities := caps()
		capabilities["runtimes"] = []string{"native", "steamcmd"}
		if _, r := node.heartbeatCaps(nil, capabilities); r.Status != 200 {
			t.Fatalf("heartbeat with Valheim runtime: %d %s", r.Status, r.Raw)
		}
	}
	srv := e.createServer("valheim", "crossplay", map[string]any{
		"config": crossplayConfig(),
	})
	host, execID, epoch := e.startToRunning(srv, a, b)
	if r := host.execStatusJoinCode(execID, srv, epoch, "running", "123456"); r.Status != 204 {
		t.Fatalf("join-code report: %d %s", r.Status, r.Raw)
	}

	r := e.do("GET", "/v1/servers/"+srv, nil, e.token)
	body := e.mustOK(r)
	summary := body["summary"].(map[string]any)
	if got := summary["join_code"]; got != "123456" {
		t.Fatalf("summary join_code=%v, want 123456", got)
	}
	if _, ok := summary["address"]; ok {
		t.Fatalf("crossplay summary exposes an address: %+v", summary)
	}
	var eventCount int
	if err := e.st.DB.Get(&eventCount, e.st.Rebind(
		`SELECT COUNT(*) FROM events WHERE server_id=? AND type='server.join_code'`), srv); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("join-code event count=%d, want 1", eventCount)
	}

	if r := host.execStatusJoinCode(execID, srv, epoch, "starting", "123456"); r.Status != 204 {
		t.Fatalf("starting report: %d %s", r.Status, r.Raw)
	}
	body = e.mustOK(e.do("GET", "/v1/servers/"+srv, nil, e.token))
	summary = body["summary"].(map[string]any)
	if _, ok := summary["join_code"]; ok {
		t.Fatalf("non-running execution exposes a join code: %+v", summary)
	}
	if err := e.st.DB.Get(&eventCount, e.st.Rebind(
		`SELECT COUNT(*) FROM events WHERE server_id=? AND type='server.join_code'`), srv); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("unchanged join code emitted %d events, want 1", eventCount)
	}

	if r := host.execStatusJoinCode(execID, srv, epoch, "running", ""); r.Status != 204 {
		t.Fatalf("empty join-code report: %d %s", r.Status, r.Raw)
	}
	body = e.mustOK(e.do("GET", "/v1/servers/"+srv, nil, e.token))
	summary = body["summary"].(map[string]any)
	if _, ok := summary["join_code"]; ok {
		t.Fatalf("cleared join code remains in summary: %+v", summary)
	}
	if err := e.st.DB.Get(&eventCount, e.st.Rebind(
		`SELECT COUNT(*) FROM events WHERE server_id=? AND type='server.join_code'`), srv); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("clearing join code emitted an event, count=%d", eventCount)
	}

	fresh := e.createServer("valheim", "empty-crossplay", map[string]any{
		"config": crossplayConfig(),
	})
	freshHost, freshExecID, freshEpoch := e.startToRunning(fresh, a, b)
	if r := freshHost.execStatusJoinCode(freshExecID, fresh, freshEpoch, "running", ""); r.Status != 204 {
		t.Fatalf("empty join-code report for fresh execution: %d %s", r.Status, r.Raw)
	}
	var rowCount int
	if err := e.st.DB.Get(&rowCount, e.st.Rebind(
		`SELECT COUNT(*) FROM execution_info WHERE execution_id=?`), freshExecID); err != nil {
		t.Fatal(err)
	}
	if rowCount != 0 {
		t.Fatalf("empty join code created %d execution_info rows, want 0", rowCount)
	}
	if err := e.st.DB.Get(&eventCount, e.st.Rebind(
		`SELECT COUNT(*) FROM events WHERE server_id=? AND type='server.join_code'`), fresh); err != nil {
		t.Fatal(err)
	}
	if eventCount != 0 {
		t.Fatalf("empty join code emitted %d events for a fresh execution", eventCount)
	}
}

func TestCrossplayModeCannotChangeWhileRunning(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("nodeA")
	b := e.newAgent("nodeB")
	for _, node := range []*agent{a, b} {
		capabilities := caps()
		capabilities["runtimes"] = []string{"native", "steamcmd"}
		if _, r := node.heartbeatCaps(nil, capabilities); r.Status != 200 {
			t.Fatalf("heartbeat with Valheim runtime: %d %s", r.Status, r.Raw)
		}
	}

	disabled := crossplayConfig()
	disabled["crossplay"] = false
	running := e.createServer("valheim", "running", map[string]any{
		"config": disabled,
	})
	e.startToRunning(running, a, b)
	r := e.do("PATCH", "/v1/servers/"+running, map[string]any{
		"config": crossplayConfig(),
	}, e.token)
	if r.Status != http.StatusConflict {
		t.Fatalf("crossplay toggle while running: %d %s, want 409", r.Status, r.Raw)
	}

	stopped := e.createServer("valheim", "stopped", map[string]any{
		"config": disabled,
	})
	r = e.do("PATCH", "/v1/servers/"+stopped, map[string]any{
		"config": crossplayConfig(),
	}, e.token)
	if r.Status != http.StatusOK {
		t.Fatalf("crossplay toggle while stopped: %d %s, want 200", r.Status, r.Raw)
	}
}

func TestEnrollTokenAndHeartbeat(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("laptop")
	d, r := a.heartbeat(nil)
	if r.Status != 200 {
		t.Fatalf("heartbeat %d %s", r.Status, r.Raw)
	}
	if d.Executions == nil {
		t.Fatal("expected executions array")
	}
	// node is listed
	b := e.mustOK(e.do("GET", "/v1/groups/"+e.group+"/nodes", nil, e.token))
	nodes := b["nodes"].([]any)
	if len(nodes) != 1 {
		t.Fatalf("nodes: %v", b)
	}
}

func TestEnrollDeviceFlow(t *testing.T) {
	e := newEnv(t)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	r := e.do("POST", "/v1/agent/enroll/device", map[string]any{
		"public_key": base64.StdEncoding.EncodeToString(pub),
		"hostname":   "desktop", "os": "linux", "arch": "x86_64",
	}, "")
	b := e.mustOK(r)
	userCode, devCode := b["user_code"].(string), b["device_code"].(string)
	if userCode == "" || devCode == "" {
		t.Fatal("missing codes")
	}
	// poll before approval → 202
	p := e.do("POST", "/v1/agent/enroll/device/poll", map[string]any{"device_code": devCode}, "")
	if p.Status != 202 {
		t.Fatalf("poll pre-approval: %d", p.Status)
	}
	// user approves
	e.mustOK(e.do("POST", "/v1/device-links/approve",
		map[string]any{"user_code": userCode, "group_id": e.group, "name": "desktop"}, e.token))
	// poll → 200 with node_id
	p = e.do("POST", "/v1/agent/enroll/device/poll", map[string]any{"device_code": devCode}, "")
	eb := e.mustOK(p)
	if eb["node_id"] == "" {
		t.Fatal("no node_id")
	}
}

func TestBadSignatureRejected(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("n1")
	raw := []byte(`{"executions":[]}`)
	// wrong signature
	if r := a.signedDo("POST", "/v1/agent/heartbeat", raw, 0, func(h http.Header) {
		h.Set("X-Varde-Signature", base64.StdEncoding.EncodeToString(make([]byte, 64)))
	}); r.Status != 401 {
		t.Fatalf("bad sig: %d", r.Status)
	}
	// stale timestamp
	if r := a.signedDo("POST", "/v1/agent/heartbeat", raw, -120000, nil); r.Status != 401 {
		t.Fatalf("skew: %d", r.Status)
	}
	// disabled node → 403
	if _, err := e.st.DB.Exec(e.st.Rebind(`UPDATE nodes SET admin_state='disabled' WHERE id=?`), a.nodeID); err != nil {
		t.Fatal(err)
	}
	if r := a.signedDo("POST", "/v1/agent/heartbeat", raw, 0, nil); r.Status != 403 {
		t.Fatalf("disabled: %d", r.Status)
	}
}

func TestStartRunStopLifecycle(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("n1")
	srv := e.createServer("testgame", "s1", nil)
	_, execID, epoch := e.startToRunning(srv, a)

	// stop
	e.mustOK(e.do("POST", "/v1/servers/"+srv+"/stop", nil, e.token))
	e.reconcile()
	d, _ := a.heartbeat(nil)
	if len(d.Executions) != 1 || d.Executions[0].Action != "stop" {
		t.Fatalf("expected stop directive: %+v", d.Executions)
	}
	if r := a.execStatus(execID, srv, epoch, "stopped"); r.Status != 204 {
		t.Fatalf("stopped: %d %s", r.Status, r.Raw)
	}
	e.reconcile()
	if e.activeExecCount(srv) != 0 {
		t.Fatal("execution not ended")
	}
	if e.observed(srv) != "stopped" {
		t.Fatalf("observed=%s", e.observed(srv))
	}
}

func TestSplitBrain(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("nodeA")
	b := e.newAgent("nodeB")
	srv := e.createServer("testgame", "s1", nil)
	host, execA, epochA := e.startToRunning(srv, a, b)
	other := a
	if host == a {
		other = b
	}
	if got := e.serverEpoch(srv); got != 1 {
		t.Fatalf("epoch=%d", got)
	}

	// host stops heartbeating; advance past lease expiry; other still heartbeats.
	e.clk.advance(testTimings.LeaseTTLMs + testTimings.OfflineAfterMs + 5000)
	other.heartbeat(nil)
	e.reconcile()
	other.heartbeat(nil)

	ex := e.activeExec(srv)
	if ex == nil {
		t.Fatal("no new active execution")
	}
	if ex["id"] == execA || ex["node"] == host.nodeID {
		t.Fatalf("expected new exec not on old host, got %+v", ex)
	}
	epochB := ex["epoch"].(int64)
	if epochB != epochA+1 {
		t.Fatalf("epochs %d → %d", epochA, epochB)
	}
	// exactly one active execution
	if e.activeExecCount(srv) != 1 {
		t.Fatal("two active executions!")
	}

	// new host reports running on the new epoch.
	execB := ex["id"].(string)
	if r := other.execStatus(execB, srv, epochB, "running"); r.Status != 204 {
		t.Fatalf("new host running: %d %s", r.Status, r.Raw)
	}

	// old host comes back: stale writes must 409.
	if r := host.execStatus(execA, srv, epochA, "running"); r.Status != 409 {
		t.Fatalf("stale status: %d %s", r.Status, r.Raw)
	}
	if r := host.createSnapshot(execA, srv, "dep_x", epochA, "scheduled", "snap_stale", "deadbeef"); r.Status != 409 {
		t.Fatalf("stale snapshot: %d %s", r.Status, r.Raw)
	}
	// old host's directives must not include its old execution.
	d, r := host.heartbeat([]map[string]any{
		{"execution_id": execA, "server_id": srv, "epoch": epochA, "state": "running"},
	})
	if r.Status != 200 {
		t.Fatalf("A heartbeat: %d", r.Status)
	}
	for _, ex := range d.Executions {
		if ex.ExecutionID == execA {
			t.Fatal("stale execution still in A directives")
		}
	}
	if e.activeExecCount(srv) != 1 {
		t.Fatal("two active executions after A return")
	}
}

func TestRecoveryCommittedOnly(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("nodeA")
	b := e.newAgent("nodeB")
	srv := e.createServer("testgame", "s1", nil)
	host, execA, epA := e.startToRunning(srv, a, b)
	other := a
	if host == a {
		other = b
	}

	// committed snapshot + uncommitted (local) newer snapshot on host.
	if r := host.createSnapshot(execA, srv, "dep_x", epA, "scheduled", "snap_old", "aa"); r.Status != 201 {
		t.Fatalf("snap1 %d %s", r.Status, r.Raw)
	}
	// commit it: mark committed directly (replica on other made ready below)
	if _, err := e.st.DB.Exec(e.st.Rebind(
		`UPDATE snapshots SET state='committed', committed_at=? WHERE id='snap_old'`), e.clk.ms); err != nil {
		t.Fatal(err)
	}
	if r := host.createSnapshot(execA, srv, "dep_x", epA, "scheduled", "snap_new", "bb"); r.Status != 201 {
		t.Fatalf("snap2 %d %s", r.Status, r.Raw)
	}
	// other has a ready replica of snap_old so it's restoreable there.
	if r := other.replicaReady("snap_old"); r.Status != 204 {
		t.Fatalf("replica ready: %d %s", r.Status, r.Raw)
	}

	// host dies → lease expiry → recovery must pick snap_old (committed), not snap_new.
	e.clk.advance(testTimings.LeaseTTLMs + testTimings.OfflineAfterMs + 1000)
	other.heartbeat(nil)
	e.reconcile()
	ex := e.activeExec(srv)
	if ex == nil || ex["node"] != other.nodeID {
		t.Fatalf("no recovery exec on other node: %+v", ex)
	}
	d, _ := other.heartbeat(nil)
	var found *struct {
		SnapshotID    string   `json:"snapshot_id"`
		SourceNodeIDs []string `json:"source_node_ids"`
	}
	for _, ex := range d.Executions {
		if ex.ServerID == srv {
			found = ex.Restore
		}
	}
	if found == nil || found.SnapshotID != "snap_old" {
		t.Fatalf("recovery restore: %+v", found)
	}
}

func TestLatestSaveUnavailableAndAllowOlder(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("nodeA")
	b := e.newAgent("nodeB")
	srv := e.createServer("testgame", "s1", nil)
	host, execA, epA := e.startToRunning(srv, a, b)
	other := b
	if host == b {
		other = a
	}
	// snapshot on host only, then normal stop → start
	if r := host.createSnapshot(execA, srv, "dep_x", epA, "final", "snap_solo", "cc"); r.Status != 201 {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	// delete the auto-assigned replica rows so snap_solo's only copy is on host
	if _, err := e.st.DB.Exec(e.st.Rebind(
		`DELETE FROM snapshot_replicas WHERE snapshot_id='snap_solo' AND node_id!=?`), host.nodeID); err != nil {
		t.Fatal(err)
	}
	e.mustOK(e.do("POST", "/v1/servers/"+srv+"/stop", nil, e.token))
	e.reconcile()
	if r := host.execStatus(execA, srv, epA, "stopped"); r.Status != 204 {
		t.Fatalf("stopped %d %s", r.Status, r.Raw)
	}
	e.reconcile()

	// host offline: newest save's only replica is unreachable →
	// start must 409 latest_save_unavailable
	e.clk.advance(testTimings.OfflineAfterMs + 5000)
	other.heartbeat(nil)
	e.reconcile()
	r := e.do("POST", "/v1/servers/"+srv+"/start", map[string]any{}, e.token)
	if r.Status != 409 || r.Body["code"] != "latest_save_unavailable" {
		t.Fatalf("want 409 latest_save_unavailable, got %d %s", r.Status, r.Raw)
	}
	// allow_older_snapshot proceeds (restore from an older/nonexistent snap →
	// clean start is allowed per spec only when no reachable save).
	r = e.do("POST", "/v1/servers/"+srv+"/start",
		map[string]any{"allow_older_snapshot": true}, e.token)
	e.mustOK(r)
}

func TestRetentionDeleteAck(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("nodeA")
	settings := store.DefaultGroupSettings()
	settings.SnapshotRetention = 1
	settingsJSON, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.DB.Exec(e.st.Rebind(
		`UPDATE groups SET settings_json=? WHERE id=?`), string(settingsJSON), e.group); err != nil {
		t.Fatal(err)
	}

	srv := e.createServer("testgame", "retention", map[string]any{
		"preferred_node_id":   a.nodeID,
		"replication_factor":  1,
		"min_commit_replicas": 1,
	})
	host, execID, epoch := e.startToRunning(srv, a)
	snapshotIDs := []string{"snap_old_a", "snap_old_b", "snap_latest"}
	for i, snapshotID := range snapshotIDs {
		if r := host.createSnapshot(execID, srv, "dep_x", epoch, "scheduled", snapshotID, fmt.Sprintf("d%d", i)); r.Status != 201 {
			t.Fatalf("create %s: %d %s", snapshotID, r.Status, r.Raw)
		}
	}
	server, err := e.st.GetServer(context.Background(), srv)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.srv.ApplyRetention(context.Background(), server); err != nil {
		t.Fatal(err)
	}

	d, r := host.heartbeat(nil)
	if r.Status != 200 {
		t.Fatalf("heartbeat: %d %s", r.Status, r.Raw)
	}
	deleting := map[string]bool{}
	for _, id := range d.DeleteSnapshots {
		deleting[id] = true
	}
	for _, id := range snapshotIDs[:2] {
		if !deleting[id] {
			t.Fatalf("retention directive missing %s: %+v", id, d.DeleteSnapshots)
		}
	}

	next, r := host.heartbeatWithDeletedSnapshots(d.DeleteSnapshots)
	if r.Status != 200 {
		t.Fatalf("delete acknowledgement heartbeat: %d %s", r.Status, r.Raw)
	}
	if len(next.DeleteSnapshots) != 0 {
		t.Fatalf("delete list after acknowledgement: %+v", next.DeleteSnapshots)
	}
	for _, id := range snapshotIDs[:2] {
		var count int
		if err := e.st.DB.Get(&count, e.st.Rebind(
			`SELECT COUNT(*) FROM snapshots WHERE id=?`), id); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("snapshot %s remains after final replica deletion", id)
		}
		if err := e.st.DB.Get(&count, e.st.Rebind(
			`SELECT COUNT(*) FROM snapshot_replicas WHERE snapshot_id=?`), id); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("replicas remain for %s after acknowledgement", id)
		}
	}

	afterReadyAck, r := host.heartbeatWithDeletedSnapshots([]string{"snap_latest"})
	if r.Status != 200 {
		t.Fatalf("ready-replica acknowledgement heartbeat: %d %s", r.Status, r.Raw)
	}
	if len(afterReadyAck.DeleteSnapshots) != 0 {
		t.Fatalf("ready-replica ack produced delete directives: %+v", afterReadyAck.DeleteSnapshots)
	}
	var state string
	if err := e.st.DB.Get(&state, e.st.Rebind(
		`SELECT state FROM snapshot_replicas WHERE snapshot_id='snap_latest' AND node_id=?`), host.nodeID); err != nil {
		t.Fatal(err)
	}
	if state != "ready" {
		t.Fatalf("ready replica state after ignored acknowledgement=%q", state)
	}
	if err := e.st.DB.Get(&state, e.st.Rebind(
		`SELECT state FROM snapshots WHERE id='snap_latest'`)); err != nil {
		t.Fatal(err)
	}
	if state != "committed" {
		t.Fatalf("snapshot state after ignored acknowledgement=%q", state)
	}
}

// A newer save that lives only on an offline machine must block a normal
// start even when an older save is reachable — otherwise /start silently
// restores the older save. allow_older_snapshot and automatic recovery both
// fall back to the older committed save.
func TestOlderReachableSaveDoesNotBypassLatestBlocker(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("nodeA")
	b := e.newAgent("nodeB")
	srv := e.createServer("testgame", "s1", nil)
	host, execA, epA := e.startToRunning(srv, a, b)
	other := a
	if host == a {
		other = b
	}

	// snap_old: committed, reachable on other. snap_new: committed too, but
	// its only replica lives on host.
	if r := host.createSnapshot(execA, srv, "dep_x", epA, "scheduled", "snap_old", "aa"); r.Status != 201 {
		t.Fatalf("snap_old %d %s", r.Status, r.Raw)
	}
	if r := host.createSnapshot(execA, srv, "dep_x", epA, "scheduled", "snap_new", "bb"); r.Status != 201 {
		t.Fatalf("snap_new %d %s", r.Status, r.Raw)
	}
	if _, err := e.st.DB.Exec(e.st.Rebind(
		`UPDATE snapshots SET state='committed', committed_at=? WHERE id IN ('snap_old','snap_new')`), e.clk.ms); err != nil {
		t.Fatal(err)
	}
	// restrict each snapshot's replicas to the intended node
	if _, err := e.st.DB.Exec(e.st.Rebind(
		`DELETE FROM snapshot_replicas WHERE snapshot_id='snap_old' AND node_id!=?`), other.nodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.DB.Exec(e.st.Rebind(
		`DELETE FROM snapshot_replicas WHERE snapshot_id='snap_new' AND node_id!=?`), host.nodeID); err != nil {
		t.Fatal(err)
	}
	if r := other.replicaReady("snap_old"); r.Status != 204 {
		t.Fatalf("replica ready: %d %s", r.Status, r.Raw)
	}

	e.mustOK(e.do("POST", "/v1/servers/"+srv+"/stop", nil, e.token))
	e.reconcile()
	if r := host.execStatus(execA, srv, epA, "stopped"); r.Status != 204 {
		t.Fatalf("stopped %d %s", r.Status, r.Raw)
	}
	e.reconcile()

	// host offline: snap_new unreachable, snap_old reachable on other.
	e.clk.advance(testTimings.OfflineAfterMs + 5000)
	other.heartbeat(nil)
	e.reconcile()

	r := e.do("POST", "/v1/servers/"+srv+"/start", map[string]any{}, e.token)
	if r.Status != 409 || r.Body["code"] != "latest_save_unavailable" {
		t.Fatalf("want 409 latest_save_unavailable, got %d %s", r.Status, r.Raw)
	}
	if e.activeExec(srv) != nil {
		t.Fatal("a blocked start must not create an execution")
	}

	// allow_older_snapshot: restores snap_old, not snap_new.
	r = e.do("POST", "/v1/servers/"+srv+"/start",
		map[string]any{"allow_older_snapshot": true}, e.token)
	e.mustOK(r)
	e.reconcile()
	ex := e.activeExec(srv)
	if ex == nil {
		t.Fatal("allow_older start produced no execution")
	}
	d, _ := other.heartbeat(nil)
	var found *struct {
		SnapshotID    string   `json:"snapshot_id"`
		SourceNodeIDs []string `json:"source_node_ids"`
	}
	for _, ex := range d.Executions {
		if ex.ServerID == srv {
			found = ex.Restore
		}
	}
	if found == nil || found.SnapshotID != "snap_old" {
		t.Fatalf("allow_older restore: %+v", found)
	}

	// the exec dies → automatic re-placement is recovery and must also use
	// snap_old (newest committed save reachable), never 409
	execB := ex["id"].(string)
	epochB := ex["epoch"].(int64)
	if r := other.execStatus(execB, srv, epochB, "stopped"); r.Status != 204 {
		t.Fatalf("exec stop %d %s", r.Status, r.Raw)
	}
	e.reconcile()
	ex = e.activeExec(srv)
	if ex == nil {
		t.Fatal("recovery produced no execution")
	}
	d, _ = other.heartbeat(nil)
	found = nil
	for _, ex := range d.Executions {
		if ex.ServerID == srv {
			found = ex.Restore
		}
	}
	if found == nil || found.SnapshotID != "snap_old" {
		t.Fatalf("recovery restore: %+v", found)
	}
}

func TestRecoveryWaitsForOfflineOnlySave(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("nodeA")
	b := e.newAgent("nodeB")
	srv := e.createServer("testgame", "s1", nil)
	host, execID, epoch := e.startToRunning(srv, a, b)
	other := a
	if host == a {
		other = b
	}

	const snapshotID = "snap_recovery"
	if r := host.createSnapshot(execID, srv, "dep_x", epoch, "scheduled", snapshotID, "cc"); r.Status != 201 {
		t.Fatalf("snapshot %d %s", r.Status, r.Raw)
	}
	if _, err := e.st.DB.Exec(e.st.Rebind(
		`UPDATE snapshots SET state='committed', committed_at=? WHERE id=?`), e.clk.ms, snapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.DB.Exec(e.st.Rebind(
		`DELETE FROM snapshot_replicas WHERE snapshot_id=? AND node_id!=?`), snapshotID, host.nodeID); err != nil {
		t.Fatal(err)
	}

	e.clk.advance(testTimings.LeaseTTLMs + testTimings.OfflineAfterMs + 1000)
	other.heartbeat(nil)
	e.reconcile()
	if e.activeExec(srv) != nil {
		t.Fatal("recovery created an execution while the only committed save was offline")
	}
	var observed string
	if err := e.st.DB.Get(&observed, e.st.Rebind(
		`SELECT observed_state FROM servers WHERE id=?`), srv); err != nil {
		t.Fatal(err)
	}
	if observed != "recovering" {
		t.Fatalf("observed state = %q, want recovering", observed)
	}
	var eventData string
	if err := e.st.DB.Get(&eventData, e.st.Rebind(
		`SELECT data_json FROM events WHERE server_id=? AND type='server.save_unavailable' ORDER BY id DESC LIMIT 1`), srv); err != nil {
		t.Fatalf("save unavailable event: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(eventData), &data); err != nil {
		t.Fatalf("decode save unavailable event: %v", err)
	}
	if data["snapshot_id"] != snapshotID {
		t.Fatalf("save unavailable event snapshot_id = %v, want %s", data["snapshot_id"], snapshotID)
	}
	nodes, ok := data["nodes"].([]any)
	if !ok || len(nodes) != 1 || nodes[0] != host.nodeID {
		t.Fatalf("save unavailable event nodes = %v, want [%s]", data["nodes"], host.nodeID)
	}

	host.heartbeat(nil)
	e.reconcile()
	ex := e.activeExec(srv)
	if ex == nil {
		t.Fatal("recovery did not create an execution after the save host returned")
	}
	target := host
	switch ex["node"] {
	case host.nodeID:
	case other.nodeID:
		target = other
	default:
		t.Fatalf("recovery placed on unknown node %v", ex["node"])
	}
	d, _ := target.heartbeat(nil)
	var found *struct {
		SnapshotID    string   `json:"snapshot_id"`
		SourceNodeIDs []string `json:"source_node_ids"`
	}
	for _, execution := range d.Executions {
		if execution.ServerID == srv {
			found = execution.Restore
		}
	}
	if found == nil || found.SnapshotID != snapshotID {
		t.Fatalf("recovery restore: %+v, want %s", found, snapshotID)
	}
}

func TestCommitPolicyMinAndAnchor(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("nodeA")
	b := e.newAgent("nodeB")
	c := e.newAgent("nodeC")
	// group requires anchor replica
	e.mustOK(e.do("PATCH", "/v1/groups/"+e.group, map[string]any{
		"settings": map[string]any{"require_anchor_for_commit": true},
	}, e.token))

	srv := e.createServer("testgame", "s1", map[string]any{
		"replication_factor": 3, "min_commit_replicas": 2,
	})
	hostAgent, execID, epA := e.startToRunning(srv, a, b, c)
	// anchor = first non-host agent; nonAnchor host=producer's replica is
	// already ready so an anchor that hosts wouldn't prove anything
	var anchorA, nonHost *agent
	for _, ag := range []*agent{a, b, c} {
		if ag == hostAgent {
			continue
		}
		if anchorA == nil {
			anchorA = ag
		} else {
			nonHost = ag
		}
	}
	e.mustOK(e.do("PATCH", "/v1/nodes/"+anchorA.nodeID, map[string]any{"anchor": true}, e.token))
	if r := hostAgent.createSnapshot(execID, srv, "dep_x", epA, "scheduled", "snap_p", "dd"); r.Status != 201 {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	if r := nonHost.replicaReady("snap_p"); r.Status != 204 {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	var st string
	if err := e.st.DB.Get(&st, `SELECT state FROM snapshots WHERE id='snap_p'`); err != nil {
		t.Fatal(err)
	}
	if st == "committed" {
		t.Fatal("committed without anchor replica")
	}
	if r := anchorA.replicaReady("snap_p"); r.Status != 204 {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	if err := e.st.DB.Get(&st, `SELECT state FROM snapshots WHERE id='snap_p'`); err != nil {
		t.Fatal(err)
	}
	if st != "committed" {
		t.Fatalf("state=%s want committed", st)
	}
}

func TestSingleNodeCommitClamp(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("nodeA")
	srv := e.createServer("testgame", "s1", map[string]any{
		"replication_factor": 5, "min_commit_replicas": 3,
	})
	_, execA, epA := e.startToRunning(srv, a)
	if r := a.createSnapshot(execA, srv, "dep_x", epA, "scheduled", "snap_c", "ee"); r.Status != 201 {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	var st string
	if err := e.st.DB.Get(&st, `SELECT state FROM snapshots WHERE id='snap_c'`); err != nil {
		t.Fatal(err)
	}
	if st != "committed" {
		t.Fatalf("single-node clamp: state=%s", st)
	}
}

func TestAnchorsGetReplicationTasks(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("nodeA")
	b := e.newAgent("nodeB")
	e.mustOK(e.do("PATCH", "/v1/nodes/"+b.nodeID, map[string]any{"anchor": true}, e.token))
	srv := e.createServer("testgame", "s1", map[string]any{"replication_factor": 2})
	hostAgent, execA, epA := e.startToRunning(srv, a, b)
	_ = execA
	if r := hostAgent.createSnapshot(e.activeExec(srv)["id"].(string), srv, "dep_x", epA, "scheduled", "snap_r", "ff"); r.Status != 201 {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	// whichever non-producer agent is the anchor should see the task; if the
	// anchor IS the producer, the task goes to the other node — check both.
	da, _ := a.heartbeat(nil)
	db, _ := b.heartbeat(nil)
	tasks := append(append([]struct {
		SnapshotID string `json:"snapshot_id"`
		ServerID   string `json:"server_id"`
	}{}, da.ReplicationTasks...), db.ReplicationTasks...)
	var saw bool
	for _, rt := range tasks {
		if rt.SnapshotID == "snap_r" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("no replication task issued: a=%+v b=%+v", da.ReplicationTasks, db.ReplicationTasks)
	}
}

func TestMigrationEndToEnd(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("nodeA")
	b := e.newAgent("nodeB")
	srv := e.createServer("testgame", "s1", nil)
	hostAgent, execA, epA := e.startToRunning(srv, a, b)
	var otherAgent *agent
	if hostAgent == a {
		otherAgent = b
	} else {
		otherAgent = a
	}
	target := otherAgent.nodeID

	e.mustOK(e.do("POST", "/v1/servers/"+srv+"/move",
		map[string]any{"target_node_id": target}, e.token))
	e.reconcile()
	// host sees stop directive with migration reason
	d, _ := hostAgent.heartbeat(nil)
	var sawStop bool
	for _, ex := range d.Executions {
		if ex.Action == "stop" {
			sawStop = true
			if ex.StopReason != "migration" {
				t.Fatalf("stop_reason=%q", ex.StopReason)
			}
		}
	}
	if !sawStop {
		t.Fatalf("no stop directive: %+v", d.Executions)
	}
	// host produces final snapshot then reports stopped
	if r := hostAgent.createSnapshot(execA, srv, "dep_x", epA, "final", "snap_mig", "99"); r.Status != 201 {
		t.Fatalf("final snap: %d %s", r.Status, r.Raw)
	}
	if r := hostAgent.execStatus(execA, srv, epA, "stopped"); r.Status != 204 {
		t.Fatalf("stopped: %d %s", r.Status, r.Raw)
	}
	e.reconcile() // stopping → await_replication; replica assigned to target
	// target pulls snapshot: sees replication task, reports ready
	d, _ = otherAgent.heartbeat(nil)
	var task bool
	for _, rt := range d.ReplicationTasks {
		if rt.SnapshotID == "snap_mig" {
			task = true
		}
	}
	if !task {
		t.Fatalf("no replication task on target: %+v", d.ReplicationTasks)
	}
	if r := otherAgent.replicaReady("snap_mig"); r.Status != 204 {
		t.Fatalf("replica ready: %d %s", r.Status, r.Raw)
	}
	e.reconcile() // await_replication → activating → done
	ex := e.activeExec(srv)
	if ex == nil || ex["node"] != target {
		t.Fatalf("no exec on target: %+v", ex)
	}
	// target's directive restores snap_mig
	d, _ = otherAgent.heartbeat(nil)
	for _, ed := range d.Executions {
		if ed.Restore == nil || ed.Restore.SnapshotID != "snap_mig" {
			t.Fatalf("restore: %+v", ed)
		}
	}
}

func TestRestartKeepsState(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{ms: 1735689600000}
	st, err := store.Open("sqlite://"+dir+"/cp.db", clk)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	recon := reconciler.New(st, testTimings, log)
	_, pk, _ := ed25519.GenerateKey(rand.Reader)
	srv := &api.Server{Store: st, Auth: &auth.Local{Store: st, Policy: auth.SignupOpen},
		Cfg: api.Config{Timings: testTimings, Version: "test"}, Recon: recon, RelayKey: pk, Log: log}
	ts := httptest.NewServer(api.NewHandler(srv, nil))
	e := &env{t: t, clk: clk, st: st, recon: recon, srv: srv, http: ts}
	e.token = e.signup("a@b.c", "password123")
	e.group = e.createGroup("g")
	a := e.newAgent("n1")
	srvID := e.createServer("testgame", "s1", nil)
	e.startToRunning(srvID, a)
	execBefore := e.activeExec(srvID)["id"]

	// "restart": reopen same file
	ts.Close()
	_ = st.Close()
	st2, err := store.Open("sqlite://"+dir+"/cp.db", clk)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()
	recon2 := reconciler.New(st2, testTimings, log)
	srv2 := &api.Server{Store: st2, Auth: &auth.Local{Store: st2, Policy: auth.SignupOpen},
		Cfg: api.Config{Timings: testTimings, Version: "test"}, Recon: recon2, RelayKey: pk, Log: log}
	ts2 := httptest.NewServer(api.NewHandler(srv2, nil))
	defer ts2.Close()
	e.st, e.recon, e.srv, e.http = st2, recon2, srv2, ts2

	if ex := e.activeExec(srvID); ex == nil || ex["id"] != execBefore {
		t.Fatalf("execution lost across restart: %+v", ex)
	}
	// agent heartbeats still renew lease on the same epoch
	d, r := a.heartbeat(nil)
	if r.Status != 200 || len(d.Executions) != 1 {
		t.Fatalf("post-restart heartbeat: %d %+v", r.Status, d)
	}
}

func TestParallelStartsNoTwoActive(t *testing.T) {
	e := newEnv(t)
	e.newAgent("n1")
	e.newAgent("n2")
	srv := e.createServer("testgame", "s1", nil)
	done := make(chan int, 8)
	for i := 0; i < 8; i++ {
		go func() {
			r := e.do("POST", "/v1/servers/"+srv+"/start", map[string]any{}, e.token)
			done <- r.Status
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	e.reconcile()
	if n := e.activeExecCount(srv); n > 1 {
		t.Fatalf("%d active executions", n)
	}
}

func TestNoLeaseResurrection(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("n1")
	srv := e.createServer("testgame", "s1", nil)
	host, execID, epoch := e.startToRunning(srv, a)

	var leaseBefore int64
	if err := e.st.DB.Get(&leaseBefore, e.st.Rebind(
		`SELECT lease_expires_at FROM server_executions WHERE id=?`), execID); err != nil {
		t.Fatal(err)
	}

	// Lease expires; a late heartbeat arrives before the reconciler ticks.
	e.clk.advance(testTimings.LeaseTTLMs + 1)
	_, r := host.heartbeat([]map[string]any{
		{"execution_id": execID, "server_id": srv, "epoch": epoch, "state": "running"},
	})
	if r.Status != 200 {
		t.Fatalf("heartbeat: %d %s", r.Status, r.Raw)
	}
	var leaseAfter int64
	if err := e.st.DB.Get(&leaseAfter, e.st.Rebind(
		`SELECT lease_expires_at FROM server_executions WHERE id=? AND ended_at IS NULL`), execID); err != nil {
		t.Fatal(err)
	}
	if leaseAfter != leaseBefore {
		t.Fatalf("expired lease renewed: %d → %d", leaseBefore, leaseAfter)
	}
	// Directive still lists the exec until the reconciler reaps it — but the
	// lease stays expired, so the next pass ends it 'lost'.
	e.reconcile()
	ex := e.st.DB
	var endReason string
	if err := ex.Get(&endReason, e.st.Rebind(
		`SELECT end_reason FROM server_executions WHERE id=?`), execID); err != nil {
		t.Fatalf("execution not ended lost: %v", err)
	}
	if endReason != "lost" {
		t.Fatalf("end_reason=%s", endReason)
	}
}

func TestCrashLoopGuard(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("n1")
	b := e.newAgent("n2")
	srv := e.createServer("testgame", "s1", nil)
	host, execID, epoch := e.startToRunning(srv, a, b)

	// agent reports the run failed while desired is still 'running'
	if r := host.execStatus(execID, srv, epoch, "failed"); r.Status != 204 {
		t.Fatalf("failed report: %d %s", r.Status, r.Raw)
	}
	e.reconcile()
	if e.observed(srv) != "failed" {
		t.Fatalf("observed=%s", e.observed(srv))
	}
	var endReason string
	if err := e.st.DB.Get(&endReason, e.st.Rebind(
		`SELECT end_reason FROM server_executions WHERE id=?`), execID); err != nil {
		t.Fatal(err)
	}
	if endReason != "failed" {
		t.Fatalf("running failure end_reason=%q", endReason)
	}
	// several passes: never a new execution, and server.failed only once
	for i := 0; i < 3; i++ {
		e.reconcile()
	}
	if n := e.activeExecCount(srv); n != 0 {
		t.Fatalf("auto-reactivated after failure (%d active)", n)
	}
	var failedEvents int
	if err := e.st.DB.Get(&failedEvents, e.st.Rebind(
		`SELECT COUNT(*) FROM events WHERE server_id=? AND type='server.failed'`), srv); err != nil {
		t.Fatal(err)
	}
	if failedEvents != 1 {
		t.Fatalf("server.failed emitted %d times", failedEvents)
	}

	// explicit start clears the guard and activates normally
	r := e.do("POST", "/v1/servers/"+srv+"/start", map[string]any{}, e.token)
	e.mustOK(r)
	ex := e.activeExec(srv)
	if ex == nil || ex["id"] == execID {
		t.Fatalf("no new execution after explicit start: %+v", ex)
	}
	if ex["epoch"].(int64) != epoch+1 {
		t.Fatalf("epoch %d → %d", epoch, ex["epoch"])
	}
}

func TestRestoreFailureRetries(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("nodeA")
	b := e.newAgent("nodeB")
	srv := e.createServer("testgame", "restore-retry", map[string]any{
		"preferred_node_id":   a.nodeID,
		"replication_factor":  2,
		"min_commit_replicas": 1,
	})
	host, execID, epoch := e.startToRunning(srv, a, b)
	other := a
	if host == a {
		other = b
	}
	const snapshotID = "snap_restore_retry"
	if r := host.createSnapshot(execID, srv, "dep_x", epoch, "final", snapshotID, "restore"); r.Status != 201 {
		t.Fatalf("create snapshot: %d %s", r.Status, r.Raw)
	}
	if r := other.replicaReady(snapshotID); r.Status != 204 {
		t.Fatalf("replica ready: %d %s", r.Status, r.Raw)
	}

	e.clk.advance(testTimings.LeaseTTLMs + testTimings.OfflineAfterMs + 1000)
	other.heartbeat(nil)
	e.reconcile()
	first := e.activeExec(srv)
	if first == nil || first["node"] != other.nodeID {
		t.Fatalf("no recovery execution on online node: %+v", first)
	}

	reportRestoreFailure := func() string {
		e.t.Helper()
		active := e.activeExec(srv)
		if active == nil || active["node"] != other.nodeID {
			e.t.Fatalf("no recovery execution to fail: %+v", active)
		}
		recoveryID := active["id"].(string)
		recoveryEpoch := active["epoch"].(int64)
		d, r := other.heartbeat(nil)
		if r.Status != 200 {
			e.t.Fatalf("recovery heartbeat: %d %s", r.Status, r.Raw)
		}
		foundRestore := false
		for _, directive := range d.Executions {
			if directive.ExecutionID == recoveryID && directive.Restore != nil &&
				directive.Restore.SnapshotID == snapshotID {
				foundRestore = true
			}
		}
		if !foundRestore {
			e.t.Fatalf("recovery execution %s did not restore %s: %+v", recoveryID, snapshotID, d.Executions)
		}
		if r := other.execStatus(recoveryID, srv, recoveryEpoch, "restoring"); r.Status != 204 {
			e.t.Fatalf("restoring report: %d %s", r.Status, r.Raw)
		}
		if r := other.execStatusWithMessage(recoveryID, srv, recoveryEpoch, "failed", "manifest fetch failed"); r.Status != 204 {
			e.t.Fatalf("restore failed report: %d %s", r.Status, r.Raw)
		}
		return recoveryID
	}

	firstFailedID := reportRestoreFailure()
	e.reconcile()
	if got := e.observed(srv); got != "recovering" {
		t.Fatalf("observed after first restore failure=%q", got)
	}
	if e.activeExec(srv) != nil {
		t.Fatal("recovery retried before the backoff elapsed")
	}
	var endReason string
	if err := e.st.DB.Get(&endReason, e.st.Rebind(
		`SELECT end_reason FROM server_executions WHERE id=?`), firstFailedID); err != nil {
		t.Fatal(err)
	}
	if endReason != "restore_failed" {
		t.Fatalf("restore failure end_reason=%q", endReason)
	}
	var eventCount int
	if err := e.st.DB.Get(&eventCount, e.st.Rebind(
		`SELECT COUNT(*) FROM events WHERE server_id=? AND type='server.restore_failed'`), srv); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("server.restore_failed event count=%d", eventCount)
	}
	var eventData string
	if err := e.st.DB.Get(&eventData, e.st.Rebind(
		`SELECT data_json FROM events WHERE server_id=? AND type='server.restore_failed' ORDER BY id DESC LIMIT 1`), srv); err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(eventData), &data); err != nil {
		t.Fatal(err)
	}
	if data["execution_id"] != firstFailedID || data["message"] != "manifest fetch failed" {
		t.Fatalf("restore failure event data=%+v", data)
	}
	if data["epoch"] != float64(first["epoch"].(int64)) {
		t.Fatalf("restore failure event epoch=%v", data["epoch"])
	}

	e.clk.advance(9999)
	other.heartbeat(nil)
	e.reconcile()
	if e.activeExec(srv) != nil {
		t.Fatal("recovery retried before 10 seconds")
	}
	e.clk.advance(1)
	other.heartbeat(nil)
	e.reconcile()
	if e.activeExec(srv) == nil {
		t.Fatal("recovery did not retry after 10 seconds")
	}

	reportRestoreFailure()
	e.reconcile()
	if e.observed(srv) != "recovering" || e.activeExec(srv) != nil {
		t.Fatalf("state after second restore failure: observed=%s active=%+v", e.observed(srv), e.activeExec(srv))
	}
	e.clk.advance(10_000)
	other.heartbeat(nil)
	e.reconcile()
	if e.activeExec(srv) == nil {
		t.Fatal("third recovery execution was not created")
	}

	reportRestoreFailure()
	e.reconcile()
	if e.observed(srv) != "failed" {
		t.Fatalf("observed after three restore failures=%q", e.observed(srv))
	}
	if e.activeExec(srv) != nil {
		t.Fatal("recovery continued after three restore failures")
	}
	e.clk.advance(60_000)
	other.heartbeat(nil)
	e.reconcile()
	if e.activeExec(srv) != nil || e.observed(srv) != "failed" {
		t.Fatalf("server retried after restore failure limit: observed=%s active=%+v", e.observed(srv), e.activeExec(srv))
	}
	if err := e.st.DB.Get(&eventCount, e.st.Rebind(
		`SELECT COUNT(*) FROM events WHERE server_id=? AND type='server.restore_failed'`), srv); err != nil {
		t.Fatal(err)
	}
	if eventCount != 3 {
		t.Fatalf("server.restore_failed event count=%d, want 3", eventCount)
	}
}

func TestFailedExecDoesNotBlockLostRecovery(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("n1")
	b := e.newAgent("n2")
	srv := e.createServer("testgame", "s1", nil)
	host, execID, _ := e.startToRunning(srv, a, b)
	other := a
	if host == a {
		other = b
	}

	// lease expiry (lost) still triggers automatic recovery
	e.clk.advance(testTimings.LeaseTTLMs + testTimings.OfflineAfterMs + 1000)
	other.heartbeat(nil)
	e.reconcile()
	ex := e.activeExec(srv)
	if ex == nil || ex["id"] == execID || ex["node"] == host.nodeID {
		t.Fatalf("no automatic recovery after lost lease: %+v", ex)
	}
}

// ---- cookie sessions + CSRF ----

// rawDo performs a request with explicit headers/cookies and returns the
// raw response so cookie headers can be inspected.
func (e *env) rawDo(method, path string, body []byte, headers map[string]string, cookies ...*http.Cookie) *http.Response {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.http.URL+path, bytes.NewReader(body))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp
}

func sessionCookie(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == "varde_session" {
			return c
		}
	}
	t.Fatalf("no varde_session cookie set")
	return nil
}

func TestCookieSession(t *testing.T) {
	e := newEnv(t)
	e.do("POST", "/v1/auth/signup", map[string]any{
		"email": "cookie@example.com", "password": "pw123456!", "display_name": "c",
	}, "")
	login, _ := json.Marshal(map[string]any{"email": "cookie@example.com", "password": "pw123456!"})
	resp := e.rawDo("POST", "/v1/auth/login", login, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	c := sessionCookie(t, resp)
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge <= 0 {
		t.Fatalf("bad cookie attrs: %+v", c)
	}
	// cookie works for auth
	if r := e.rawDo("GET", "/v1/me", nil, nil, c); r.StatusCode != 200 {
		t.Fatalf("cookie /me: %d", r.StatusCode)
	}
	// logout clears the cookie and revokes the session
	resp = e.rawDo("POST", "/v1/auth/logout", nil,
		map[string]string{"Origin": e.http.URL}, c)
	if resp.StatusCode != 204 {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if c2 := sessionCookie(t, resp); c2.MaxAge >= 0 {
		t.Fatalf("logout cookie not cleared: %+v", c2)
	}
	if r := e.rawDo("GET", "/v1/me", nil, nil, c); r.StatusCode != 401 {
		t.Fatalf("revoked session still works: %d", r.StatusCode)
	}
}

func TestCSRFGuard(t *testing.T) {
	e := newEnv(t)
	r := e.do("POST", "/v1/auth/signup", map[string]any{
		"email": "csrf@example.com", "password": "pw123456!", "display_name": "c",
	}, "")
	token := r.Body["token"].(string)
	c := &http.Cookie{Name: "varde_session", Value: token}
	body, _ := json.Marshal(map[string]any{"name": "g2"})

	// cookie + matching Origin (request's own host) passes
	if resp := e.rawDo("POST", "/v1/groups", body,
		map[string]string{"Origin": e.http.URL}, c); resp.StatusCode != 201 {
		t.Fatalf("cookie+matching origin: %d", resp.StatusCode)
	}
	// cookie + foreign Origin rejected
	if resp := e.rawDo("POST", "/v1/groups", body,
		map[string]string{"Origin": "https://evil.example"}, c); resp.StatusCode != 403 {
		t.Fatalf("cookie+foreign origin: %d", resp.StatusCode)
	}
	// cookie + no Origin/Referer rejected
	if resp := e.rawDo("POST", "/v1/groups", body, nil, c); resp.StatusCode != 403 {
		t.Fatalf("cookie+no origin: %d", resp.StatusCode)
	}
	// Referer fallback passes
	if resp := e.rawDo("POST", "/v1/groups", body,
		map[string]string{"Referer": e.http.URL + "/groups"}, c); resp.StatusCode != 201 {
		t.Fatalf("cookie+matching referer: %d", resp.StatusCode)
	}
	// bearer + no Origin passes (CSRF only applies to cookie sessions)
	if resp := e.rawDo("POST", "/v1/groups", body,
		map[string]string{"Authorization": "Bearer " + token}); resp.StatusCode != 201 {
		t.Fatalf("bearer+no origin: %d", resp.StatusCode)
	}
	// cookie + matching public-url Origin passes even when host differs
	e.srv.Cfg.PublicURL = "https://varde.example.com"
	if resp := e.rawDo("POST", "/v1/groups", body,
		map[string]string{"Origin": "https://varde.example.com"}, c); resp.StatusCode != 201 {
		t.Fatalf("cookie+public-url origin: %d", resp.StatusCode)
	}
}

// heartbeatWithMesh reports mesh peers as part of the heartbeat body.
func (a *agent) heartbeatWithMesh(peers []map[string]any) apiResp {
	a.e.t.Helper()
	body := map[string]any{
		"capabilities": caps(), "executions": []any{},
		"mesh": map[string]any{"peers": peers},
	}
	raw, _ := json.Marshal(body)
	return a.signedDo("POST", "/v1/agent/heartbeat", raw, 0, nil)
}

func TestNodeConnections(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("node-a")
	b := e.newAgent("node-b")
	// name the nodes so connections can resolve names
	e.mustOK(e.do("PATCH", "/v1/nodes/"+a.nodeID, map[string]any{"name": "alpha"}, e.token))
	e.mustOK(e.do("PATCH", "/v1/nodes/"+b.nodeID, map[string]any{"name": "beta"}, e.token))

	a.heartbeatWithMesh([]map[string]any{
		{"node_id": b.nodeID, "path": "direct", "rtt_us": 1234},
		{"node_id": "node_not_in_group", "path": "relayed", "rtt_us": 9},
	})
	e.clk.advance(1000)
	a.heartbeatWithMesh([]map[string]any{
		{"node_id": b.nodeID, "path": "direct", "rtt_us": 1234},
		{"node_id": "node_not_in_group", "path": "relayed", "rtt_us": 9},
	})

	check := func(m map[string]any, where string) {
		conns, _ := m["connections"].([]any)
		if len(conns) != 1 {
			t.Fatalf("%s: expected 1 connection, got %v", where, m["connections"])
		}
		c := conns[0].(map[string]any)
		if c["node_id"] != b.nodeID || c["name"] != "beta" || c["path"] != "direct" || c["rtt_us"] != 1234.0 {
			t.Fatalf("%s: bad connection %+v", where, c)
		}
	}
	check(e.mustOK(e.do("GET", "/v1/nodes/"+a.nodeID, nil, e.token)), "GET node")
	list := e.mustOK(e.do("GET", "/v1/groups/"+e.group+"/nodes", nil, e.token))
	found := false
	for _, n := range list["nodes"].([]any) {
		nm := n.(map[string]any)
		switch nm["id"] {
		case a.nodeID:
			found = true
			check(nm, "list nodes")
		case b.nodeID:
			if conns, _ := nm["connections"].([]any); len(conns) != 0 {
				t.Fatalf("node b should have empty connections, got %v", conns)
			}
		}
	}
	if !found {
		t.Fatal("node a not in list")
	}
}

// An admin must not mint owner invites — that would let them promote
// themselves. Owners (and operators) still can.
func TestOwnerInviteRequiresOwner(t *testing.T) {
	e := newEnv(t)
	adminTok := e.signup("admin@example.com", "password123")
	admin, err := e.st.UserByEmail(context.Background(), "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.AddMember(context.Background(), e.group, admin.ID, "admin", e.clk.ms); err != nil {
		t.Fatal(err)
	}
	r := e.do("POST", "/v1/groups/"+e.group+"/invites",
		map[string]any{"role": "owner"}, adminTok)
	if r.Status != 403 {
		t.Fatalf("admin creating owner invite: want 403, got %d %s", r.Status, r.Raw)
	}
	// member invites are still fine for admins
	e.mustOK(e.do("POST", "/v1/groups/"+e.group+"/invites",
		map[string]any{"role": "member"}, adminTok))
	// and the group owner can still create owner invites
	e.mustOK(e.do("POST", "/v1/groups/"+e.group+"/invites",
		map[string]any{"role": "owner"}, e.token))
}

// Node settings can only be changed by the node's owner (the user whose
// enrollment created it), an admin/owner of the group, or an operator.
func TestUpdateNodeRequiresOwnership(t *testing.T) {
	e := newEnv(t)
	memberTok := e.signup("member@example.com", "password123")
	member, err := e.st.UserByEmail(context.Background(), "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.AddMember(context.Background(), e.group, member.ID, "member", e.clk.ms); err != nil {
		t.Fatal(err)
	}
	adminTok := e.signup("admin@example.com", "password123")
	admin, err := e.st.UserByEmail(context.Background(), "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.AddMember(context.Background(), e.group, admin.ID, "admin", e.clk.ms); err != nil {
		t.Fatal(err)
	}

	// a node enrolled via the operator's token is owned by the operator
	a := e.newAgent("owned")
	var owner sql.NullString
	if err := e.st.DB.GetContext(context.Background(), &owner, e.st.Rebind(
		`SELECT owner_user_id FROM nodes WHERE id=?`), a.nodeID); err != nil {
		t.Fatal(err)
	}
	if !owner.Valid {
		t.Fatal("enrolled node has NULL owner_user_id")
	}

	// a plain member cannot change someone else's node
	r := e.do("PATCH", "/v1/nodes/"+a.nodeID,
		map[string]any{"name": "renamed"}, memberTok)
	if r.Status != 403 {
		t.Fatalf("member updating other's node: want 403, got %d %s", r.Status, r.Raw)
	}
	// an admin can
	e.mustOK(e.do("PATCH", "/v1/nodes/"+a.nodeID,
		map[string]any{"name": "renamed"}, adminTok))
	// and so can the owner (the operator user here)
	e.mustOK(e.do("PATCH", "/v1/nodes/"+a.nodeID,
		map[string]any{"name": "renamed-again"}, e.token))

	// a member-owned node: hand the member ownership directly
	if _, err := e.st.DB.ExecContext(context.Background(), e.st.Rebind(
		`UPDATE nodes SET owner_user_id=? WHERE id=?`), member.ID, a.nodeID); err != nil {
		t.Fatal(err)
	}
	e.mustOK(e.do("PATCH", "/v1/nodes/"+a.nodeID,
		map[string]any{"hosting_enabled": false}, memberTok))

	// a node with NULL owner is admin-only
	if _, err := e.st.DB.ExecContext(context.Background(), e.st.Rebind(
		`UPDATE nodes SET owner_user_id=NULL WHERE id=?`), a.nodeID); err != nil {
		t.Fatal(err)
	}
	r = e.do("PATCH", "/v1/nodes/"+a.nodeID,
		map[string]any{"name": "nope"}, memberTok)
	if r.Status != 403 {
		t.Fatalf("member updating ownerless node: want 403, got %d %s", r.Status, r.Raw)
	}
	e.mustOK(e.do("PATCH", "/v1/nodes/"+a.nodeID,
		map[string]any{"name": "yep"}, adminTok))
}

// /logs?execution_id= must not leak executions from other servers.
func TestServerLogsRejectsForeignExecution(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("nodeA")
	b := e.newAgent("nodeB")
	srvA := e.createServer("testgame", "a", nil)
	srvB := e.createServer("testgame", "b", nil)
	_, execA, _ := e.startToRunning(srvA, a, b)
	_, _, _ = e.startToRunning(srvB, a, b)

	r := e.do("GET", "/v1/servers/"+srvB+"/logs?execution_id="+execA, nil, e.token)
	if r.Status != 404 {
		t.Fatalf("foreign execution: want 404, got %d %s", r.Status, r.Raw)
	}
	r = e.do("GET", "/v1/servers/"+srvA+"/logs?execution_id="+execA, nil, e.token)
	if r.Status != 200 {
		t.Fatalf("own execution: want 200, got %d %s", r.Status, r.Raw)
	}
	r = e.do("GET", "/v1/servers/"+srvA+"/logs?execution_id=exec_nonexistent", nil, e.token)
	if r.Status != 404 {
		t.Fatalf("nonexistent execution: want 404, got %d %s", r.Status, r.Raw)
	}
}

// A replica-ready report is only accepted from a node the CP assigned the
// snapshot to (or the node that produced it). Anything else gets a 409 and
// changes nothing.
func TestReplicaReadyRequiresAssignment(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("nodeA")
	b := e.newAgent("nodeB")
	c := e.newAgent("nodeC")
	srv := e.createServer("testgame", "s1", nil)
	host, execA, epA := e.startToRunning(srv, a, b, c)
	var others []*agent
	for _, n := range []*agent{a, b, c} {
		if n != host {
			others = append(others, n)
		}
	}
	outsider, assigned := others[0], others[1]
	if r := host.createSnapshot(execA, srv, "dep_x", epA, "final", "snap_x", "aa"); r.Status != 201 {
		t.Fatalf("snap %d %s", r.Status, r.Raw)
	}
	// remove the outsider's replica row entirely — it was never assigned
	if _, err := e.st.DB.Exec(e.st.Rebind(
		`DELETE FROM snapshot_replicas WHERE snapshot_id='snap_x' AND node_id=?`), outsider.nodeID); err != nil {
		t.Fatal(err)
	}
	if r := outsider.replicaReady("snap_x"); r.Status != 409 {
		t.Fatalf("unassigned replica ready: want 409, got %d %s", r.Status, r.Raw)
	}
	var state string
	if err := e.st.DB.Get(&state, e.st.Rebind(
		`SELECT state FROM snapshots WHERE id='snap_x'`)); err != nil {
		t.Fatal(err)
	}
	if state == "committed" {
		t.Fatal("unassigned ready report committed the snapshot")
	}
	// an assigned node can still report ready
	if r := assigned.replicaReady("snap_x"); r.Status != 204 {
		t.Fatalf("assigned replica ready: want 204, got %d %s", r.Status, r.Raw)
	}
}
