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
