package plugins

import "testing"

func managedManifest() Manifest {
	m := validManifest()
	m.Settings = []Field{
		{Name: "hosting", Type: "select", Options: []string{"managed", "manual"}, Default: "managed"},
		{Name: "licence", Type: "select", Options: []string{"noncommercial", "commercial"}, Required: true},
		{Name: "address", Type: "string", Required: true},
		{Name: "token", Type: "secret", Required: true},
	}
	m.ManagedServer = &ManagedServer{Game: "autotournament", ModeSetting: "hosting", Config: map[string]string{"licence": "licence"}, Bindings: map[string]string{"address": "public_url", "token": "secret:api_token"}}
	return m
}

func TestManagedManifestValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*Manifest)
	}{
		{name: "unknown game", edit: func(m *Manifest) { m.ManagedServer.Game = "unknown" }},
		{name: "unknown mode", edit: func(m *Manifest) { m.ManagedServer.ModeSetting = "missing" }},
		{name: "mode without default", edit: func(m *Manifest) { m.Settings[0].Default = nil }},
		{name: "mode without alternative", edit: func(m *Manifest) { m.Settings[0].Options = []string{"managed"} }},
		{name: "unknown config", edit: func(m *Manifest) { m.ManagedServer.Config["unknown"] = "licence" }},
		{name: "unknown input", edit: func(m *Manifest) { m.ManagedServer.Config["licence"] = "missing" }},
		{name: "missing required config", edit: func(m *Manifest) { m.ManagedServer.Config = nil }},
		{name: "secret input", edit: func(m *Manifest) { m.ManagedServer.Config["licence"] = "token" }},
		{name: "public secret input", edit: func(m *Manifest) { m.ManagedServer.Config["steam_web_api_key"] = "address" }},
		{name: "generated and mapped secret", edit: func(m *Manifest) {
			m.Settings = append(m.Settings, Field{Name: "input", Type: "secret"})
			m.ManagedServer.Config["api_token"] = "input"
		}},
		{name: "incompatible config type", edit: func(m *Manifest) { m.Settings[1].Type = "bool"; m.Settings[1].Options = nil }},
		{name: "unknown target", edit: func(m *Manifest) { m.ManagedServer.Bindings["missing"] = "public_url" }},
		{name: "unknown source", edit: func(m *Manifest) { m.ManagedServer.Bindings["address"] = "ip_address" }},
		{name: "non-secret source", edit: func(m *Manifest) { m.ManagedServer.Bindings["token"] = "secret:licence" }},
		{name: "unprotected secret", edit: func(m *Manifest) { m.Settings[3].Type = "string" }},
		{name: "no bindings", edit: func(m *Manifest) { m.ManagedServer.Bindings = nil }},
		{name: "mode is bound", edit: func(m *Manifest) { m.ManagedServer.Bindings["hosting"] = "public_url" }},
		{name: "binding cycle", edit: func(m *Manifest) { m.ManagedServer.Config["licence"] = "address" }},
		{name: "hidden binding", edit: func(m *Manifest) { m.Settings[2].Hidden = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := managedManifest()
			test.edit(&m)
			if err := validateManifest(&m, true); err == nil {
				t.Errorf("validateManifest(%s) = nil, want invalid managed_server", test.name)
			}
		})
	}
	m := managedManifest()
	if err := validateManifest(&m, true); err != nil {
		t.Fatalf("validateManifest(valid managed declaration) = %v, want nil", err)
	}
	m.ManagedServer.Bindings["address"] = "setup_code"
	if err := validateManifest(&m, true); err != nil {
		t.Errorf("validateManifest(setup_code binding) = %v, want nil", err)
	}
}

func TestManagedSecretConfig(t *testing.T) {
	for _, field := range []Field{{Name: "steam", Type: "secret"}, {Name: "steam", Type: "string", Secret: true}} {
		m := managedManifest()
		m.Settings = append(m.Settings, field)
		m.ManagedServer.Config["steam_web_api_key"] = "steam"
		if err := validateManifest(&m, true); err != nil {
			t.Fatal(err)
		}
	}
	m := managedManifest()
	m.ManagedServer.Game = "valheim"
	m.Settings = append(m.Settings, Field{Name: "password_input", Type: "secret", Required: true})
	m.ManagedServer.Config = map[string]string{"server_name": "address", "world_name": "address", "password": "password_input"}
	m.ManagedServer.Bindings = map[string]string{"token": "setup_code"}
	if err := validateManifest(&m, true); err != nil {
		t.Fatal(err)
	}
}

func TestManagedModeDefaultsAndManualAlternative(t *testing.T) {
	m := managedManifest()
	for _, test := range []struct {
		settings map[string]any
		want     bool
	}{
		{settings: nil, want: true},
		{settings: map[string]any{"hosting": "managed"}, want: true},
		{settings: map[string]any{"hosting": "manual"}, want: false},
	} {
		if got := m.Managed(test.settings); got != test.want {
			t.Errorf("Managed(%v) = %t, want %t", test.settings, got, test.want)
		}
	}
	m.ManagedServer = nil
	if m.Managed(nil) || m.Bound("address") {
		t.Error("manifest without managed_server is managed or bound, want neither")
	}
}

func TestManagedManifestRequiredSecret(t *testing.T) {
	m := managedManifest()
	m.ManagedServer.Game = "valheim"
	m.ManagedServer.Config = map[string]string{"server_name": "address", "world_name": "address"}
	m.ManagedServer.Bindings = map[string]string{"token": "setup_code"}
	if err := validateManifest(&m, true); err == nil {
		t.Error("validateManifest(missing required password binding) = nil, want error")
	}
	m.ManagedServer.Bindings["token"] = "secret:password"
	if err := validateManifest(&m, true); err != nil {
		t.Errorf("validateManifest(generated required password) = %v, want nil", err)
	}
}
