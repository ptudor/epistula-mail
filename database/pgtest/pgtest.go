// Package pgtest spins up an ephemeral Postgres database per test and
// runs the embedded migrations against it, so integration tests can
// exercise real DB-coupled code (the recipients resolver, storage.Ingest,
// the migrations runner, GC) without test fixtures rotting out of sync
// with the schema.
//
// Tests opt in via the MAIL_DATABASE_TEST_PG env var; if it's unset the
// helper calls t.Skip so go test in sandboxed CI keeps working.
//
// Usage:
//
//	func TestSomething(t *testing.T) {
//	    db, _ := pgtest.Open(t)            // Open also schedules cleanup
//	    // db is *storage.DB pointed at a fresh schema-migrated database
//	}
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ptudor/epistula-mail/database/migrations"
	"github.com/ptudor/epistula-mail/database/storage"
)

const envVar = "MAIL_DATABASE_TEST_PG"

// Open creates a fresh database, applies all migrations, and returns a
// *storage.DB pointing at it. Cleanup (dropping the database) is registered
// with t.Cleanup. Calls t.Skip when MAIL_DATABASE_TEST_PG is unset.
//
// The bootstrap DSN must point at a database the test user can connect to;
// the default is postgres://localhost/postgres. The actual test runs
// against a unique database named mail_database_test_<random>.
func Open(t *testing.T) (*storage.DB, string) {
	t.Helper()
	db, dsn := OpenBare(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := migrations.Apply(ctx, db.Pool()); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return db, dsn
}

// OpenBare creates a fresh EMPTY database (no migrations applied) and
// returns a *storage.DB pointing at it. Used by tests that bootstrap the
// schema another way (e.g. the schema.sql ⇄ migrations parity test).
// Cleanup is registered with t.Cleanup; skips when MAIL_DATABASE_TEST_PG
// is unset.
func OpenBare(t *testing.T) (*storage.DB, string) {
	t.Helper()
	bootstrapDSN := os.Getenv(envVar)
	if bootstrapDSN == "" {
		t.Skipf("set %s to run integration tests (e.g. postgres://localhost/postgres)", envVar)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	dbName := "mail_database_test_" + randomSuffix(t)

	testDSN, err := replaceDBName(bootstrapDSN, dbName)
	if err != nil {
		t.Fatalf("rewrite DSN: %v", err)
	}

	bootstrap, err := pgx.Connect(ctx, bootstrapDSN)
	if err != nil {
		t.Fatalf("connect bootstrap: %v", err)
	}
	if _, err := bootstrap.Exec(ctx, `CREATE DATABASE `+pgQuoteIdent(dbName)); err != nil {
		_ = bootstrap.Close(ctx)
		t.Fatalf("create database %s: %v", dbName, err)
	}
	if err := bootstrap.Close(ctx); err != nil {
		t.Fatalf("close bootstrap: %v", err)
	}

	db, err := storage.Open(ctx, storage.Config{
		DSN:              testDSN,
		StatementTimeout: 10 * time.Second,
	})
	if err != nil {
		dropDatabase(t, bootstrapDSN, dbName)
		t.Fatalf("open test pool: %v", err)
	}

	t.Cleanup(func() {
		db.Close()
		dropDatabase(t, bootstrapDSN, dbName)
	})

	return db, testDSN
}

func dropDatabase(t *testing.T, bootstrapDSN, dbName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, bootstrapDSN)
	if err != nil {
		t.Logf("pgtest cleanup: connect: %v", err)
		return
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `DROP DATABASE IF EXISTS `+pgQuoteIdent(dbName)+` WITH (FORCE)`); err != nil {
		t.Logf("pgtest cleanup: drop %s: %v", dbName, err)
	}
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// pgQuoteIdent wraps an identifier in double quotes, doubling embedded
// quotes per the SQL standard. The test database name is generated locally
// (no untrusted input) but doing this correctly keeps the helper safe for
// any future caller.
func pgQuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// replaceDBName swaps the path component of a URL-style DSN. Only the URL
// form (postgres://...) is supported; keyword-style DSNs are rejected with
// an error telling the operator to switch forms.
func replaceDBName(dsn, newDB string) (string, error) {
	// URL form: postgres://user:pass@host:port/dbname?args
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("parse DSN: %w", err)
		}
		u.Path = "/" + newDB
		return u.String(), nil
	}
	return "", fmt.Errorf("only URL-style DSNs are supported (postgres://...)")
}
