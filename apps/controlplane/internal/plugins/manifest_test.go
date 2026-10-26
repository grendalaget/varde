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
	if err := os.WriteFile(filepath.Join(dir, "secret"), []byte(strings.Repeat("f", 32)+"\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := validManifest()
	manifest.SecretEnv = ""
	manifest.SecretFile = "secret"
	path := writeManifest(t, dir, "plugin.json", manifest)
	registry, err := Load([]string{path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v", path, err)
	}
	plugin, ok := registry.Get(manifest.ID)
	if !ok || string(plugin.Secret) != strings.Repeat("f", 32) {
		t.Fatalf("Load(%q) secret = %q, want trailing CR/LF removed", path, plugin.Secret)
	}
}

func TestLoadManifestRejectsInvalidDefaults(t *testing.T) {
	t.Setenv("FAKE_PLUGIN_SECRET", strings.Repeat("s", 32))
	tests := []struct {
		name  string
		field Field
	}{
		{name: "wrong type", field: Field{Name: "port", Type: "int", Default: "27015"}},
		{name: "option not declared", field: Field{Name: "region", Type: "select", Default: "ap", Options: []string{"eu", "us"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := validManifest()
			manifest.Settings = append(manifest.Settings, tt.field)
			path := writeManifest(t, t.TempDir(), "plugin.json", manifest)
			if _, err := Load([]string{path}); err == nil || !strings.Contains(err.Error(), "default") {
				t.Fatalf("Load(%q) error = %v, want invalid default error", path, err)
			}
		})
	}
}

func TestLoadManifestRejectsSecretDefaults(t *testing.T) {
	t.Setenv("FAKE_PLUGIN_SECRET", strings.Repeat("s", 32))
	for _, field := range []Field{
		{Name: "other_secret", Type: "secret", Default: "not-secret"},
		{Name: "token", Type: "string", Secret: true, Default: "not-secret"},
	} {
		manifest := validManifest()
		manifest.Settings = append(manifest.Settings, field)
		path := writeManifest(t, t.TempDir(), "plugin.json", manifest)
		_, err := Load([]string{path})
		want := field.Name + ": secret fields cannot have a default"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Load(%q) error = %v, want %q", path, err, want)
		}
	}
}

func TestLoadManifestValidatesSecretFieldTypes(t *testing.T) {
	t.Setenv("FAKE_PLUGIN_SECRET", strings.Repeat("s", 32))
	tests := []struct {
		name    string
		field   Field
		wantErr string
	}{
		{
			name:    "bool",
			field:   Field{Name: "enabled", Type: "bool", Secret: true},
			wantErr: "enabled: only string fields can be marked secret",
		},
		{
			name:    "multiselect",
			field:   Field{Name: "regions", Type: "multiselect", Options: []string{"eu"}, Secret: true},
			wantErr: "regions: only string fields can be marked secret",
		},
		{
			name:  "string",
			field: Field{Name: "token", Type: "string", Secret: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := validManifest()
			manifest.Settings = append(manifest.Settings, tt.field)
			path := writeManifest(t, t.TempDir(), "plugin.json", manifest)
			_, err := Load([]string{path})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("Load(%q) error = %v, want error containing %q", path, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Errorf("Load(%q) error = %v, want nil", path, err)
			}
		})
	}
}

func TestFieldValidateRequiredValues(t *testing.T) {
	tests := []struct {
		name  string
		field Field
		value any
		valid bool
	}{
		{
			name:  "required string whitespace",
			field: Field{Name: "title", Type: "string", Required: true},
			value: " \t",
		},
		{
			name:  "required select empty",
			field: Field{Name: "region", Type: "select", Required: true, Options: []string{"eu"}},
			value: "",
		},
		{
			name:  "required multiselect empty",
			field: Field{Name: "modes", Type: "multiselect", Required: true, Options: []string{"a"}},
			value: []any{},
		},
		{
			name:  "required members empty",
			field: Field{Name: "members", Type: "member_multiselect", Required: true},
			value: []any{},
		},
		{
			name:  "optional multiselect empty",
			field: Field{Name: "modes", Type: "multiselect", Options: []string{"a"}},
			value: []any{}, valid: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.field.Validate(tt.value)
			if gotValid := err == nil; gotValid != tt.valid {
				t.Errorf("Field.Validate(%v) error = %v, want valid=%t", tt.value, err, tt.valid)
			}
		})
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
