package plugins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeManifest(t *testing.T, dir, name string, manifest Manifest) string {
	t.Helper()
	path := filepath.Join(dir, name)
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("json.Marshal(manifest) error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", path, err)
	}
	return path
}

func validManifest() Manifest {
	return Manifest{
		ID: "fake-plugin", Name: "Fake plugin", Version: "1.0.0",
		BaseURL: "http://127.0.0.1:9000/", SecretEnv: "FAKE_PLUGIN_SECRET",
		Settings: []Field{
			{Name: "api_key", Type: "secret", Required: true},
			{Name: "selected", Type: "multiselect", Options: []string{"a", "b"}},
			{Name: "cache", Type: "string", Hidden: true},
		},
		Pages: []Page{{
			ID: "home", Title: "Home", List: &List{Endpoint: "/list"},
			Actions: []Action{{ID: "run", Title: "Run", Endpoint: "/run", Fields: []Field{
				{Name: "members", Type: "member_multiselect", Requires: "steam_id"},
			}}},
		}},
	}
}

func TestLoadManifestNormalizesAndReadsSecret(t *testing.T) {
	t.Setenv("FAKE_PLUGIN_SECRET", strings.Repeat("s", 32))
	path := writeManifest(t, t.TempDir(), "plugin.json", validManifest())

	registry, err := Load([]string{path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v", path, err)
	}
	plugin, ok := registry.Get("fake-plugin")
	if !ok {
		t.Fatal("Registry.Get(fake-plugin) = false, want true")
	}
	if plugin.BaseURL != "http://127.0.0.1:9000" {
		t.Errorf("plugin.BaseURL = %q, want trailing slash removed", plugin.BaseURL)
	}
	if len(plugin.Secret) != 32 {
		t.Errorf("plugin secret length = %d, want 32", len(plugin.Secret))
	}
	if len(registry.All()) != 1 {
		t.Errorf("Registry.All() length = %d, want 1", len(registry.All()))
	}
}

func TestLoadManifestReadsRelativeSecretFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secret"), []byte(strings.Repeat("f", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := validManifest()
	manifest.SecretEnv = ""
	manifest.SecretFile = "secret"
	path := writeManifest(t, dir, "plugin.json", manifest)
	if _, err := Load([]string{path}); err != nil {
		t.Fatalf("Load(%q) error = %v", path, err)
	}
}

func TestLoadManifestRejectsInvalidDefinitions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*Manifest)
		wantErr string
	}{
		{name: "one-character id", mutate: func(m *Manifest) { m.ID = "x" }, wantErr: "must match"},
		{name: "both secret sources", mutate: func(m *Manifest) { m.SecretFile = "secret" }, wantErr: "exactly one"},
		{name: "neither secret source", mutate: func(m *Manifest) { m.SecretEnv = "" }, wantErr: "exactly one"},
		{name: "short secret", mutate: func(*Manifest) {}, wantErr: "at least 32"},
		{name: "invalid base url", mutate: func(m *Manifest) { m.BaseURL = "ftp://plugin.test" }, wantErr: "http or https"},
		{name: "url query", mutate: func(m *Manifest) { m.BaseURL = "https://plugin.test/?x=1" }, wantErr: "query"},
		{name: "invalid endpoint", mutate: func(m *Manifest) { m.Pages[0].List.Endpoint = "https://elsewhere.test" }, wantErr: "absolute path"},
		{name: "endpoint traversal", mutate: func(m *Manifest) { m.Pages[0].List.Endpoint = "/api/../admin" }, wantErr: "absolute path"},
		{name: "multiselect requires options", mutate: func(m *Manifest) { m.Settings[1].Options = nil }, wantErr: "options are required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FAKE_PLUGIN_SECRET", strings.Repeat("s", 32))
			manifest := validManifest()
			tc.mutate(&manifest)
			if tc.name == "short secret" {
				t.Setenv("FAKE_PLUGIN_SECRET", "short")
			}
			path := writeManifest(t, t.TempDir(), "plugin.json", manifest)
			_, err := Load([]string{path})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), path) {
				t.Fatalf("Load(%q) error = %v, want path and %q", path, err, tc.wantErr)
			}
		})
	}
}

func TestLoadRejectsDuplicatePluginIDs(t *testing.T) {
	t.Setenv("FAKE_PLUGIN_SECRET", strings.Repeat("s", 32))
	dir := t.TempDir()
	first := writeManifest(t, dir, "first.json", validManifest())
	second := writeManifest(t, dir, "second.json", validManifest())
	_, err := Load([]string{first, second})
	if err == nil || !strings.Contains(err.Error(), "duplicate plugin id") || !strings.Contains(err.Error(), second) {
		t.Fatalf("Load(%q, %q) error = %v, want duplicate id with second path", first, second, err)
	}
}

func TestLoadRejectsUnknownManifestFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugin.json")
	data := []byte(`{"id":"fake-plugin","name":"Fake","version":"1","base_url":"http://localhost","secret_env":"X","unknown":true}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load([]string{path}); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("Load(%q) error = %v, want named manifest error", path, err)
	}
}
