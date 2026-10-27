package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/grendalaget/varde/apps/controlplane/internal/outbound"
	"github.com/grendalaget/varde/apps/controlplane/internal/plugins"
	"github.com/grendalaget/varde/apps/controlplane/internal/secrets"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

func TestPluginSettingsListActionAndErrorMapping(t *testing.T) {
	e := newEnv(t)
	pluginSecret := strings.Repeat("plugin-secret-", 3)
	t.Setenv("FAKE_PLUGIN_SECRET", pluginSecret)
	var failStatus atomic.Int32
	var badHref atomic.Bool
	var slowList atomic.Bool
	var invalidStore atomic.Bool
	var mu sync.Mutex
	var lastOp, lastAuth string
	var lastBody map[string]any
	pluginServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("plugin request method = %s, want POST", r.Method)
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		lastOp, lastAuth, lastBody = r.Header.Get("X-Varde-Op"), r.Header.Get("Authorization"), body
		mu.Unlock()
		if status := int(failStatus.Load()); status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"plugin says no"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/varde/v1/settings":
			if invalidStore.Load() {
				_, _ = w.Write([]byte(`{"store":{"cached":"private-cache","mode":"one"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"store":{"cached":"private-cache"}}`))
		case "/varde/v1/disable":
			_, _ = w.Write([]byte(`{}`))
		case "/list":
			if slowList.Load() {
				time.Sleep(1100 * time.Millisecond)
			}
			_, _ = w.Write([]byte(`{"items":[{"title":"Current event","href":"https://example.test/event"}]}`))
		case "/run":
			if badHref.Load() {
				_, _ = w.Write([]byte(`{"message":"Action complete","href":"javascript:alert(1)"}`))
			} else {
				_, _ = w.Write([]byte(`{"message":"Action complete","href":"https://example.test/result"}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer pluginServer.Close()

	manifest := map[string]any{
		"id": "fake-plugin", "name": "Fake plugin", "description": "Plugin for UI testing",
		"version": "1", "service_token": "fake-plugin-token",
		"base_url": pluginServer.URL, "secret_env": "FAKE_PLUGIN_SECRET",
		"settings": []any{
			map[string]any{
				"name": "api_key", "type": "secret", "required": true,
				"label": "API token", "placeholder": "Paste token",
			},
			map[string]any{"name": "mode", "type": "select", "options": []string{"one", "two"}},
			map[string]any{"name": "cached", "type": "string", "hidden": true},
		},
		"pages": []any{map[string]any{
			"id": "dashboard", "title": "Dashboard", "list": map[string]any{"endpoint": "/list"},
			"actions": []any{map[string]any{
				"id": "run", "title": "Run", "endpoint": "/run",
				"fields": []any{
					map[string]any{"name": "members", "type": "member_multiselect", "requires": "steam_id", "required": true},
					map[string]any{"name": "modes", "type": "multiselect", "options": []string{"one", "two"}, "required": true},
				},
			}},
		}},
	}
	manifestData, _ := json.Marshal(manifest)
	manifestPath := filepath.Join(t.TempDir(), "plugin.json")
	if err := os.WriteFile(manifestPath, manifestData, 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := plugins.Load([]string{manifestPath})
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Plugins = registry
	e.srv.Secrets, err = secrets.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	e.srv.PluginHTTP = outbound.NewClient(time.Second)

	memberID := "plugin-member"
	if _, err := e.st.DB.Exec(`INSERT INTO users (id,email,display_name,password_hash,is_operator,created_at)
		VALUES(?,?,?,?,?,?)`, memberID, "plugin-member@example.test", "Plugin Member", "unused", 0, e.clk.ms); err != nil {
		t.Fatal(err)
	}
	memberSteamID := "76561198000000000"
	if err := e.st.SetUserSteam(t.Context(), memberID, &memberSteamID, nil, nil); err != nil {
		t.Fatal(err)
	}
	memberToken := e.signup("session-member@example.test", "password123")
	memberUser, err := e.st.UserByEmail(t.Context(), "session-member@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.DB.Exec(`INSERT INTO group_members(group_id,user_id,role,created_at) VALUES(?,?,?,?)`,
		e.group, memberID, "member", e.clk.ms); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.DB.Exec(`INSERT INTO group_members(group_id,user_id,role,created_at) VALUES(?,?,?,?)`,
		e.group, memberUser.ID, "member", e.clk.ms); err != nil {
		t.Fatal(err)
	}

	resp := e.do("GET", "/v1/plugins", nil, e.token)
	if resp.Status != 200 {
		t.Fatalf("GET /v1/plugins = %d %s, want 200", resp.Status, resp.Raw)
	}
	plugins := resp.Body["plugins"].([]any)
	pluginInfo := plugins[0].(map[string]any)
	if pluginInfo["description"] != "Plugin for UI testing" {
		t.Errorf("GET /v1/plugins description = %v, want manifest description", pluginInfo["description"])
	}
	for _, field := range pluginInfo["settings"].([]any) {
		info := field.(map[string]any)
		if info["name"] == "cached" {
			t.Fatal("GET /v1/plugins exposed hidden setting")
		}
		if info["name"] == "api_key" &&
			(info["label"] != "API token" || info["placeholder"] != "Paste token") {
			t.Errorf("GET /v1/plugins field metadata = %v, want label and placeholder", info)
		}
	}

	if r := e.do("PUT", "/v1/groups/"+e.group+"/plugins/fake-plugin",
		map[string]any{"settings": map[string]any{"api_key": "member", "mode": "one"}}, memberToken); r.Status != 403 {
		t.Errorf("member PUT plugin settings = %d, want 403", r.Status)
	}
	if _, err := e.st.DB.Exec(`UPDATE group_members SET role='admin' WHERE group_id=? AND user_id=?`, e.group, memberUser.ID); err != nil {
		t.Fatal(err)
	}
	if r := e.do("PUT", "/v1/groups/"+e.group+"/plugins/fake-plugin",
		map[string]any{"settings": map[string]any{"api_key": "admin-secret", "mode": "one"}}, memberToken); r.Status != 200 {
		t.Errorf("admin PUT plugin settings = %d %s, want 200", r.Status, r.Raw)
	}
	resp = e.do("PUT", "/v1/groups/"+e.group+"/plugins/fake-plugin",
		map[string]any{"settings": map[string]any{"api_key": "plaintext-api-key", "mode": "one"}}, e.token)
	if resp.Status != 200 {
		t.Fatalf("PUT group plugin = %d %s, want 200", resp.Status, resp.Raw)
	}
	row, err := e.st.GroupPlugin(t.Context(), e.group, "fake-plugin")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row.SettingsJSON, "plaintext-api-key") || strings.Contains(row.SettingsJSON, "private-cache") {
		t.Fatalf("group plugin settings stored plaintext: %s", row.SettingsJSON)
	}
	mu.Lock()
	settingsOp := lastOp
	settingsEnvelope := lastBody["settings"].(map[string]any)
	mu.Unlock()
	if settingsOp != "settings" || settingsEnvelope["api_key"] != "plaintext-api-key" {
		t.Errorf("settings hook operation/envelope = %q/%v", settingsOp, settingsEnvelope)
	}

	for _, input := range []map[string]any{
		{"settings": map[string]any{"api_key": "value", "mode": "one", "unknown": "x"}},
		{"settings": map[string]any{"api_key": "value", "mode": "one", "cached": "user"}},
	} {
		if r := e.do("PUT", "/v1/groups/"+e.group+"/plugins/fake-plugin", input, e.token); r.Status != 400 {
			t.Errorf("PUT invalid settings %v = %d, want 400", input, r.Status)
		}
	}
	invalidStore.Store(true)
	if r := e.do("PUT", "/v1/groups/"+e.group+"/plugins/fake-plugin",
		map[string]any{"settings": map[string]any{"api_key": "rejected", "mode": "one"}}, e.token); r.Status != 502 {
		t.Errorf("settings hook storing non-hidden key = %d, want 502", r.Status)
	}
	invalidStore.Store(false)
	if r := e.do("PUT", "/v1/groups/"+e.group+"/plugins/fake-plugin",
		map[string]any{"settings": map[string]any{"api_key": "", "mode": "two"}}, e.token); r.Status != 200 {
		t.Errorf("blank secret update = %d, want 200", r.Status)
	}
	mu.Lock()
	preservedSecret := lastBody["settings"].(map[string]any)["api_key"]
	mu.Unlock()
	if preservedSecret != "plaintext-api-key" {
		t.Errorf("settings hook secret = %v, want stored secret preserved", preservedSecret)
	}

	resp = e.do("GET", "/v1/groups/"+e.group+"/plugins", nil, e.token)
	if resp.Status != 200 {
		t.Fatalf("GET group plugins = %d %s, want 200", resp.Status, resp.Raw)
	}
	pluginSettings := resp.Body["plugins"].([]any)[0].(map[string]any)
	if pluginSettings["settings"].(map[string]any)["api_key"] != nil {
		t.Errorf("GET group plugins returned secret value: %v", pluginSettings["settings"])
	}
	if len(pluginSettings["secrets_set"].([]any)) != 1 || pluginSettings["secrets_set"].([]any)[0] != "api_key" {
		t.Errorf("GET group plugins secrets_set = %v, want [api_key]", pluginSettings["secrets_set"])
	}
	if _, ok := pluginSettings["settings"].(map[string]any)["cached"]; ok {
		t.Errorf("GET group plugins exposed hidden setting: %v", pluginSettings["settings"])
	}

	resp = e.do("POST", "/v1/groups/"+e.group+"/plugins/fake-plugin/pages/dashboard/list", nil, e.token)
	if resp.Status != 200 {
		t.Fatalf("POST plugin list = %d %s, want 200", resp.Status, resp.Raw)
	}
	mu.Lock()
	listOp, listEnvelope := lastOp, lastBody
	mu.Unlock()
	if listOp != "list" {
		t.Errorf("list X-Varde-Op = %q, want list", listOp)
	}
	if _, ok := listEnvelope["values"]; ok {
		t.Errorf("list plugin envelope contains values: %v", listEnvelope)
	}
	if r := e.do("POST", "/v1/groups/"+e.group+"/plugins/fake-plugin/pages/missing/list", nil, e.token); r.Status != 404 {
		t.Errorf("unknown plugin page = %d, want 404", r.Status)
	}
	if r := e.do("POST", "/v1/groups/"+e.group+"/plugins/fake-plugin/pages/dashboard/actions/missing", map[string]any{"values": map[string]any{}}, e.token); r.Status != 404 {
		t.Errorf("unknown plugin action = %d, want 404", r.Status)
	}
	resp = e.do("POST", "/v1/groups/"+e.group+"/plugins/fake-plugin/pages/dashboard/actions/run",
		map[string]any{"values": map[string]any{"members": []string{memberID}, "modes": []string{"one"}}}, e.token)
	if resp.Status != 200 {
		t.Fatalf("POST plugin action = %d %s, want 200", resp.Status, resp.Raw)
	}
	if notes, ok := resp.Body["notes"].([]any); !ok || len(notes) != 0 {
		t.Errorf("POST plugin action notes = %v, want empty array for omitted notes", resp.Body["notes"])
	}
	mu.Lock()
	op, authHeader, envelope := lastOp, lastAuth, lastBody
	mu.Unlock()
	if op != "action:run" {
		t.Errorf("X-Varde-Op = %q, want action:run", op)
	}
	if !strings.HasPrefix(authHeader, "Bearer ") {
		t.Fatalf("plugin Authorization = %q, want bearer JWT", authHeader)
	}
	parsed, err := jwt.Parse(strings.TrimPrefix(authHeader, "Bearer "), func(token *jwt.Token) (any, error) {
		return []byte(pluginSecret), nil
	})
	if err != nil || !parsed.Valid {
		t.Fatalf("plugin JWT parse = %v, valid=%v", err, parsed != nil && parsed.Valid)
	}
	claims := parsed.Claims.(jwt.MapClaims)
	if claims["iss"] != "varde-control-plane" || claims["aud"] != "fake-plugin" ||
		claims["sub"] != e.do("GET", "/v1/me", nil, e.token).Body["user"].(map[string]any)["id"] ||
		claims["grp"] != e.group || claims["role"] != "owner" || claims["jti"] == "" ||
		claims["exp"].(float64)-claims["iat"].(float64) != 60 {
		t.Errorf("plugin JWT claims = %v, missing required values", claims)
	}
	settings := envelope["settings"].(map[string]any)
	if settings["api_key"] != "plaintext-api-key" || settings["cached"] != "private-cache" {
		t.Errorf("plugin action settings = %v, want decrypted secret and hidden value", settings)
	}
	values := envelope["values"].(map[string]any)
	expanded := values["members"].([]any)[0].(map[string]any)
	if expanded["steam_id"] != "76561198000000000" || expanded["display_name"] != "Plugin Member" || expanded["user_id"] != memberID {
		t.Errorf("expanded plugin member = %v, want user profile and Steam ID", expanded)
	}
	for _, input := range []map[string]any{
		{"values": map[string]any{"members": []string{memberID}, "modes": []string{}}},
		{"values": map[string]any{"members": []string{"not-a-member"}, "modes": []string{"one"}}},
	} {
		if r := e.do("POST", "/v1/groups/"+e.group+"/plugins/fake-plugin/pages/dashboard/actions/run", input, e.token); r.Status != 400 {
			t.Errorf("invalid action values %v = %d, want 400", input, r.Status)
		}
	}
	noSteamID := "plugin-no-steam"
	if _, err := e.st.DB.Exec(`INSERT INTO users (id,email,display_name,password_hash,is_operator,created_at) VALUES(?,?,?,?,?,?)`,
		noSteamID, "no-steam@example.test", "No Steam", "unused", 0, e.clk.ms); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.DB.Exec(`INSERT INTO group_members(group_id,user_id,role,created_at) VALUES(?,?,?,?)`,
		e.group, noSteamID, "member", e.clk.ms); err != nil {
		t.Fatal(err)
	}
	if r := e.do("POST", "/v1/groups/"+e.group+"/plugins/fake-plugin/pages/dashboard/actions/run",
		map[string]any{"values": map[string]any{"members": []string{noSteamID}, "modes": []string{"one"}}}, e.token); r.Status != 400 ||
		!strings.Contains(r.Body["message"].(string), "No Steam") {
		t.Errorf("member missing required Steam ID = %d %s, want 400 naming member", r.Status, r.Raw)
	}
	badHref.Store(true)
	if r := e.do("POST", "/v1/groups/"+e.group+"/plugins/fake-plugin/pages/dashboard/actions/run",
		map[string]any{"values": map[string]any{"members": []string{memberID}, "modes": []string{"one"}}}, e.token); r.Status != 502 {
		t.Errorf("invalid plugin href = %d, want 502", r.Status)
	}
	badHref.Store(false)
	pluginToken := mustServiceToken(t, e, "fake-plugin-token", []string{"servers", "events", "members"})
	for _, req := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/v1/groups/" + e.group + "/plugins", nil},
		{"PUT", "/v1/groups/" + e.group + "/plugins/fake-plugin", map[string]any{"settings": map[string]any{}}},
		{"DELETE", "/v1/groups/" + e.group + "/plugins/fake-plugin", nil},
		{"POST", "/v1/groups/" + e.group + "/plugins/fake-plugin/pages/dashboard/list", nil},
		{"POST", "/v1/groups/" + e.group + "/plugins/fake-plugin/pages/dashboard/actions/run", map[string]any{"values": map[string]any{}}},
	} {
		if r := e.do(req.method, req.path, req.body, pluginToken); r.Status != 403 {
			t.Errorf("service token %s %s = %d, want 403", req.method, req.path, r.Status)
		}
	}
	if r := e.do("GET", "/v1/groups/"+e.group+"/members", nil, pluginToken); r.Status != 200 {
		t.Errorf("enabled plugin service token members = %d, want 200", r.Status)
	} else if bytes.Contains(r.Raw, []byte("steam_id")) {
		t.Errorf("members scope exposed Steam IDs without steam_ids scope: %s", r.Raw)
	}
	for _, path := range []string{"servers", "events", "members"} {
		if r := e.do("GET", "/v1/groups/"+e.group+"/"+path, nil, pluginToken); r.Status != 200 {
			t.Errorf("enabled plugin service token GET %s = %d %s, want 200", path, r.Status, r.Raw)
		}
	}
	otherGroup := e.createGroup("unbound")
	if r := e.do("GET", "/v1/groups/"+otherGroup+"/members", nil, pluginToken); r.Status != 403 {
		t.Errorf("plugin service token without group binding = %d, want 403", r.Status)
	}
	steamToken := mustServiceToken(t, e, "steam-reader", []string{"members", "steam_ids"})
	groupIDs, _ := json.Marshal([]string{e.group})
	if _, err := e.st.DB.Exec(`UPDATE service_tokens SET group_ids=? WHERE name=?`, string(groupIDs), "steam-reader"); err != nil {
		t.Fatal(err)
	}
	if r := e.do("GET", "/v1/groups/"+e.group+"/members", nil, steamToken); r.Status != 200 || !bytes.Contains(r.Raw, []byte("steam_id")) {
		t.Errorf("steam_ids scope response = %d %s, want Steam IDs", r.Status, r.Raw)
	}

	failStatus.Store(http.StatusBadRequest)
	resp = e.do("POST", "/v1/groups/"+e.group+"/plugins/fake-plugin/pages/dashboard/list", nil, e.token)
	if resp.Status != 400 || resp.Body["message"] != "plugin says no" {
		t.Errorf("plugin 4xx mapping = %d %v, want 400 plugin message", resp.Status, resp.Body)
	}
	failStatus.Store(http.StatusInternalServerError)
	resp = e.do("POST", "/v1/groups/"+e.group+"/plugins/fake-plugin/pages/dashboard/list", nil, e.token)
	if resp.Status != 502 || resp.Body["code"] != "plugin_unavailable" || resp.Body["message"] != "Fake plugin is unavailable" {
		t.Errorf("plugin 5xx mapping = %d %v, want generic 502", resp.Status, resp.Body)
	}
	failStatus.Store(0)
	slowList.Store(true)
	if r := e.do("POST", "/v1/groups/"+e.group+"/plugins/fake-plugin/pages/dashboard/list", nil, e.token); r.Status != 502 ||
		r.Body["code"] != "plugin_unavailable" {
		t.Errorf("plugin timeout mapping = %d %s, want 502 plugin_unavailable", r.Status, r.Raw)
	}
	slowList.Store(false)
	resp = e.do("DELETE", "/v1/groups/"+e.group+"/plugins/fake-plugin", nil, e.token)
	if resp.Status != 204 {
		t.Fatalf("DELETE group plugin = %d %s, want 204", resp.Status, resp.Raw)
	}
	mu.Lock()
	disableOp := lastOp
	mu.Unlock()
	if disableOp != "disable" {
		t.Errorf("disable X-Varde-Op = %q, want disable", disableOp)
	}
	configuredSecrets := e.srv.Secrets
	e.srv.Secrets = nil
	if r := e.do("GET", "/v1/groups/"+e.group+"/plugins", nil, e.token); r.Status != 503 || r.Body["code"] != "secrets_unconfigured" {
		t.Errorf("plugin with unconfigured secrets = %d %s, want 503 secrets_unconfigured", r.Status, r.Raw)
	}
	e.srv.Secrets = configuredSecrets
	resp = e.do("POST", "/v1/groups/"+e.group+"/plugins/fake-plugin/pages/dashboard/list", nil, e.token)
	if resp.Status != 404 || resp.Body["message"] != "plugin not enabled" {
		t.Errorf("disabled plugin list = %d %v, want 404 plugin not enabled", resp.Status, resp.Body)
	}
	failStatus.Store(http.StatusBadRequest)
	if r := e.do("PUT", "/v1/groups/"+e.group+"/plugins/fake-plugin",
		map[string]any{"settings": map[string]any{"api_key": "bad", "mode": "one"}}, e.token); r.Status != 400 ||
		r.Body["message"] != "plugin says no" {
		t.Errorf("plugin 4xx settings PUT = %d %s, want 400 with plugin message", r.Status, r.Raw)
	}
	var stored int
	if err := e.st.DB.Get(&stored, `SELECT COUNT(*) FROM group_plugins WHERE group_id=? AND plugin_id=?`, e.group, "fake-plugin"); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Errorf("failed settings PUT stored %d plugin rows, want 0", stored)
	}
	failStatus.Store(http.StatusBadGateway)
	if r := e.do("PUT", "/v1/groups/"+e.group+"/plugins/fake-plugin",
		map[string]any{"settings": map[string]any{"api_key": "bad", "mode": "one"}}, e.token); r.Status != http.StatusBadGateway ||
		r.Body["code"] != "plugin_unavailable" || r.Body["message"] != "plugin says no" {
		t.Errorf("plugin 5xx settings PUT = %d %s, want plugin message", r.Status, r.Raw)
	}
	failStatus.Store(0)
}

func TestPluginSettingsAcceptOptionalFalseBoolAndMissingInt(t *testing.T) {
	e := newEnv(t)
	t.Setenv("OPTIONAL_PLUGIN_SECRET", strings.Repeat("optional-secret-", 3))
	pluginServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer pluginServer.Close()
	manifest := map[string]any{
		"id": "optional-plugin", "name": "Optional plugin", "version": "1",
		"base_url": pluginServer.URL, "secret_env": "OPTIONAL_PLUGIN_SECRET",
		"settings": []any{
			map[string]any{"name": "enabled", "type": "bool"},
			map[string]any{"name": "count", "type": "int"},
		},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "optional-plugin.json")
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
	e.srv.PluginHTTP = outbound.NewClient(time.Second)

	response := e.do(http.MethodPut, "/v1/groups/"+e.group+"/plugins/optional-plugin",
		map[string]any{"settings": map[string]any{"enabled": false}}, e.token)
	if response.Status != http.StatusOK {
		t.Fatalf("PUT optional plugin settings = %d %s, want 200", response.Status, response.Raw)
	}
	record, err := e.st.GroupPlugin(t.Context(), e.group, "optional-plugin")
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal([]byte(record.SettingsJSON), &stored); err != nil {
		t.Fatal(err)
	}
	if stored["enabled"] != false {
		t.Errorf("stored optional bool = %v, want false", stored["enabled"])
	}
	if _, exists := stored["count"]; exists {
		t.Errorf("stored optional int = %v, want omitted", stored["count"])
	}
}

func TestEmptyPluginCatalogDoesNotNeedSecrets(t *testing.T) {
	e := newEnv(t)
	e.srv.Plugins = nil
	e.srv.Secrets = nil
	r := e.do("GET", "/v1/groups/"+e.group+"/plugins", nil, e.token)
	if r.Status != 200 || len(r.Body["plugins"].([]any)) != 0 {
		t.Fatalf("empty plugin catalog response = %d %s, want 200 and []", r.Status, r.Raw)
	}
}

func TestDeleteGroupRemovesPluginRows(t *testing.T) {
	e := newEnv(t)
	if err := e.st.UpsertGroupPlugin(t.Context(), store.GroupPlugin{
		GroupID: e.group, PluginID: "deleted-plugin", SettingsJSON: `{}`, EnabledBy: "user",
		CreatedAt: e.clk.ms, UpdatedAt: e.clk.ms,
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.DeleteGroup(t.Context(), e.group); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := e.st.DB.Get(&count, `SELECT COUNT(*) FROM group_plugins WHERE group_id=?`, e.group); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("group_plugins after DeleteGroup = %d rows, want 0", count)
	}
}
