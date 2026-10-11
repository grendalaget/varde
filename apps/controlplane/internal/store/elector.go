package store

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"strings"
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

// PrimaryFileElector reports leadership from a "primary file" — e.g.
// LiteFS's <mount>/.primary, which LiteFS materializes on replica nodes
// with the hostname of the node holding the write lease. The file does not
// exist on the primary itself, and is re-read on each call so leadership
// follows the lease with no extra machinery.
type PrimaryFileElector struct {
	path string
	self string
}

// NewPrimaryFileElector builds an elector bound to path (the file that
// names the current primary, when it is someone else) for the node named
// self.
func NewPrimaryFileElector(path, self string) *PrimaryFileElector {
	return &PrimaryFileElector{path: path, self: self}
}

// IsLeader reports whether self may write: the file is absent (we are the
// primary, or the lease holder is not yet recorded) or names us. It reports
// false only while the file names another node. The gate prevents error
// loops, not split-brain — writes off the lease holder still fail at the
// storage layer.
func (e *PrimaryFileElector) IsLeader() bool {
	b, err := os.ReadFile(e.path)
	if err != nil {
		return true // no file = no recorded other-primary
	}
	name := strings.TrimSpace(string(b))
	return name == "" || name == e.self
}
