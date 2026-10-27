package api_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/grendalaget/varde/apps/controlplane/internal/api"
	"github.com/grendalaget/varde/apps/controlplane/internal/enroll"
	"github.com/grendalaget/varde/apps/controlplane/internal/plugins"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

type emptyPluginManager struct{}

func (emptyPluginManager) Catalog() []plugins.CatalogEntry { return nil }
func (emptyPluginManager) Installed(context.Context) ([]store.InstalledPlugin, error) {
	return nil, nil
}
func (emptyPluginManager) Install(context.Context, string) (store.InstalledPlugin, error) {
	return store.InstalledPlugin{}, nil
}
func (emptyPluginManager) Uninstall(context.Context, string) error { return nil }

func TestPluginAdminRequiresOperatorAndHonorsEmailAllowlist(t *testing.T) {
	e := newEnv(t)
	e.srv.PluginManager = emptyPluginManager{}
	regular := e.signup("member@example.com", "password123")
	if response := e.do(http.MethodGet, "/v1/admin/plugins", nil, regular); response.Status != http.StatusForbidden {
		t.Fatalf("non-operator plugin list status = %d, want 403", response.Status)
	}
	me := e.do(http.MethodGet, "/v1/me", nil, regular)
	if me.Status != http.StatusOK || me.Body["operator"] != false {
		t.Fatalf("non-operator /v1/me = status %d, body %s", me.Status, me.Raw)
	}

	allowlisted := e.signup("ops@example.com", "password123")
	e.srv.Cfg.Operators = api.ParseOperatorEmails(" Other@example.com, OPS@EXAMPLE.COM ")
	if response := e.do(http.MethodGet, "/v1/admin/plugins", nil, allowlisted); response.Status != http.StatusOK {
		t.Fatalf("allowlisted plugin list status = %d, body %s", response.Status, response.Raw)
	}
	me = e.do(http.MethodGet, "/v1/me", nil, allowlisted)
	if me.Status != http.StatusOK || me.Body["operator"] != true {
		t.Fatalf("allowlisted /v1/me = status %d, body %s", me.Status, me.Raw)
	}
	updated := e.do(http.MethodPatch, "/v1/me", map[string]any{"display_name": "Operations"}, allowlisted)
	if updated.Status != http.StatusOK || updated.Body["operator"] != true {
		t.Fatalf("allowlisted PATCH /v1/me = status %d, body %s", updated.Status, updated.Raw)
	}
}

type nonLeader struct{}

func (nonLeader) IsLeader() bool { return false }

func TestPluginRequestsReplayToKnownPrimary(t *testing.T) {
	e := newEnv(t)
	e.recon.Leader = nonLeader{}
	e.srv.PrimaryID = func() string { return "328f00abc123" }
	rawToken, err := enroll.ServiceToken()
	if err != nil {
		t.Fatal(err)
	}
	now := e.clk.ms
	if err := e.st.CreatePluginInstall(context.Background(), &store.ServiceToken{
		ID: "st_replay", Name: "plugin-dynamic-plugin", TokenHash: enroll.Hash(rawToken),
		ScopesJSON: `["members"]`, CreatedAt: now,
	}, &store.InstalledPlugin{
		ID: "dynamic-plugin", Version: "1.0.0", SealedSecret: "sealed",
		Port: 7310, Status: "running", InstalledAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, e.http.URL+"/v1/groups/"+e.group+"/plugins/dynamic-plugin", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK ||
		response.Header.Get("fly-replay") != "instance=328f00abc123" {
		t.Fatalf("replica response = %d, fly-replay=%q", response.StatusCode, response.Header.Get("fly-replay"))
	}
	_ = response.Body.Close()

	e.srv.PrimaryID = func() string { return "" }
	req, err = http.NewRequest(http.MethodGet, e.http.URL+"/v1/groups/"+e.group+"/plugins/dynamic-plugin", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unknown-primary response = %d, want 503", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "plugin runs on the leader") {
		t.Fatal("unknown-primary response did not explain that the plugin runs on the leader")
	}

	req, err = http.NewRequest(http.MethodGet, e.http.URL+"/v1/groups/"+e.group+"/plugins/static-plugin", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusUnauthorized || response.Header.Get("fly-replay") != "" {
		t.Fatalf("static plugin response = %d, fly-replay=%q; expected local auth handling",
			response.StatusCode, response.Header.Get("fly-replay"))
	}
	_ = response.Body.Close()
}

func TestInstalledPluginServiceTokenMustBeEnabled(t *testing.T) {
	e := newEnv(t)
	rawToken, err := enroll.ServiceToken()
	if err != nil {
		t.Fatal(err)
	}
	now := e.clk.ms
	serviceToken := &store.ServiceToken{
		ID: "st_dynamic_plugin", Name: "plugin-dynamic-plugin", TokenHash: enroll.Hash(rawToken),
		ScopesJSON: `["members"]`, CreatedAt: now,
	}
	installed := &store.InstalledPlugin{
		ID: "dynamic-plugin", Version: "1.0.0", SealedSecret: "sealed",
		Port: 7310, Status: "running", InstalledAt: now, UpdatedAt: now,
	}
	if err := e.st.CreatePluginInstall(context.Background(), serviceToken, installed); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.Plugins.Add(&plugins.Plugin{
		Manifest: plugins.Manifest{
			ID: "static-plugin", Name: "Static plugin", Version: "1.0.0",
			BaseURL: "http://127.0.0.1:7311", ServiceToken: serviceToken.Name,
		},
		Secret:       []byte(strings.Repeat("s", 32)),
		ManifestPath: "static-plugin.json",
	}); err != nil {
		t.Fatal(err)
	}
	path := "/v1/groups/" + e.group + "/members"
	response := e.do(http.MethodGet, path, nil, rawToken)
	if response.Status != http.StatusForbidden || !strings.Contains(string(response.Raw), "plugin not enabled") {
		t.Fatalf("disabled dynamic plugin token status = %d, body %s", response.Status, response.Raw)
	}
	operator, err := e.st.UserByEmail(context.Background(), "op@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.UpsertGroupPlugin(context.Background(), store.GroupPlugin{
		GroupID: e.group, PluginID: installed.ID, SettingsJSON: `{}`, EnabledBy: operator.ID,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	response = e.do(http.MethodGet, path, nil, rawToken)
	if response.Status != http.StatusOK {
		t.Fatalf("enabled dynamic plugin token status = %d, body %s", response.Status, response.Raw)
	}
}
