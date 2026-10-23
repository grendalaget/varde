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
)

func TestPluginSettingsListActionAndErrorMapping(t *testing.T) {
	e := newEnv(t)
	pluginSecret := strings.Repeat("plugin-secret-", 3)
	t.Setenv("FAKE_PLUGIN_SECRET", pluginSecret)
	var failStatus atomic.Int32
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
			_, _ = w.Write([]byte(`{"store":{"cached":"private-cache"}}`))
		case "/varde/v1/disable":
			_, _ = w.Write([]byte(`{}`))
		case "/list":
			_, _ = w.Write([]byte(`{"items":[{"title":"Current event","href":"https://example.test/event"}]}`))
		case "/run":
			_, _ = w.Write([]byte(`{"message":"Action complete","notes":["Started"],"href":"https://example.test/result"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer pluginServer.Close()

	manifest := map[string]any{
		"id": "fake-plugin", "name": "Fake plugin", "version": "1",
		"base_url": pluginServer.URL, "secret_env": "FAKE_PLUGIN_SECRET",
		"settings": []any{
			map[string]any{"name": "api_key", "type": "secret", "required": true},
			map[string]any{"name": "mode", "type": "select", "options": []string{"one", "two"}},
			map[string]any{"name": "cached", "type": "string", "hidden": true},
		},
		"pages": []any{map[string]any{
			"id": "dashboard", "title": "Dashboard", "list": map[string]any{"endpoint": "/list"},
			"actions": []any{map[string]any{
				"id": "run", "title": "Run", "endpoint": "/run",
				"fields": []any{map[string]any{"name": "members", "type": "member_multiselect", "requires": "steam_id", "required": true}},
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
	if _, err := e.st.DB.Exec(`INSERT INTO users (id,email,display_name,password_hash,is_operator,steam_id,created_at)
		VALUES(?,?,?,?,?,?,?)`, memberID, "plugin-member@example.test", "Plugin Member", "unused", 0, "76561198000000000", e.clk.ms); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.DB.Exec(`INSERT INTO group_members(group_id,user_id,role,created_at) VALUES(?,?,?,?)`,
		e.group, memberID, "member", e.clk.ms); err != nil {
		t.Fatal(err)
	}

	resp := e.do("GET", "/v1/plugins", nil, e.token)
	if resp.Status != 200 {
		t.Fatalf("GET /v1/plugins = %d %s, want 200", resp.Status, resp.Raw)
	}
	plugins := resp.Body["plugins"].([]any)
	pluginInfo := plugins[0].(map[string]any)
	for _, field := range pluginInfo["settings"].([]any) {
		if field.(map[string]any)["name"] == "cached" {
			t.Fatal("GET /v1/plugins exposed hidden setting")
		}
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

	resp = e.do("POST", "/v1/groups/"+e.group+"/plugins/fake-plugin/pages/dashboard/list", nil, e.token)
	if resp.Status != 200 {
		t.Fatalf("POST plugin list = %d %s, want 200", resp.Status, resp.Raw)
	}
	resp = e.do("POST", "/v1/groups/"+e.group+"/plugins/fake-plugin/pages/dashboard/actions/run",
		map[string]any{"values": map[string]any{"members": []string{memberID}}}, e.token)
	if resp.Status != 200 {
		t.Fatalf("POST plugin action = %d %s, want 200", resp.Status, resp.Raw)
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
		claims["grp"] != e.group || claims["role"] != "owner" || claims["jti"] == "" {
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
	resp = e.do("DELETE", "/v1/groups/"+e.group+"/plugins/fake-plugin", nil, e.token)
	if resp.Status != 204 {
		t.Fatalf("DELETE group plugin = %d %s, want 204", resp.Status, resp.Raw)
	}
	resp = e.do("POST", "/v1/groups/"+e.group+"/plugins/fake-plugin/pages/dashboard/list", nil, e.token)
	if resp.Status != 404 || resp.Body["message"] != "plugin not enabled" {
		t.Errorf("disabled plugin list = %d %v, want 404 plugin not enabled", resp.Status, resp.Body)
	}
}
