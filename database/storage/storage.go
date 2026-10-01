// Package storage owns the Postgres connection pool and the high-level
// operations the LDA and admin paths execute against it.
package storage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config carries the connection parameters from the daemon's TOML.
type Config struct {
	DSN              string
	StatementTimeout time.Duration
	MaxConns         int32
	MinConns         int32
	ConnMaxLifetime  time.Duration

	// SessionStatementTimeout applies StatementTimeout as a session-level
	// runtime parameter on every pooled connection, bounding queries that
	// run outside RunTx (the GC walker's per-blob checks, ad-hoc pool
	// queries). The LDA path leaves this false and relies on RunTx's
	// SET LOCAL; migrations leave it false because DDL legitimately runs
	// long.
	SessionStatementTimeout bool

	// SkipPing creates the (lazy) pgxpool without the eager connectivity ping,
	// so Open succeeds even when Postgres is briefly down. Used by the serve
	// readiness probe (R-043): the process must come up and report NOT-ready
	// until PG recovers, rather than failing to start.
	SkipPing bool
}

// DB wraps a pgxpool with the daemon's per-tx defaults.
type DB struct {
	pool     *pgxpool.Pool
	cfg      Config
	ownsPool bool // true for Open, false for NewFromPool
}

// Open creates and verifies the connection pool. Returns an error if the
// pool cannot be reached within 5 seconds.
func Open(ctx context.Context, cfg Config) (*DB, error) {
	if cfg.DSN == "" {
		return nil, errors.New("storage: DSN is required")
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse DSN: %w", err)
	}
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		poolCfg.MinConns = cfg.MinConns
	}
	if cfg.ConnMaxLifetime > 0 {
		poolCfg.MaxConnLifetime = cfg.ConnMaxLifetime
	}
	if cfg.SessionStatementTimeout && cfg.StatementTimeout > 0 {
		poolCfg.ConnConfig.RuntimeParams["statement_timeout"] =
			strconv.FormatInt(cfg.StatementTimeout.Milliseconds(), 10)
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	if !cfg.SkipPing {
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := pool.Ping(pingCtx); err != nil {
			pool.Close()
			return nil, fmt.Errorf("ping postgres: %w", err)
		}
	}

	slog.Debug("postgres pool ready",
		"max_conns", poolCfg.MaxConns,
		"min_conns", poolCfg.MinConns,
	)
	return &DB{pool: pool, cfg: cfg, ownsPool: true}, nil
}

// Pool returns the underlying connection pool for callers that need to
// run their own queries (admin CLI, gc, migrations, etc.).
func (db *DB) Pool() *pgxpool.Pool { return db.pool }

// NewFromPool wraps an externally-owned pool so callers that already
// manage their pgxpool lifecycle (e.g. the IMAP server's serve mode)
// can still invoke the high-level Ingest path. The returned DB does
// NOT close the pool on Close — that's the caller's responsibility.
func NewFromPool(pool *pgxpool.Pool, cfg Config) *DB {
	return &DB{pool: pool, cfg: cfg, ownsPool: false}
}

// Close releases the pool only if this DB owns it. Pools wrapped via
// NewFromPool are the caller's responsibility to close.
func (db *DB) Close() {
	if db.pool != nil && db.ownsPool {
		db.pool.Close()
	}
}

// Ping does a round-trip against the pool.
func (db *DB) Ping(ctx context.Context) error {
	if db.pool == nil {
		return errors.New("storage: pool is closed")
	}
	return db.pool.Ping(ctx)
}

// RunTx begins a transaction, sets statement_timeout, runs fn, and commits
// or rolls back. Exported for callers outside the package (gc sweep) that
// need transactional work with the same timeout discipline as Ingest.
func (db *DB) RunTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return db.runTx(ctx, fn)
}

// runTx begins a transaction, sets statement_timeout, runs fn, and commits
// or rolls back. Panics inside fn roll back and re-panic.
func (db *DB) runTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return db.RunTxCommitted(ctx, fn, nil)
}

// RunTxCommitted records a successful commit before any post-transaction work.
// The callback must not perform I/O; the LDA uses it for its acceptance latch.
func (db *DB) RunTxCommitted(ctx context.Context, fn func(tx pgx.Tx) error, afterCommit func()) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	var committed bool
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	if db.cfg.StatementTimeout > 0 {
		stmt := fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", db.cfg.StatementTimeout.Milliseconds())
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("set statement_timeout: %w", err)
		}
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	if afterCommit != nil {
		afterCommit()
	}
	return nil
}

// IsRetryable returns true for errors that the LDA should map to EX_TEMPFAIL
// (defer/requeue) rather than a bounce-class code. It classifies by SQLSTATE
// where possible — the reliable signal — and falls back to typed net errors
// and a substring list only for errors that never surface as a structured
// PgError.
//
// A wrong answer here is expensive in both directions: a false negative
// bounces legitimate mail on a routine Postgres restart or a lock-wait
// timeout; a false positive would defer a genuinely permanent error forever.
// So constraint/data errors (SQLSTATE class 22/23/42/...) are deliberately
// NOT retryable — a retry would fail identically.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}

	// A statement_timeout or the delivery watchdog surfaces as a context
	// deadline; the next delivery cycle may succeed.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	// Structured Postgres error → classify exactly by SQLSTATE class. This is
	// checked before the generic fallbacks because it is authoritative: a
	// PgError we recognize as non-transient (e.g. 23505 unique_violation)
	// must return false even if its message happens to contain a fallback
	// substring.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if len(pgErr.Code) >= 2 {
			switch pgErr.Code[:2] {
			case "08", // connection exception
				"40", // transaction rollback: 40001 serialization_failure, 40P01 deadlock_detected
				"53", // insufficient resources: 53300 too_many_connections, 53100 disk_full
				"57": // operator intervention: 57014 query_canceled (statement_timeout), 57P01 admin shutdown, 57P03 cannot_connect_now
				return true
			}
		}
		// Any other structured PG error (23xxx constraint, 22xxx data,
		// 42xxx syntax/permission, ...) is a genuine, non-transient failure.
		return false
	}

	// pgx flags errors where the request was provably never sent (safe to
	// retry without side effects).
	if pgconn.SafeToRetry(err) {
		return true
	}

	// Network-level timeouts (dial/read/write) are transient.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	// Last-resort substring fallback for driver-internal errors that arrive as
	// plain wrapped strings rather than a typed PgError/net.Error.
	msg := err.Error()
	for _, sub := range []string{
		"connection refused",
		"connection reset",
		"server closed the connection",
		"unexpected EOF",
		"i/o timeout",
		"could not translate host",
		"context deadline exceeded",
		"connection timed out",
		"broken pipe",
	} {
		if strings.Contains(msg, sub) {
			return true
		}
	}
	return false
}
