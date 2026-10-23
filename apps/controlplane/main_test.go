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

func TestValidateTimingProfile(t *testing.T) {
	for _, tc := range []struct {
		name        string
		heartbeatMs int64
		leaseTTLMs  int64
		wantErr     bool
	}{
		{name: "standard", heartbeatMs: 5000, leaseTTLMs: 20000},
		{name: "short", heartbeatMs: 1000, leaseTTLMs: 5000},
		{name: "lease below three heartbeats", heartbeatMs: 1000, leaseTTLMs: 2999, wantErr: true},
		{name: "zero heartbeat", heartbeatMs: 0, leaseTTLMs: 5000, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTimingProfile(tc.name, tc.heartbeatMs, tc.leaseTTLMs)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateTimingProfile() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
