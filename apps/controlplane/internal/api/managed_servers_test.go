package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grendalaget/varde/apps/controlplane/internal/api"
	"github.com/grendalaget/varde/apps/controlplane/internal/api/gen"
	"github.com/grendalaget/varde/apps/controlplane/internal/plugins"
	"github.com/grendalaget/varde/apps/controlplane/internal/secrets"
)

type managedHook struct {
	calls       atomic.Int64
	fail        atomic.Bool
	mu          sync.Mutex
	url         string
	token       string
	key         string
	beforeReply func()
}

func newManagedEnv(t *testing.T) (*env, *managedHook, string) {
	t.Helper()
	return newManagedEnvManifest(t, nil)
}

func newManagedEnvManifest(t *testing.T, edit func(*plugins.Manifest)) (*env, *managedHook, string) {
	t.Helper()
	e := newEnv(t)
	hook := &managedHook{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/varde/v1/settings" {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		hook.calls.Add(1)
		var body struct {
			Settings map[string]any `json:"settings"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode settings hook: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		hook.mu.Lock()
		hook.url, _ = body.Settings["at_url"].(string)
		hook.token, _ = body.Settings["at_token"].(string)
		hook.key = r.Header.Get("Idempotency-Key")
		beforeReply := hook.beforeReply
		hook.mu.Unlock()
		if beforeReply != nil {
			beforeReply()
		}
		w.Header().Set("Content-Type", "application/json")
		if hook.fail.Load() {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"licence rejected"}`))
			return
		}
		_, _ = w.Write([]byte(`{"store":{"cached":"fleet-key-cache"}}`))
	}))
	t.Cleanup(server.Close)
	t.Setenv("MANAGED_PLUGIN_SECRET", strings.Repeat("s", 32))
	m := plugins.Manifest{ID: "managed-test", Name: "Managed tournament", Version: "1", BaseURL: server.URL, SecretEnv: "MANAGED_PLUGIN_SECRET",
		Settings: []plugins.Field{
			{Name: "hosting", Type: "select", Default: "managed", Options: []string{"managed", "manual"}},
			{Name: "licence", Type: "select", Required: true, Options: []string{"noncommercial", "commercial"}},
			{Name: "at_url", Type: "string", Required: true},
			{Name: "at_token", Type: "secret", Required: true},
			{Name: "cached", Type: "string", Hidden: true},
		},
		ManagedServer: &plugins.ManagedServer{Game: "autotournament", ModeSetting: "hosting", Config: map[string]string{"licence": "licence"}, Bindings: map[string]string{"at_url": "public_url", "at_token": "secret:api_token"}},
	}
	if edit != nil {
		edit(&m)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	e.srv.Plugins, err = plugins.Load([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Secrets, err = secrets.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	e.srv.PluginHTTP = server.Client()
	e.srv.Cfg.Gateway = &api.GatewayConf{URL: "wss://gateway.example.org/v1/tunnel", Domain: "example.site"}
	return e, hook, "/v1/groups/" + e.group + "/plugins/managed-test"
}

func TestManagedServerRequiredGeneratedSecret(t *testing.T) {
	e, _, path := newManagedEnvManifest(t, func(m *plugins.Manifest) {
		m.Settings = []plugins.Field{
			{Name: "hosting", Type: "select", Default: "managed", Options: []string{"managed", "manual"}},
			{Name: "title", Type: "string", Required: true},
			{Name: "world", Type: "string", Required: true},
			{Name: "password", Type: "secret", Required: true},
		}
		m.ManagedServer = &plugins.ManagedServer{Game: "valheim", ModeSetting: "hosting", Config: map[string]string{"server_name": "title", "world_name": "world"}, Bindings: map[string]string{"password": "secret:password"}}
	})
	e.mustOK(e.do("PUT", path, map[string]any{"settings": map[string]any{"title": "Managed Valheim", "world": "world"}}, e.token))
	a := e.newAgent("valheim-pc")
	caps := tournamentCaps()
	caps["drivers"] = []string{"valheim"}
	caps["runtimes"] = []string{"steamcmd"}
	a.heartbeatCaps(nil, caps)
	started := e.mustOK(e.do("POST", path+"/managed-server/start", nil, e.token))
	sid := started["server_id"].(string)
	_, reply := a.heartbeatCaps(nil, caps)
	var directives gen.AgentDirectives
	if err := json.Unmarshal(reply.Raw, &directives); err != nil {
		t.Fatal(err)
	}
	if directives.Executions == nil || len(*directives.Executions) != 1 {
		t.Fatal("managed Valheim execution missing, want a run directive")
	}
	config := (*directives.Executions)[0].Config
	password, _ := (*config)["password"].(string)
	if len(password) < 16 {
		t.Fatal("required managed password missing from runtime configuration")
	}
	srv, err := e.st.GetServer(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(srv.ConfigJSON, password) || strings.Contains(srv.ConfigJSON, "password") {
		t.Error("required generated secret persisted in ordinary config")
	}
}

func TestManagedStandbyGetsRuntimeSecrets(t *testing.T) {
	e, _, path := newManagedEnv(t)
	e.mustOK(e.do("PUT", path, map[string]any{"settings": map[string]any{"licence": "noncommercial"}}, e.token))
	a, b := e.newAgent("host"), e.newAgent("standby")
	a.heartbeatCaps(nil, tournamentCaps())
	b.heartbeatCaps(nil, tournamentCaps())
	status := e.mustOK(e.do("POST", path+"/managed-server/start", nil, e.token))
	id := status["server_id"].(string)
	host, _, _ := e.startToRunning(id, a, b)
	standby := a
	if host == a {
		standby = b
	}
	d, _ := standby.heartbeatCaps(nil, tournamentCaps())
	serving := managedDirective(t, host, id)
	if len(d.Standby) != 1 {
		t.Fatal("managed standby missing")
	}
	for _, field := range []string{"api_token", "public_url", "licence"} {
		if d.Standby[0].Config[field] != (*serving.Config)[field] {
			t.Fatalf("standby %s differs from serving config", field)
		}
	}
}

func TestManagedOrphansStop(t *testing.T) {
	for _, mode := range []string{"missing plugin", "disabled group", "manual mode", "uninstall"} {
		t.Run(mode, func(t *testing.T) {
			e, _, path := newManagedEnv(t)
			e.mustOK(e.do("PUT", path, map[string]any{"settings": map[string]any{"licence": "noncommercial"}}, e.token))
			a := e.newAgent("host")
			a.heartbeatCaps(nil, tournamentCaps())
			status := e.mustOK(e.do("POST", path+"/managed-server/start", nil, e.token))
			id := status["server_id"].(string)
			switch mode {
			case "missing plugin":
				registry, err := plugins.Load(nil)
				if err != nil {
					t.Fatal(err)
				}
				e.srv.Plugins = registry
			case "disabled group":
				if err := e.st.DeleteGroupPlugin(t.Context(), e.group, "managed-test"); err != nil {
					t.Fatal(err)
				}
			case "manual mode":
				if _, err := e.st.DB.Exec(`UPDATE group_plugins SET settings_json='{"hosting":"manual"}' WHERE group_id=?`, e.group); err != nil {
					t.Fatal(err)
				}
			case "uninstall":
				e.srv.PluginManager = emptyPluginManager{}
				e.mustOK(e.do("DELETE", "/v1/admin/plugins/managed-test", nil, e.token))
			}
			if mode != "uninstall" {
				if err := e.srv.ReconcileManagedServers(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			srv, err := e.st.GetServer(t.Context(), id)
			if err != nil || srv.DesiredState != "stopped" {
				t.Fatalf("orphan desired state = %+v, error %v", srv, err)
			}
		})
	}
}

func TestManagedConfigChangeRestartsAndReacknowledges(t *testing.T) {
	e, hook, path := newManagedEnv(t)
	e.mustOK(e.do("PUT", path, map[string]any{"settings": map[string]any{"licence": "noncommercial"}}, e.token))
	a := e.newAgent("host")
	a.heartbeatCaps(nil, tournamentCaps())
	status := e.mustOK(e.do("POST", path+"/managed-server/start", nil, e.token))
	id := status["server_id"].(string)
	_, oldID, oldEpoch := e.startToRunning(id, a)
	reportManagedHealthy(t, e, a, id, oldID, oldEpoch)
	if err := e.srv.ReconcileManagedServers(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.mustOK(e.do("PUT", path, map[string]any{"settings": map[string]any{"licence": "commercial"}}, e.token))
	e.mustOK(e.do("POST", path+"/managed-server/start", nil, e.token))
	_, response := a.heartbeatCaps(nil, tournamentCaps())
	var directives gen.AgentDirectives
	if err := json.Unmarshal(response.Raw, &directives); err != nil {
		t.Fatal(err)
	}
	if directives.Executions == nil || len(*directives.Executions) != 1 || (*directives.Executions)[0].Action != "stop" {
		t.Fatal("config change did not stop the old execution")
	}
	row, err := e.st.ManagedPluginServer(t.Context(), e.group, "managed-test")
	if err != nil || row.BindingDigest != "" {
		t.Fatal("old binding acknowledgement survived config change")
	}
	e.mustOK(a.execStatus(oldID, id, oldEpoch, "stopped"))
	e.reconcile()
	next := managedDirective(t, a, id)
	if next.ExecutionId == oldID || next.Epoch <= oldEpoch || (*next.Config)["licence"] != "commercial" {
		t.Fatal("new execution does not have new config and epoch")
	}
	e.mustOK(a.execStatus(next.ExecutionId, id, next.Epoch, "running"))
	reportManagedHealthy(t, e, a, id, next.ExecutionId, next.Epoch)
	if err := e.srv.ReconcileManagedServers(t.Context()); err != nil {
		t.Fatal(err)
	}
	if hook.calls.Load() != 2 {
		t.Fatalf("hook calls = %d, want a fresh acknowledgement", hook.calls.Load())
	}
	e.mustOK(e.do("POST", path+"/managed-server/start", nil, e.token))
	if managedDirective(t, a, id).Action != "run" {
		t.Fatal("unchanged config triggered another restart")
	}
}

func TestServerConfigEditRestartsExecution(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("host")
	id := e.createServer("testgame", "Config", nil)
	_, execution, epoch := e.startToRunning(id, a)
	e.mustOK(e.do("PATCH", "/v1/servers/"+id, map[string]any{"config": map[string]any{"message": "updated"}}, e.token))
	d, _ := a.heartbeat(nil)
	if len(d.Executions) != 1 || d.Executions[0].Action != "stop" {
		t.Fatal("ordinary config edit did not request a graceful stop")
	}
	e.mustOK(a.execStatus(execution, id, epoch, "stopped"))
	e.reconcile()
	_, response := a.heartbeat(nil)
	var actual gen.AgentDirectives
	if err := json.Unmarshal(response.Raw, &actual); err != nil {
		t.Fatal(err)
	}
	if actual.Executions == nil || len(*actual.Executions) != 1 || (*(*actual.Executions)[0].Config)["message"] != "updated" || (*actual.Executions)[0].ExecutionId == execution {
		t.Fatal("ordinary restart did not apply new config")
	}
}

func TestManagedStartReplacesDeletedServer(t *testing.T) {
	for _, retained := range []bool{false, true} {
		t.Run(map[bool]string{false: "purged", true: "retained saves"}[retained], func(t *testing.T) {
			e, _, path := newManagedEnv(t)
			e.mustOK(e.do("PUT", path, map[string]any{"settings": map[string]any{"licence": "noncommercial"}}, e.token))
			a := e.newAgent("host")
			a.heartbeatCaps(nil, tournamentCaps())
			status := e.mustOK(e.do("POST", path+"/managed-server/start", nil, e.token))
			id := status["server_id"].(string)
			_, execution, epoch := e.startToRunning(id, a)
			if retained {
				srv, err := e.st.GetServer(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				e.mustOK(a.createSnapshot(execution, id, *srv.DeploymentID, epoch, "scheduled", "snap_owned", "88"))
			}
			e.mustOK(e.do("POST", path+"/managed-server/stop", nil, e.token))
			e.mustOK(a.execStatus(execution, id, epoch, "stopped"))
			e.mustOK(e.do("DELETE", "/v1/servers/"+id, nil, e.token))
			if retained {
				old, err := e.st.ManagedPluginServer(t.Context(), e.group, "managed-test")
				if err != nil || old.ServerID != id {
					t.Fatal("fixture did not retain deleted owned server")
				}
			}
			a.heartbeatCaps(nil, tournamentCaps())
			replacement := e.mustOK(e.do("POST", path+"/managed-server/start", nil, e.token))
			if replacement["server_id"] == id {
				t.Fatal("deleted server was reused")
			}
			row, err := e.st.ManagedPluginServer(t.Context(), e.group, "managed-test")
			if err != nil || row.ServerID != replacement["server_id"] {
				t.Fatal("ownership row was not repointed")
			}
			e.mustOK(e.do("POST", path+"/managed-server/start", nil, e.token))
			servers, err := e.st.ListServers(t.Context(), e.group)
			if err != nil || len(servers) != 1 {
				t.Fatal("replacement is not unique")
			}
		})
	}
}

func reportManagedHealthy(t *testing.T, e *env, a *agent, serverID, executionID string, epoch int64) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"server_id": serverID, "epoch": epoch, "state": "running", "health": "ok"})
	if err != nil {
		t.Fatal(err)
	}
	e.mustOK(a.signedDo("POST", "/v1/agent/executions/"+executionID+"/status", body, 0, nil))
}

func TestManagedBindingDoesNotRestoreConcurrentGroupChanges(t *testing.T) {
	for _, change := range []string{"delete", "manual"} {
		t.Run(change, func(t *testing.T) {
			e, hook, path := newManagedEnv(t)
			e.mustOK(e.do("PUT", path, map[string]any{"settings": map[string]any{"licence": "noncommercial"}}, e.token))
			a := e.newAgent("host")
			a.heartbeatCaps(nil, tournamentCaps())
			status := e.mustOK(e.do("POST", path+"/managed-server/start", nil, e.token))
			id := status["server_id"].(string)
			_, execution, epoch := e.startToRunning(id, a)
			reportManagedHealthy(t, e, a, id, execution, epoch)
			hook.mu.Lock()
			hook.beforeReply = func() {
				if change == "delete" {
					if err := e.st.DeleteGroupPlugin(t.Context(), e.group, "managed-test"); err != nil {
						t.Error(err)
					}
					return
				}
				group, err := e.st.GroupPlugin(t.Context(), e.group, "managed-test")
				if err != nil {
					t.Error(err)
					return
				}
				var settings map[string]any
				if err := json.Unmarshal([]byte(group.SettingsJSON), &settings); err != nil {
					t.Error(err)
					return
				}
				settings["hosting"] = "manual"
				raw, _ := json.Marshal(settings)
				group.SettingsJSON = string(raw)
				if err := e.st.UpsertGroupPlugin(t.Context(), *group); err != nil {
					t.Error(err)
				}
			}
			hook.mu.Unlock()
			if err := e.srv.ReconcileManagedServers(t.Context()); err != nil {
				t.Fatal(err)
			}
			group, err := e.st.GroupPlugin(t.Context(), e.group, "managed-test")
			if change == "delete" {
				if err == nil {
					t.Fatal("deleted plugin resurrected")
				}
			} else if err != nil || !strings.Contains(group.SettingsJSON, `"hosting":"manual"`) {
				t.Fatal("concurrent mode change overwritten")
			}
			row, err := e.st.ManagedPluginServer(t.Context(), e.group, "managed-test")
			if err != nil || row.BindingDigest != "" {
				t.Fatal("stale hook response acknowledged")
			}
			if err := e.srv.ReconcileManagedServers(t.Context()); err != nil {
				t.Fatal(err)
			}
			srv, err := e.st.GetServer(t.Context(), id)
			if err != nil || srv.DesiredState != "stopped" {
				t.Fatal("concurrent disable did not converge to stopped")
			}
		})
	}
}

func TestManagedBindingCASDoesNotInsertDeletedGroup(t *testing.T) {
	e, _, path := newManagedEnv(t)
	e.mustOK(e.do("PUT", path, map[string]any{"settings": map[string]any{"licence": "noncommercial"}}, e.token))
	group, err := e.st.GroupPlugin(t.Context(), e.group, "managed-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.DeleteGroupPlugin(t.Context(), e.group, "managed-test"); err != nil {
		t.Fatal(err)
	}
	updated, err := e.st.CompareAndSwapGroupPluginSettings(t.Context(), group, group)
	if err != nil || updated {
		t.Fatal("binding CAS recreated a deleted group plugin")
	}
}

func managedDirective(t *testing.T, a *agent, serverID string) gen.ExecutionDirective {
	t.Helper()
	_, reply := a.heartbeatCaps(nil, tournamentCaps())
	var directives gen.AgentDirectives
	if err := json.Unmarshal(reply.Raw, &directives); err != nil {
		t.Fatal(err)
	}
	if directives.Executions != nil {
		for _, ed := range *directives.Executions {
			if ed.ServerId == serverID && ed.Action == "run" {
				return ed
			}
		}
	}
	t.Fatal("heartbeat has no managed execution, want a serving run directive")
	return gen.ExecutionDirective{}
}

func TestManagedServerAPIAndBindingReconcile(t *testing.T) {
	e, hook, path := newManagedEnv(t)
	statusPath := path + "/managed-server"
	if r := e.do("GET", statusPath, nil, e.token); r.Status != 200 || r.Body["phase"] != "stopped" {
		t.Fatalf("GET absent managed server = %d/%v, want 200/stopped", r.Status, r.Body["phase"])
	}
	if r := e.do("POST", statusPath+"/start", nil, e.token); r.Status != 400 {
		t.Errorf("start disabled plugin status = %d, want 400", r.Status)
	}
	if r := e.do("PUT", path, map[string]any{"settings": map[string]any{}}, e.token); r.Status != 400 {
		t.Errorf("enable without licence status = %d, want 400", r.Status)
	}
	if r := e.do("PUT", path, map[string]any{"settings": map[string]any{"licence": "noncommercial", "at_url": "https://override.example"}}, e.token); r.Status != 400 {
		t.Errorf("override managed setting status = %d, want 400", r.Status)
	}
	e.mustOK(e.do("PUT", path, map[string]any{"settings": map[string]any{"licence": "noncommercial"}}, e.token))
	for _, mode := range []any{nil, "", false, "unknown"} {
		if r := e.do("PUT", path, map[string]any{"settings": map[string]any{"hosting": mode}}, e.token); r.Status != 400 {
			t.Errorf("PUT hosting=%v status = %d, want 400", mode, r.Status)
		}
	}
	if got := hook.calls.Load(); got != 0 {
		t.Errorf("hook calls before server health = %d, want 0", got)
	}
	if r := e.do("POST", statusPath+"/start", nil, e.token); r.Status != 400 || r.Body["code"] != "group_pc_required" {
		t.Fatalf("start without PC = %d/%v, want 400/group_pc_required", r.Status, r.Body["code"])
	}
	a := e.newAgent("tournament-pc")
	a.heartbeatCaps(nil, tournamentCaps())
	started := e.mustOK(e.do("POST", statusPath+"/start", nil, e.token))
	sid := started["server_id"].(string)
	if again := e.mustOK(e.do("POST", statusPath+"/start", nil, e.token)); again["server_id"] != sid {
		t.Errorf("repeated start server = %v, want %s", again["server_id"], sid)
	}
	ed := managedDirective(t, a, sid)
	token, ok := (*ed.Config)["api_token"].(string)
	if !ok || len(token) < 16 {
		t.Fatal("managed directive lacks a generated API token of at least 16 characters")
	}
	row, err := e.st.ManagedPluginServer(t.Context(), e.group, "managed-test")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := e.st.GetServer(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row.SecretsJSON, token) || strings.Contains(srv.ConfigJSON, token) {
		t.Error("stored managed secret is plaintext, want encrypted outside server config")
	}
	view := e.do("GET", "/v1/servers/"+sid, nil, e.token)
	if bytes.Contains(view.Raw, []byte(token)) {
		t.Error("server API exposed managed secret, want redacted config")
	}
	if err := e.srv.ReconcileManagedServers(t.Context()); err != nil {
		t.Fatal(err)
	}
	if hook.calls.Load() != 0 {
		t.Error("settings hook called before healthy execution")
	}
	body, _ := json.Marshal(map[string]any{"server_id": sid, "epoch": ed.Epoch, "state": "running", "health": "ok", "join_code": "managed-setup-code"})
	e.mustOK(a.signedDo("POST", "/v1/agent/executions/"+ed.ExecutionId+"/status", body, 0, nil))
	e.srv.ManagedLeader = nonLeader{}
	if err := e.srv.ReconcileManagedServers(t.Context()); err != nil {
		t.Fatal(err)
	}
	if hook.calls.Load() != 0 {
		t.Error("settings hook called on a replica")
	}
	e.srv.ManagedLeader = nil
	if err := e.srv.ReconcileManagedServers(t.Context()); err != nil {
		t.Fatal(err)
	}
	ready := e.mustOK(e.do("GET", statusPath, nil, e.token))
	if ready["phase"] != "ready" || ready["setup_code"] != "managed-setup-code" || ready["node_name"] == "" {
		t.Errorf("managed status = %v, want ready, setup code and node name", ready)
	}
	if got := hook.calls.Load(); got != 1 {
		t.Errorf("healthy hook calls = %d, want 1", got)
	}
	hook.mu.Lock()
	if hook.token != token || hook.url != ready["public_url"] || hook.key == "" {
		t.Error("hook lacks runtime values or durable idempotency key")
	}
	hook.mu.Unlock()
	restarted := &api.Server{Store: e.st, Plugins: e.srv.Plugins, Secrets: e.srv.Secrets, PluginHTTP: e.srv.PluginHTTP, Cfg: e.srv.Cfg, Recon: e.recon}
	if err := restarted.ReconcileManagedServers(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := hook.calls.Load(); got != 1 {
		t.Errorf("hook calls after reconciler restart = %d, want 1", got)
	}
	group, err := e.st.GroupPlugin(t.Context(), e.group, "managed-test")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(group.SettingsJSON, token) || strings.Contains(group.SettingsJSON, "fleet-key-cache") {
		t.Error("plugin secrets or hidden hook values stored in plaintext")
	}
	e.srv.Cfg.Gateway.Domain = "changed.site"
	if pending := e.mustOK(e.do("GET", statusPath, nil, e.token)); pending["phase"] != "starting" {
		t.Errorf("unacknowledged URL status = %v, want starting until hook succeeds", pending)
	}
	hook.fail.Store(true)
	if err := e.srv.ReconcileManagedServers(t.Context()); err != nil {
		t.Fatal(err)
	}
	failed := e.mustOK(e.do("GET", statusPath, nil, e.token))
	if failed["phase"] != "failed" || !strings.Contains(failed["reason"].(string), "licence rejected") {
		t.Errorf("hook rejection status = %v, want failed with hook reason", failed)
	}
	if err := e.srv.ReconcileManagedServers(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := hook.calls.Load(); got != 2 {
		t.Errorf("hook calls during backoff = %d, want 2", got)
	}
	hook.fail.Store(false)
	e.clk.advance(time.Second.Milliseconds())
	if err := restarted.ReconcileManagedServers(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := hook.calls.Load(); got != 3 {
		t.Errorf("hook calls after retry = %d, want 3", got)
	}
	updated := e.mustOK(e.do("GET", statusPath, nil, e.token))
	if updated["phase"] != "ready" || !strings.HasSuffix(updated["public_url"].(string), ".changed.site") {
		t.Errorf("URL rewire status = %v, want ready on changed domain", updated)
	}
	rotated := "replacement-managed-token-32-characters"
	sealed, err := e.srv.Secrets.Seal(`{"api_token":"`+rotated+`"}`, "managed-server:"+e.group+"/managed-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.DB.Exec(`UPDATE managed_plugin_servers SET secrets_json=? WHERE server_id=?`, sealed, sid); err != nil {
		t.Fatal(err)
	}
	if pending := e.mustOK(e.do("GET", statusPath, nil, e.token)); pending["phase"] != "starting" {
		t.Errorf("unacknowledged secret status = %v, want starting until hook succeeds", pending)
	}
	if err := e.srv.ReconcileManagedServers(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := hook.calls.Load(); got != 4 {
		t.Errorf("hook calls after secret rotation = %d, want 4", got)
	}
	hook.mu.Lock()
	if hook.token != rotated {
		t.Error("hook did not receive rotated runtime secret")
	}
	hook.mu.Unlock()
	manual := e.mustOK(e.do("PUT", path, map[string]any{"settings": map[string]any{"hosting": "manual", "at_url": "https://manual.example", "at_token": "manual-token"}}, e.token))
	if manual["settings"].(map[string]any)["at_url"] != "https://manual.example" {
		t.Error("manual address is hidden, want editable public manual setting")
	}
	if r := e.do("POST", statusPath+"/start", nil, e.token); r.Status != 400 {
		t.Errorf("start in manual mode status = %d, want 400", r.Status)
	}
	if stopped := e.mustOK(e.do("POST", statusPath+"/stop", nil, e.token)); stopped["phase"] != "stopped" {
		t.Errorf("stop phase = %v, want stopped", stopped["phase"])
	}
	if _, err := e.st.DB.Exec(`UPDATE servers SET desired_state='running',observed_state='failed' WHERE id=?`, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.DB.Exec(`UPDATE server_executions SET ended_at=?,state='failed' WHERE server_id=?`, e.clk.ms, sid); err != nil {
		t.Fatal(err)
	}
	if status := e.mustOK(e.do("GET", statusPath, nil, e.token)); status["phase"] != "failed" {
		t.Errorf("failed execution status = %v, want failed without an active lease", status)
	}
}

func TestManagedServerAPIOwnerAdminOnly(t *testing.T) {
	e, _, path := newManagedEnv(t)
	token := e.signup("managed-member@example.test", "password123")
	u, err := e.st.UserByEmail(t.Context(), "managed-member@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.DB.Exec(`INSERT INTO group_members(group_id,user_id,role,created_at) VALUES(?,?,?,?)`, e.group, u.ID, "member", e.clk.ms); err != nil {
		t.Fatal(err)
	}
	serviceToken := mustServiceToken(t, e, "managed-test-token", []string{"servers", "events", "members"})
	for _, credential := range []string{token, serviceToken} {
		for _, req := range []struct{ method, suffix string }{{"GET", ""}, {"POST", "/start"}, {"POST", "/stop"}} {
			if r := e.do(req.method, path+"/managed-server"+req.suffix, nil, credential); r.Status != 403 {
				t.Errorf("non-admin %s %s status = %d, want 403", req.method, req.suffix, r.Status)
			}
		}
	}
}
