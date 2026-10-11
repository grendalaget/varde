package store

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrimaryFileElector(t *testing.T) {
	// LiteFS materializes .primary only on replicas; the primary has none.
	p := filepath.Join(t.TempDir(), ".primary")
	e := NewPrimaryFileElector(p, "node-a")
	if !e.IsLeader() {
		t.Fatal("absent primary file should lead (we are the primary)")
	}
	if err := os.WriteFile(p, []byte("node-b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if e.IsLeader() {
		t.Fatal("another node's name should not lead")
	}
	if err := os.WriteFile(p, []byte("node-a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !e.IsLeader() {
		t.Fatal("own name in primary file should lead")
	}
	if err := os.WriteFile(p, []byte("node-b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if e.IsLeader() {
		t.Fatal("leadership should stop when the lease moves away")
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if !e.IsLeader() {
		t.Fatal("leadership should return when the file disappears")
	}
}

func TestElectorSqliteAlwaysLeader(t *testing.T) {
	s, err := Open("sqlite://"+t.TempDir()+"/t.db", nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()
	e := s.NewElector(1, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	if !e.IsLeader() {
		t.Fatal("sqlite elector should always lead")
	}
}

func TestElectorPostgresSingleLeader(t *testing.T) {
	url := postgresTestURL()
	if url == "" {
		t.Skip("VARDE_TEST_POSTGRES_URL unset")
	}
	open := func() *Store {
		s, err := Open(url, nil)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}
	s1, s2 := open(), open()
	defer func() { _ = s1.Close() }()
	defer func() { _ = s2.Close() }()

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	e1 := s1.NewElector(42, slog.Default())
	e2 := s2.NewElector(42, slog.Default())
	go e1.Run(ctx1)
	go e2.Run(ctx2)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && e1.IsLeader() == e2.IsLeader() {
		time.Sleep(50 * time.Millisecond)
	}
	if e1.IsLeader() == e2.IsLeader() {
		t.Fatal("exactly one elector should lead")
	}

	// Simulate the leader dying: kill its Postgres session (drops the
	// advisory lock) and stop its Run loop (a dead process stops competing).
	leaderStore, leaderCancel, standby := s1, cancel1, e2
	if e2.IsLeader() {
		leaderStore, leaderCancel, standby = s2, cancel2, e1
	}
	if _, err := leaderStore.DB.Exec(
		`SELECT pg_terminate_backend(pid) FROM pg_locks
		  WHERE locktype = 'advisory' AND granted AND objid = 42
		    AND pid <> pg_backend_pid()`); err != nil {
		t.Fatalf("terminate leader session: %v", err)
	}
	leaderCancel()

	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !standby.IsLeader() {
		time.Sleep(50 * time.Millisecond)
	}
	if !standby.IsLeader() {
		t.Fatal("standby did not take over after the leader's session ended")
	}
}
