package api_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grendalaget/varde/apps/controlplane/internal/api/gen"
	"github.com/grendalaget/varde/apps/controlplane/internal/plugins"
	"github.com/grendalaget/varde/apps/controlplane/internal/secrets"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

func TestServerAddonSpecValidation(t *testing.T) {
	e, serverID, token := newServerAddonEnv(t)
	tests := []struct {
		name   string
		mutate func(map[string]any)
		dev    bool
		want   int
	}{
		{name: "valid", mutate: func(map[string]any) {}, want: 204},
		{name: "loopback HTTP", mutate: func(body map[string]any) { body["url"] = "http://127.0.0.1/addon.zip" }, want: 204},
		{name: "development HTTP", mutate: func(body map[string]any) { body["url"] = "http://example.test/addon.zip" }, dev: true, want: 204},
		{name: "non-HTTP scheme", mutate: func(body map[string]any) { body["url"] = "ftp://example.test/addon.zip" }, want: 400},
		{name: "external HTTP", mutate: func(body map[string]any) { body["url"] = "http://example.test/addon.zip" }, want: 400},
		{name: "invalid SHA", mutate: func(body map[string]any) { body["sha256"] = strings.Repeat("A", 64) }, want: 400},
		{name: "invalid archive", mutate: func(body map[string]any) { body["archive"] = "tgz" }, want: 400},
		{name: "invalid target", mutate: func(body map[string]any) { body["target"] = "Server" }, want: 400},
		{name: "driver options size", mutate: func(body map[string]any) { body["driver_options"] = map[string]any{"large": strings.Repeat("x", 4096)} }, want: 400},
		{name: "too many files", mutate: func(body map[string]any) {
			files := make(map[string]string, 33)
			for i := range 33 {
				files[strings.Repeat("a", i+1)] = ""
			}
			body["files"] = files
		}, want: 400},
		{name: "file too large", mutate: func(body map[string]any) { body["files"] = map[string]string{"cfg/x": strings.Repeat("x", 64<<10+1)} }, want: 400},
		{name: "empty file key", mutate: func(body map[string]any) { body["files"] = map[string]string{"": "x"} }, want: 400},
		{name: "absolute file key", mutate: func(body map[string]any) { body["files"] = map[string]string{"/cfg/x": "x"} }, want: 400},
		{name: "parent file key", mutate: func(body map[string]any) { body["files"] = map[string]string{"cfg/../x": "x"} }, want: 400},
		{name: "dot file key segment", mutate: func(body map[string]any) { body["files"] = map[string]string{"cfg/./server.cfg": "x"} }, want: 400},
		{name: "dot file key prefix", mutate: func(body map[string]any) { body["files"] = map[string]string{"./x": "x"} }, want: 400},
		{name: "backslash file key", mutate: func(body map[string]any) { body["files"] = map[string]string{`cfg\\x`: "x"} }, want: 400},
		{name: "colon file key", mutate: func(body map[string]any) { body["files"] = map[string]string{"cfg:x": "x"} }, want: 400},
		{name: "empty path segment", mutate: func(body map[string]any) { body["files"] = map[string]string{"cfg//x": "x"} }, want: 400},
		{name: "missing driver options", mutate: func(body map[string]any) { body["driver_options"] = nil }, want: 400},
		{name: "missing files", mutate: func(body map[string]any) { body["files"] = nil }, want: 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.srv.Cfg.Dev = tt.dev
			body := validServerAddonBody()
			tt.mutate(body)
			resp := e.do("PUT", serverAddonPath(serverID), body, token)
			if resp.Status != tt.want {
				t.Errorf("PUT server addon (%s) = %d %s, want %d", tt.name, resp.Status, resp.Raw, tt.want)
			}
		})
	}
}

func TestServerAddonAuthorizationAndMetadata(t *testing.T) {
	e, serverID, pluginToken := newServerAddonEnv(t)
	wrongToken := boundServiceToken(t, e, "other-plugin-token", []string{"server_addons"}, e.group)
	noScopeToken := boundServiceToken(t, e, "missing-scope-token", []string{"servers"}, e.group)
	for _, test := range []struct {
		name  string
		token string
	}{
		{name: "wrong plugin token", token: wrongToken},
		{name: "missing scope", token: noScopeToken},
	} {
		t.Run(test.name, func(t *testing.T) {
			resp := e.do("PUT", serverAddonPath(serverID), validServerAddonBody(), test.token)
			if resp.Status != 403 {
				t.Errorf("PUT server addon with %s = %d %s, want 403", test.name, resp.Status, resp.Raw)
			}
		})
	}
	if _, err := e.st.DB.Exec(`UPDATE service_tokens SET group_ids=? WHERE name=?`, `["other-group"]`, "addon-plugin-token"); err != nil {
		t.Fatal(err)
	}
	resp := e.do("PUT", serverAddonPath(serverID), validServerAddonBody(), pluginToken)
	if resp.Status != 403 {
		t.Errorf("PUT with token bound to another group = %d %s, want 403", resp.Status, resp.Raw)
	}
	if _, err := e.st.DB.Exec(`UPDATE service_tokens SET group_ids=? WHERE name=?`, `["`+e.group+`"]`, "addon-plugin-token"); err != nil {
		t.Fatal(err)
	}
	if resp := e.do("PUT", serverAddonPath(serverID), validServerAddonBody(), e.token); resp.Status != 403 {
		t.Errorf("user PUT server addon = %d %s, want 403", resp.Status, resp.Raw)
	}
	if resp := e.do("PUT", serverAddonPath(serverID), validServerAddonBody(), pluginToken); resp.Status != 204 {
		t.Fatalf("plugin PUT server addon = %d %s, want 204", resp.Status, resp.Raw)
	}
	list := e.do("GET", "/v1/servers/"+serverID+"/addons", nil, e.token)
	if list.Status != 200 {
		t.Fatalf("GET server addons = %d %s, want 200", list.Status, list.Raw)
	}
	addons := list.Body["addons"].([]any)
	if len(addons) != 1 {
		t.Fatalf("GET server addons returned %d entries, want 1", len(addons))
	}
	entry := addons[0].(map[string]any)
	if entry["plugin_id"] != "addon-plugin" || entry["addon_id"] != "test-addon" ||
		entry["url"] != "https://example.test/addon.zip" || entry["sha256"] != strings.Repeat("a", 64) {
		t.Errorf("GET addon summary = %v, want stored metadata", entry)
	}
	if _, ok := entry["files"]; ok {
		t.Errorf("GET addon exposed file contents: %v", entry)
	}
	if _, ok := entry["driver_options"]; ok {
		t.Errorf("GET addon exposed driver options: %v", entry)
	}
	if got := entry["file_names"].([]any); len(got) != 1 || got[0] != "cfg/server.cfg" {
		t.Errorf("GET addon file_names = %v, want [cfg/server.cfg]", got)
	}

	rows, err := e.st.ListServerAddons(t.Context(), serverID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListServerAddons() = %d rows, %v; want one row", len(rows), err)
	}
	if rows[0].Target != "server" {
		t.Errorf("ListServerAddons().Target = %q, want server", rows[0].Target)
	}
	if strings.Contains(rows[0].Spec, "private addon text") {
		t.Fatal("server addon spec was stored in plaintext")
	}
	plaintext, err := e.srv.Secrets.Open(rows[0].Spec, e.group+"/addon:"+serverID+"/addon-plugin/test-addon")
	if err != nil {
		t.Fatalf("Open(sealed spec) error = %v", err)
	}
	var stored gen.ServerAddonSpec
	if err := json.Unmarshal([]byte(plaintext), &stored); err != nil {
		t.Fatalf("unmarshal unsealed spec: %v", err)
	}
	if stored.Files["cfg/server.cfg"] != "private addon text" {
		t.Errorf("unsealed file content = %q, want private addon text", stored.Files["cfg/server.cfg"])
	}
	if resp := e.do("DELETE", serverAddonPath(serverID), nil, e.token); resp.Status != 204 {
		t.Errorf("admin DELETE server addon = %d %s, want 204", resp.Status, resp.Raw)
	}
}

func TestServerAddonRequiresSupportedCatalogTarget(t *testing.T) {
	e, _, pluginToken := newServerAddonEnv(t)
	serverID := e.createServer("minecraft", "minecraft addon target", map[string]any{})
	resp := e.do("PUT", serverAddonPath(serverID), validServerAddonBody(), pluginToken)
	if resp.Status != 400 {
		t.Fatalf("PUT addon for minecraft = %d %s, want 400", resp.Status, resp.Raw)
	}
}

func TestCatalogExposesAddonTargets(t *testing.T) {
	e := newEnv(t)
	resp := e.do("GET", "/v1/games", nil, e.token)
	if resp.Status != 200 {
		t.Fatalf("GET /v1/games = %d %s, want 200", resp.Status, resp.Raw)
	}
	games := resp.Body["games"].([]any)
	wantByID := map[string][]string{
		"testgame": {"server"},
		"cs2":      {"game"},
	}
	wantOSByID := map[string]map[string][]string{
		"cs2": {"game": {"linux"}},
	}
	found := make(map[string]bool, len(wantByID))
	for _, item := range games {
		game := item.(map[string]any)
		id := game["id"].(string)
		want, ok := wantByID[id]
		wantOS, wantOSFound := wantOSByID[id]
		gotOS, gotOSFound := game["addon_target_os"]
		if wantOSFound {
			if !gotOSFound {
				t.Errorf("%s missing addon_target_os, want %v", id, wantOS)
			} else if got, ok := gotOS.(map[string]any); !ok {
				t.Errorf("%s addon_target_os = %T, want map %v", id, gotOS, wantOS)
			} else if oses, ok := got["game"].([]any); !ok || len(oses) != 1 || oses[0] != "linux" {
				t.Errorf("%s addon_target_os = %v, want %v", id, gotOS, wantOS)
			}
		} else if gotOSFound {
			t.Errorf("game %q unexpectedly has addon_target_os: %v", id, gotOS)
		}
		if !ok {
			if _, ok := game["addon_targets"]; ok {
				t.Errorf("game %q unexpectedly has addon_targets: %v", id, game["addon_targets"])
			}
			continue
		}
		found[id] = true
		targets, ok := game["addon_targets"].([]any)
		if !ok || len(targets) != len(want) {
			t.Errorf("%s addon_targets = %v, want %v", id, game["addon_targets"], want)
			continue
		}
		for i, target := range want {
			if targets[i] != target {
				t.Errorf("%s addon_targets = %v, want %v", id, targets, want)
				break
			}
		}
	}
	for id := range wantByID {
		if !found[id] {
			t.Errorf("catalog response did not include %s", id)
		}
	}
}

func TestServerAddonDisabledPluginAndSecretsConfiguration(t *testing.T) {
	e, serverID, pluginToken := newServerAddonEnv(t)
	if err := e.st.UpsertServerAddon(t.Context(), store.ServerAddon{
		ServerID: serverID, PluginID: "addon-plugin", AddonID: "test-addon",
		Spec: "sealed", CreatedAt: e.clk.ms, UpdatedAt: e.clk.ms,
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.DeleteGroupPlugin(t.Context(), e.group, "addon-plugin"); err != nil {
		t.Fatal(err)
	}
	rows, err := e.st.ListServerAddons(t.Context(), serverID)
	if err != nil || len(rows) != 0 {
		t.Errorf("DeleteGroupPlugin left %d server addons, error %v; want 0", len(rows), err)
	}
	resp := e.do("PUT", serverAddonPath(serverID), validServerAddonBody(), pluginToken)
	if resp.Status != 403 {
		t.Errorf("PUT with disabled plugin = %d %s, want 403", resp.Status, resp.Raw)
	}
	if err := e.st.UpsertServerAddon(t.Context(), store.ServerAddon{
		ServerID: serverID, PluginID: "addon-plugin", AddonID: "test-addon",
		Spec: "sealed", CreatedAt: e.clk.ms, UpdatedAt: e.clk.ms,
	}); err != nil {
		t.Fatal(err)
	}
	if resp := e.do("DELETE", serverAddonPath(serverID), nil, e.token); resp.Status != 204 {
		t.Errorf("admin DELETE with disabled plugin = %d %s, want 204", resp.Status, resp.Raw)
	}

	if err := e.st.UpsertGroupPlugin(t.Context(), store.GroupPlugin{
		GroupID: e.group, PluginID: "addon-plugin", SettingsJSON: "{}", EnabledBy: "test",
		CreatedAt: e.clk.ms, UpdatedAt: e.clk.ms,
	}); err != nil {
		t.Fatal(err)
	}
	e.srv.Secrets = nil
	resp = e.do("PUT", serverAddonPath(serverID), validServerAddonBody(), pluginToken)
	if resp.Status != 503 || resp.Body["code"] != "secrets_unconfigured" {
		t.Errorf("PUT with unconfigured secrets = %d %s, want 503 secrets_unconfigured", resp.Status, resp.Raw)
	}
}

func TestServerAddonDeleteServerCleansRows(t *testing.T) {
	e, serverID, _ := newServerAddonEnv(t)
	if err := e.st.UpsertServerAddon(t.Context(), store.ServerAddon{
		ServerID: serverID, PluginID: "addon-plugin", AddonID: "test-addon",
		Spec: "sealed", CreatedAt: e.clk.ms, UpdatedAt: e.clk.ms,
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.DeleteServer(t.Context(), serverID, e.clk.ms); err != nil {
		t.Fatal(err)
	}
	rows, err := e.st.ListServerAddons(t.Context(), serverID)
	if err != nil || len(rows) != 0 {
		t.Errorf("DeleteServer left %d server addons, error %v; want 0", len(rows), err)
	}
}

func TestServerAddonPutBodyLimit(t *testing.T) {
	e, serverID, pluginToken := newServerAddonEnv(t)
	body := validServerAddonBody()
	body["files"] = map[string]string{"cfg/server.cfg": strings.Repeat("x", 2600<<10)}
	resp := e.do("PUT", serverAddonPath(serverID), body, pluginToken)
	if resp.Status != 413 {
		t.Fatalf("oversized PUT server addon = %d, want 413", resp.Status)
	}
}

func newServerAddonEnv(t *testing.T) (*env, string, string) {
	t.Helper()
	e := newEnv(t)
	t.Setenv("ADDON_PLUGIN_SECRET", strings.Repeat("s", 40))
	manifest := map[string]any{
		"id": "addon-plugin", "name": "Addon plugin", "version": "1",
		"base_url": "http://127.0.0.1:7301", "secret_env": "ADDON_PLUGIN_SECRET",
		"service_token": "addon-plugin-token",
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "plugin.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := plugins.Load([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Plugins = registry
	e.srv.Secrets, err = secrets.New(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.UpsertGroupPlugin(t.Context(), store.GroupPlugin{
		GroupID: e.group, PluginID: "addon-plugin", SettingsJSON: "{}", EnabledBy: "test",
		CreatedAt: e.clk.ms, UpdatedAt: e.clk.ms,
	}); err != nil {
		t.Fatal(err)
	}
	serverID := e.createServer("testgame", "addon test server", nil)
	token := boundServiceToken(t, e, "addon-plugin-token", []string{"server_addons", "servers"}, e.group)
	return e, serverID, token
}

func boundServiceToken(t *testing.T, e *env, name string, scopes []string, groupID string) string {
	t.Helper()
	token := mustServiceToken(t, e, name, scopes)
	groups, err := json.Marshal([]string{groupID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.DB.Exec(`UPDATE service_tokens SET group_ids=? WHERE name=?`, string(groups), name); err != nil {
		t.Fatal(err)
	}
	return token
}

func serverAddonPath(serverID string) string {
	return "/v1/servers/" + serverID + "/addons/addon-plugin/test-addon"
}

func validServerAddonBody() map[string]any {
	return map[string]any{
		"url": "https://example.test/addon.zip", "sha256": strings.Repeat("a", 64),
		"archive": "zip", "target": "server", "driver_options": map[string]any{},
		"files": map[string]string{"cfg/server.cfg": "private addon text"},
	}
}
