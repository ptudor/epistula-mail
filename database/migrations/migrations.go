// Package migrations applies versioned SQL DDL against the epistula-database
// Postgres schema. Migration files are embedded at compile time so the
// binary needs no external assets, and each migration runs in its own
// transaction together with the corresponding schema_versions row insert,
// so a half-applied migration cannot be recorded as successful.
//
// A migration whose first line is `-- migrate: no-transaction` runs outside a
// transaction instead, statement by statement, so it can build and drop
// indexes CONCURRENTLY on a live table (see statements.go for the contract
// that puts on such a migration).
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
	"github.com/jackc/pgx/v5/pgconn"
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
	// NoTransaction is set by the `-- migrate: no-transaction` directive: the
	// script runs statement by statement outside a transaction, which is what
	// CREATE INDEX CONCURRENTLY and DROP INDEX CONCURRENTLY require.
	NoTransaction bool
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
		noTx, err := parseDirectives(string(body))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		out = append(out, Migration{
			Version:       ver,
			Description:   strings.ReplaceAll(m[2], "_", " "),
			SQL:           string(body),
			NoTransaction: noTx,
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
	return ApplyUpTo(ctx, pool, 0)
}

// ErrNoSuchVersion is returned by ApplyUpTo for a bound that names no
// embedded migration: a typo in `migrate up -to` must not silently apply
// everything, or nothing.
var ErrNoSuchVersion = errors.New("migrations: no migration with that version")

// ApplyUpTo is Apply bounded at a version: pending migrations above `through`
// are left for a later run. Zero means no bound.
//
// This is what lets a schema change roll out without a window in which either
// the old or the new binaries fail: an additive migration first (a new
// function, column or index the new code reads), then the binaries, then the
// migration that removes what only the old code used.
func ApplyUpTo(ctx context.Context, pool *pgxpool.Pool, through int) ([]Migration, error) {
	if through > 0 {
		all, err := All()
		if err != nil {
			return nil, err
		}
		known := false
		for _, m := range all {
			if m.Version == through {
				known = true
				break
			}
		}
		if !known {
			return nil, fmt.Errorf("%w: %03d", ErrNoSuchVersion, through)
		}
	}

	// A dedicated connection, because a session-level advisory lock belongs to
	// the connection that took it and pgxpool would otherwise hand the
	// subsequent statements to a different one.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()

	if err := acquireMigrationLock(ctx, conn); err != nil {
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
		if through > 0 && m.Version > through {
			break
		}
		var err error
		if m.NoTransaction {
			err = applyOutsideTransaction(ctx, conn, m)
		} else {
			err = applyOne(ctx, conn, m)
		}
		if err != nil {
			return applied, fmt.Errorf("migration %03d (%s): %w", m.Version, m.Description, err)
		}
		applied = append(applied, m)
		slog.Info("migration applied", "version", m.Version, "description", m.Description)
	}
	return applied, nil
}

// migrationLockPoll is how often a runner that found the lock taken asks again.
const migrationLockPoll = 250 * time.Millisecond

// acquireMigrationLock takes the session lock, waiting from Go rather than in
// the server.
//
// `SELECT pg_advisory_lock($1)` blocks inside a statement, and a blocked
// statement holds a snapshot. CREATE INDEX CONCURRENTLY, which the
// no-transaction migrations use, waits for every transaction whose snapshot
// is older than its own before it can finish — so a second runner waiting
// for the lock made the first runner's index build wait for the second
// runner, forever, until PostgreSQL's deadlock detector cancelled one of
// them. Polling pg_try_advisory_lock leaves a waiting runner idle between
// attempts, holding nothing the build has to wait for.
func acquireMigrationLock(ctx context.Context, conn rowQuerier) error {
	for {
		var got bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, migrationLockKey).Scan(&got); err != nil {
			return err
		}
		if got {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(migrationLockPoll):
		}
	}
}

// txBeginner is the subset of a pool or a pinned connection applyOne needs, so
// the run can be pinned to one connection for the advisory lock's sake.
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// statementRunner is what applyOutsideTransaction needs: a single connection
// that executes one statement at a time, each in its own implicit
// transaction, and reports whether it is sitting in an open transaction.
type statementRunner interface {
	rowQuerier
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Conn() *pgx.Conn
}

// applyOutsideTransaction runs a `-- migrate: no-transaction` migration:
// every statement on its own, on the one pinned connection, with the version
// recorded only after the last one succeeds. See statements.go for what that
// requires of the migration.
//
// An interrupted CREATE INDEX CONCURRENTLY leaves an INVALID index behind
// under the name it was building; a rerun's IF NOT EXISTS would then see the
// name, skip the build, and record the migration as applied with an index the
// planner never uses. So before running, any invalid index whose name this
// migration builds is dropped. Only names this migration owns are touched:
// the migration lock serializes runners, so such an index can only be the
// wreckage of an earlier attempt at this very migration.
func applyOutsideTransaction(ctx context.Context, conn statementRunner, m Migration) (err error) {
	stmts := splitStatements(m.SQL)
	if len(stmts) == 0 {
		return errors.New("no statements")
	}
	defer func() {
		// A failure inside an explicit BEGIN ... COMMIT group leaves the
		// connection in an aborted transaction. Roll it back so the advisory
		// unlock that follows can run, rather than leaving the lock to die
		// with the connection.
		if err != nil && conn.Conn().PgConn().TxStatus() != 'I' {
			rbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = conn.Exec(rbCtx, `ROLLBACK`)
		}
	}()

	for _, name := range concurrentIndexNames(stmts) {
		if err := dropInvalidIndex(ctx, conn, name); err != nil {
			return err
		}
	}
	for i, st := range stmts {
		if _, err := conn.Exec(ctx, st); err != nil {
			return fmt.Errorf("statement %d of %d (%s): %w (the migration runs outside a transaction; the statements before it are committed and all are idempotent, so rerun `migrate up`)",
				i+1, len(stmts), statementLabel(st), err)
		}
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO schema_versions (version, description) VALUES ($1, $2)
		   ON CONFLICT (version) DO NOTHING`,
		m.Version, m.Description,
	); err != nil {
		return fmt.Errorf("record version: %w", err)
	}
	return nil
}

// dropInvalidIndex removes an index of the given name (as written in the
// migration, quotes included) if it exists and is not valid.
func dropInvalidIndex(ctx context.Context, conn statementRunner, name string) error {
	var invalid bool
	err := conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_index i
			  JOIN pg_class c ON c.oid = i.indexrelid
			  JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = current_schema()
			   AND c.relname = $1
			   AND NOT i.indisvalid)`,
		strings.Trim(name, `"`)).Scan(&invalid)
	if err != nil {
		return fmt.Errorf("check index %s: %w", name, err)
	}
	if !invalid {
		return nil
	}
	slog.Warn("dropping invalid index left by an interrupted concurrent build; it will be rebuilt",
		"index", name)
	if _, err := conn.Exec(ctx, `DROP INDEX CONCURRENTLY IF EXISTS `+name); err != nil {
		return fmt.Errorf("drop invalid index %s: %w", name, err)
	}
	return nil
}

// statementLabel is the first few words of a statement, comments stripped,
// for an error message an operator can match against the migration file.
func statementLabel(st string) string {
	var words []string
	for _, line := range strings.Split(st, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "--") {
			continue
		}
		words = append(words, strings.Fields(line)...)
		if len(words) >= 6 {
			break
		}
	}
	if len(words) > 6 {
		words = words[:6]
	}
	return strings.Join(words, " ")
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
