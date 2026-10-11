package store

import (
	"context"
	"database/sql"
	"log/slog"
	"sync/atomic"
	"time"
)

// Elector reports whether this process leads a single-writer background job.
// SQLite deployments are inherently single-instance, so Elector there always
// reports leadership. On Postgres it holds (or competes for) a session-level
// advisory lock, so several replicas can run the HTTP tier while exactly one
// runs the reconciler.
type Elector struct {
	store  *Store
	key    int64
	leader atomic.Bool
	log    *slog.Logger
}

// NewElector builds an Elector. key namespaces the advisory lock; distinct
// jobs that must not overlap should share one key.
func (s *Store) NewElector(key int64, log *slog.Logger) *Elector {
	e := &Elector{store: s, key: key, log: log}
	if s.Dialect == "sqlite" {
		e.leader.Store(true)
	}
	return e
}

// IsLeader reports whether this instance currently leads. Cheap; safe to poll.
func (e *Elector) IsLeader() bool { return e.leader.Load() }

// Run competes for leadership until ctx is cancelled. On Postgres it retries
// the advisory lock on a dedicated connection every retry interval; losing
// the connection (and with it the lock) flips IsLeader to false until the
// lock is re-acquired. No-op on sqlite.
func (e *Elector) Run(ctx context.Context) {
	if e.store.Dialect == "sqlite" {
		<-ctx.Done()
		return
	}
	const retry = 2 * time.Second
	for ctx.Err() == nil {
		conn, err := e.store.DB.Conn(ctx)
		if err != nil {
			e.leader.Store(false)
			e.log.Error("leader election: conn", "error", err)
			time.Sleep(retry)
			continue
		}
		var got bool
		err = conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", e.key).Scan(&got)
		if err != nil {
			e.leader.Store(false)
			e.log.Error("leader election: try lock", "error", err)
			_ = conn.Close()
			time.Sleep(retry)
			continue
		}
		if !got {
			// Another instance leads; the lock call keeps the conn honest.
			e.leader.Store(false)
			_ = conn.Close()
			time.Sleep(retry)
			continue
		}
		if e.leader.CompareAndSwap(false, true) {
			e.log.Info("leader election: acquired leadership")
		}
		// Hold the conn (and the lock) until cancellation or a ping failure;
		// a dropped conn releases the lock server-side.
		for ctx.Err() == nil && e.pingLeaderConn(ctx, conn) {
			time.Sleep(retry)
		}
		if e.leader.CompareAndSwap(true, false) {
			e.log.Info("leader election: released leadership")
		}
		// conn.Close returns the conn to the pool where its session (and lock)
		// would stay alive; release the lock explicitly first. Best-effort:
		// a dead conn has already dropped the lock server-side.
		_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock_all()")
		_ = conn.Close()
	}
	e.leader.Store(false)
}

func (e *Elector) pingLeaderConn(ctx context.Context, conn *sql.Conn) bool {
	if err := conn.PingContext(ctx); err != nil {
		e.log.Error("leader election: lost lock connection", "error", err)
		return false
	}
	return true
}
