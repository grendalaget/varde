// Package store is the control-plane persistence layer: database/sql + sqlx
// with portable SQL (sqlx.Rebind for placeholders). sqlite (modernc, default)
// and postgres (pgx stdlib) dialects; per-dialect embedded migrations.
//
// Portability rules: timestamps are BIGINT unix ms, booleans INTEGER 0/1,
// JSON TEXT, ids TEXT. Only the sequence columns differ per dialect.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

//go:embed migrations_sqlite.sql migrations_postgres.sql
var migrations embed.FS

// Clock is injectable so reconciler/policy logic never calls time.Now.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// RealClock uses the wall clock.
var RealClock Clock = realClock{}

// Store wraps the DB handle plus dialect for rebinding.
type Store struct {
	DB      *sqlx.DB
	Dialect string // "sqlite" | "postgres"
	Clock   Clock
}

// Open parses --db values: "sqlite:///<path>" (default) or "postgres://...".
// Applies embedded migrations.
func Open(url string, clk Clock) (*Store, error) {
	if clk == nil {
		clk = RealClock
	}
	s := &Store{Clock: clk}
	var drv, dsn string
	switch {
	case url == "" || strings.HasPrefix(url, "sqlite://"):
		drv = "sqlite"
		dsn = strings.TrimPrefix(url, "sqlite://")
		if dsn == "" {
			return nil, errors.New("store: empty sqlite path")
		}
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		dsn += sep + "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate"
	case strings.HasPrefix(url, "postgres://") || strings.HasPrefix(url, "postgresql://"):
		drv = "pgx"
		dsn = url
	default:
		return nil, fmt.Errorf("store: unsupported db url %q", url)
	}
	db, err := sqlx.Connect(drv, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	db.SetMaxOpenConns(1) // sqlite safety; pgx benefits little for this workload
	s.DB = db
	s.Dialect = map[string]string{"sqlite": "sqlite", "pgx": "postgres"}[drv]
	mig, err := migrations.ReadFile("migrations_" + s.Dialect + ".sql")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(string(mig)); err != nil {
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	if err := ensureColumns(db, s.Dialect); err != nil {
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return s, nil
}

// ensureColumns adds columns introduced after the CREATE TABLE IF NOT EXISTS
// files were written — old databases keep their tables, so the column must
// be added idempotently at startup.
func ensureColumns(db *sqlx.DB, dialect string) error {
	if dialect == "postgres" {
		_, err := db.Exec(`ALTER TABLE nodes ADD COLUMN IF NOT EXISTS owner_user_id TEXT REFERENCES users(id)`)
		return err
	}
	var n int
	if err := db.Get(&n, `SELECT count(*) FROM pragma_table_info('nodes') WHERE name='owner_user_id'`); err != nil {
		return err
	}
	if n == 0 {
		_, err := db.Exec(`ALTER TABLE nodes ADD COLUMN owner_user_id TEXT REFERENCES users(id)`)
		return err
	}
	return nil
}

func (s *Store) NowMs() int64 { return s.Clock.Now().UnixMilli() }

func (s *Store) Close() error { return s.DB.Close() }

// Ping is readiness: DB reachable.
func (s *Store) Ping(ctx context.Context) error { return s.DB.PingContext(ctx) }

// Tx runs fn inside a transaction.
func (s *Store) Tx(ctx context.Context, fn func(*sqlx.Tx) error) error {
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Rebind translates ? placeholders for the dialect.
func (s *Store) Rebind(q string) string { return s.DB.Rebind(q) }

// IsUniqueViolation reports a unique-constraint failure portably.
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint") || // modernc sqlite
		strings.Contains(msg, "duplicate key") || // pgx
		strings.Contains(msg, "2067") || strings.Contains(msg, "23505")
}

// Bool converts Go bool to the INTEGER 0/1 representation.
func Bool(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// NullableInt/NullableInt64 → sql NULL when nil.
func NullableInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}
