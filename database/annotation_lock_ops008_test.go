package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ptudor/epistula-mail/database/migrations"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestAnnotationLockGrantFollowsAnnotationWriters pins migration 019's grants
// (OPS-008). mail_lock_message_for_annotation takes a row lock with its
// owner's privileges, so who may call it matters: a role that can connect but
// write no annotation must not be able to hold message rows locked and block
// EXPUNGE and MOVE. The migration revokes PUBLIC's default EXECUTE and grants
// it to the roles that can insert annotations, which on a deployed store is
// the epistula_api role that existed when the migration ran.
func TestAnnotationLockGrantFollowsAnnotationWriters(t *testing.T) {
	bootstrap := os.Getenv("MAIL_DATABASE_TEST_PG")
	if bootstrap == "" {
		t.Skip("set MAIL_DATABASE_TEST_PG to run integration tests")
	}
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	writer := "ops008_writer_" + hex.EncodeToString(suffix[:])
	other := "ops008_other_" + hex.EncodeToString(suffix[:])
	// Registered before pgtest.Open, so it runs after the test database is
	// dropped: a role cannot be dropped while a database holds its grants.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, bootstrap)
		if err != nil {
			t.Errorf("drop test roles: %v", err)
			return
		}
		defer conn.Close(ctx)
		for _, role := range []string{writer, other} {
			if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize()); err != nil {
				t.Errorf("drop role %s: %v", role, err)
			}
		}
	})

	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := db.Pool()

	for _, stmt := range []string{
		"CREATE ROLE " + pgx.Identifier{writer}.Sanitize(),
		"CREATE ROLE " + pgx.Identifier{other}.Sanitize(),
		"GRANT SELECT ON messages TO " + pgx.Identifier{writer}.Sanitize() + ", " + pgx.Identifier{other}.Sanitize(),
		"GRANT SELECT, INSERT, UPDATE ON message_annotations TO " + pgx.Identifier{writer}.Sanitize(),
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	canExecute := func(role string) bool {
		t.Helper()
		var ok bool
		if err := pool.QueryRow(ctx,
			`SELECT has_function_privilege($1, 'mail_lock_message_for_annotation(bigint)', 'EXECUTE')`, role,
		).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	// Both roles were created after the migration ran: neither may call it,
	// which also shows that PUBLIC may not.
	if canExecute(writer) || canExecute(other) {
		t.Fatal("the lock function is executable by roles the migration never granted it to")
	}

	// Re-running the migration, as happens on a store where the annotation
	// writer's role already exists, grants the writer and only the writer.
	all, err := migrations.All()
	if err != nil {
		t.Fatal(err)
	}
	var sql string
	for _, m := range all {
		if m.Version == 19 {
			sql = m.SQL
		}
	}
	if sql == "" {
		t.Fatal("migration 019 is not embedded")
	}
	if _, err := pool.Exec(ctx, sql); err != nil {
		t.Fatalf("re-run migration 019: %v", err)
	}
	if !canExecute(writer) {
		t.Error("the annotation writer was not granted the lock function")
	}
	if canExecute(other) {
		t.Error("a role that writes no annotations was granted the lock function")
	}

	var definer bool
	var config []string
	if err := pool.QueryRow(ctx,
		`SELECT prosecdef, proconfig FROM pg_proc WHERE proname = 'mail_lock_message_for_annotation'`,
	).Scan(&definer, &config); err != nil {
		t.Fatal(err)
	}
	if !definer || len(config) != 1 || config[0] != "search_path=pg_catalog, pg_temp" {
		t.Errorf("security definer %v, config %q; want a definer with a pinned search_path", definer, config)
	}
}
