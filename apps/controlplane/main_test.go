package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

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

type promotionTestLeader struct {
	falseChecks int
	calls       int
	neverLeader bool
	firstCheck  chan struct{}
}

func (l *promotionTestLeader) IsLeader() bool {
	l.calls++
	if l.calls == 1 && l.firstCheck != nil {
		close(l.firstCheck)
	}
	return !l.neverLeader && l.calls > l.falseChecks
}

func TestMigrateOnPromotionRetriesUntilSuccess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	leader := &promotionTestLeader{falseChecks: 2}
	var attempts []int
	done := make(chan struct{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() {
		migrateOnPromotion(ctx, leader, func() error {
			attempts = append(attempts, leader.calls)
			if len(attempts) == 1 {
				return errors.New("temporary migration failure")
			}
			return nil
		}, 5*time.Millisecond, log)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("migrateOnPromotion did not return after successful migration")
	}
	if got, want := leader.calls, 4; got != want {
		t.Errorf("leader checks = %d, want %d", got, want)
	}
	if got, want := len(attempts), 2; got != want {
		t.Fatalf("migration attempts = %d, want %d", got, want)
	}
	if got, want := attempts[0], 3; got != want {
		t.Errorf("first migration leader check = %d, want %d", got, want)
	}
	if got, want := attempts[1], 4; got != want {
		t.Errorf("second migration leader check = %d, want %d", got, want)
	}
}

func TestMigrateOnPromotionStopsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	leader := &promotionTestLeader{neverLeader: true, firstCheck: make(chan struct{})}
	migrationCalled := false
	done := make(chan struct{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() {
		migrateOnPromotion(ctx, leader, func() error {
			migrationCalled = true
			return nil
		}, 5*time.Millisecond, log)
		close(done)
	}()

	select {
	case <-leader.firstCheck:
	case <-time.After(time.Second):
		t.Fatal("migrateOnPromotion did not check leadership")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("migrateOnPromotion did not return after context cancellation")
	}
	if migrationCalled {
		t.Error("migrateOnPromotion called migrate without leadership")
	}
}
