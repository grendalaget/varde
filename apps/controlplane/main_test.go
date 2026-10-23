package main

import "testing"

func TestSqlitePath(t *testing.T) {
	for in, want := range map[string]string{
		"sqlite:///tmp/x/cp.db":       "/tmp/x/cp.db",
		"sqlite:///C:/varde/cp.db":    "C:/varde/cp.db",
		"sqlite:///x.db?pragma=1":     "/x.db",
		"postgres://h/db":             "",
		"sqlite://relative/dir/db.db": "relative/dir/db.db",
		"":                            "",
	} {
		if got := sqlitePath(in); got != want {
			t.Errorf("sqlitePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAutoTournamentPrivateAddressDefaults(t *testing.T) {
	tests := []struct {
		name      string
		authMode  string
		envValue  string
		args      []string
		want      bool
		wantError bool
	}{
		{name: "local default", authMode: "local", want: true},
		{name: "federated default", authMode: "federated", want: false},
		{name: "command line auth mode", authMode: "local", args: []string{"--auth-mode=federated"}, want: false},
		{name: "environment override", authMode: "federated", envValue: "true", want: true},
		{name: "environment disables local private access", authMode: "local", envValue: "false", want: false},
		{name: "flag overrides environment", authMode: "local", envValue: "true", args: []string{"--autotournament-allow-private=false"}, want: false},
		{name: "flag enables private addresses", authMode: "federated", args: []string{"--autotournament-allow-private"}, want: true},
		{name: "invalid environment value", authMode: "local", envValue: "maybe", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("VARDE_AUTH_MODE", test.authMode)
			t.Setenv("VARDE_AUTOTOURNAMENT_ALLOW_PRIVATE", test.envValue)
			fs, cfg := serveFlags()
			if err := fs.Parse(test.args); err != nil {
				t.Fatal(err)
			}
			err := applyAutoTournamentPrivateDefault(cfg)
			if (err != nil) != test.wantError {
				t.Fatalf("applyAutoTournamentPrivateDefault() error = %v, wantError %t", err, test.wantError)
			}
			if err == nil && cfg.autotournamentAllowPrivate != test.want {
				t.Errorf("allow private = %t, want %t", cfg.autotournamentAllowPrivate, test.want)
			}
		})
	}
}
