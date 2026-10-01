// Package migrations applies versioned SQL DDL against the epistula-database
// Postgres schema. Migration files are embedded at compile time so the
// binary needs no external assets, and each migration runs in its own
// transaction together with the corresponding schema_versions row insert,
// so a half-applied migration cannot be recorded as successful.
//
// File naming: migrations/NNN_short_description.sql where NNN is a 3-digit
// zero-padded integer. Files are applied in ascending version order. Each
// version is applied at most once; the schema_versions table tracks state.
//
// Migration 001 is the initial schema (see schema.sql at the project root —
// the two are kept in sync; new deployments may bootstrap from either).
package migrations

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed *.sql
var migrationFS embed.FS

var fileNameRe = regexp.MustCompile(`^([0-9]{3})_([a-z0-9_]+)\.sql$`)

// Migration is a single embedded DDL script.
type Migration struct {
	Version     int
	Description string
	SQL         string
}

// All returns every embedded migration sorted by ascending Version.
func All() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	out := make([]Migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := fileNameRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		ver, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("parse version in %s: %w", e.Name(), err)
		}
		body, err := fs.ReadFile(migrationFS, e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		out = append(out, Migration{
			Version:     ver,
			Description: strings.ReplaceAll(m[2], "_", " "),
			SQL:         string(body),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i := 1; i < len(out); i++ {
		if out[i].Version == out[i-1].Version {
			return nil, fmt.Errorf("duplicate migration version %d", out[i].Version)
		}
	}
	return out, nil
}

// AppliedVersions returns the set of migration versions already recorded in
// schema_versions. If the table does not yet exist, returns an empty set
// (a fresh database) so the runner can create it via migration 001.
func AppliedVersions(ctx context.Context, pool *pgxpool.Pool) (map[int]struct{}, error) {
	return appliedVersionsOn(ctx, pool)
}

// appliedVersionsOn is AppliedVersions against any querier, so the run can be
// pinned to the connection holding the advisory lock (RA6X-037).
func appliedVersionsOn(ctx context.Context, q rowQuerier) (map[int]struct{}, error) {
	var exists bool
	if err := q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			 WHERE table_schema = current_schema() AND table_name = 'schema_versions'
		)`).Scan(&exists); err != nil {
		return nil, fmt.Errorf("probe schema_versions: %w", err)
	}
	applied := make(map[int]struct{})
	if !exists {
		return applied, nil
	}
	rows, err := q.Query(ctx, `SELECT version FROM schema_versions`)
	if err != nil {
		return nil, fmt.Errorf("query schema_versions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan schema_versions: %w", err)
		}
		applied[v] = struct{}{}
	}
	return applied, rows.Err()
}

// Pending returns the migrations in All() that are NOT yet recorded in
// schema_versions, in ascending order.
func Pending(ctx context.Context, pool *pgxpool.Pool) ([]Migration, error) {
	return pendingOn(ctx, pool, All)
}

// pendingOn is Pending against any querier, so Apply can compute the pending
// set on the connection that holds the migration lock (RA6X-037). Reading it
// on a different connection would reopen the window the lock exists to close.
func pendingOn(ctx context.Context, q rowQuerier, list func() ([]Migration, error)) ([]Migration, error) {
	all, err := list()
	if err != nil {
		return nil, err
	}
	applied, err := appliedVersionsOn(ctx, q)
	if err != nil {
		return nil, err
	}
	var pending []Migration
	for _, m := range all {
		if _, ok := applied[m.Version]; !ok {
			pending = append(pending, m)
		}
	}
	return pending, nil
}

// allMigrations is All as a value, so pendingOn can be given the real list.
var allMigrations = All

// rowQuerier is the read surface pendingOn needs, satisfied by both
// *pgxpool.Pool and a pinned *pgxpool.Conn.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// migrationLockKey is the advisory-lock key every migration runner takes
// before it looks at what is pending. Derived from a fixed string so two
// binaries built from this repository — `migrate up`, a serve process that
// migrates at startup, an operator running both — agree on it without
// configuration.
//
// A SESSION lock, not a transaction lock: each migration commits in its own
// transaction, so a transaction-scoped lock would be released between them and
// the serialization would cover only one migration at a time.
const migrationLockKey int64 = 0x6D61696C5F6D6967 // "mail_mig"

// Apply runs every pending migration in order, serialized against any other
// runner (RA6X-037).
//
// Each migration executes in its own transaction with the schema_versions
// insert; a failure aborts the run and leaves earlier (successful) migrations
// applied. Returns the migrations actually applied.
//
// The lock is taken BEFORE the pending set is computed, and held across the
// whole run. Without it two runners each read the same pending list and each
// executed the same SQL: idempotent DDL let both succeed, but a data migration
// ran twice and migration 009's sequence initialization advanced twice.
// `ON CONFLICT DO NOTHING` on the tracking row hides that — it makes recording
// the version idempotent, which is a different thing from making the migration
// body run once.
func Apply(ctx context.Context, pool *pgxpool.Pool) ([]Migration, error) {
	// A dedicated connection, because a session-level advisory lock belongs to
	// the connection that took it and pgxpool would otherwise hand the
	// subsequent statements to a different one.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return nil, fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		// Release on every path, including cancellation. A session lock also
		// dies with the connection, so a crashed runner does not wedge the
		// database — but a runner that merely returned must not hold it until
		// its pool connection happens to be recycled.
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, migrationLockKey); err != nil {
			slog.Warn("release migration lock", "err", err)
		}
	}()

	// Read the pending set UNDER the lock: a runner that waited here must see
	// what the previous one committed, not what was pending when it started.
	pending, err := pendingOn(ctx, conn, allMigrations)
	if err != nil {
		return nil, err
	}
	applied := make([]Migration, 0, len(pending))
	for _, m := range pending {
		if err := applyOne(ctx, conn, m); err != nil {
			return applied, fmt.Errorf("migration %03d (%s): %w", m.Version, m.Description, err)
		}
		applied = append(applied, m)
		slog.Info("migration applied", "version", m.Version, "description", m.Description)
	}
	return applied, nil
}

// txBeginner is the subset of a pool or a pinned connection applyOne needs, so
// the run can be pinned to one connection for the advisory lock's sake.
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

func applyOne(ctx context.Context, pool txBeginner, m Migration) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	if _, err := tx.Exec(ctx, m.SQL); err != nil {
		return fmt.Errorf("exec sql: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_versions (version, description) VALUES ($1, $2)
		   ON CONFLICT (version) DO NOTHING`,
		m.Version, m.Description,
	); err != nil {
		// schema_versions may not exist yet if migration 001 forgot to
		// create it; surface that as an explicit error rather than a
		// cryptic relation-not-found.
		if errors.Is(err, pgx.ErrNoRows) || strings.Contains(err.Error(), "schema_versions") {
			return fmt.Errorf("record version (does migration 001 create schema_versions?): %w", err)
		}
		return fmt.Errorf("record version: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}
