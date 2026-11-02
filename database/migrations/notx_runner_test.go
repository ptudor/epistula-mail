package migrations_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/migrations"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestApplyUpToStopsAtTheBound covers the rollout shape the no-transaction
// migrations are designed for: the additive ones first, then (after a deploy)
// the rest.
func TestApplyUpToStopsAtTheBound(t *testing.T) {
	db, _ := pgtest.OpenBare(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	applied, err := migrations.ApplyUpTo(ctx, db.Pool(), 29)
	if err != nil {
		t.Fatalf("ApplyUpTo(29): %v", err)
	}
	if n := len(applied); n == 0 || applied[n-1].Version != 29 {
		t.Fatalf("ApplyUpTo(29) applied through %d, want 29", applied[n-1].Version)
	}
	pending, err := migrations.Pending(ctx, db.Pool())
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) == 0 || pending[0].Version != 30 {
		t.Fatalf("pending after -to 29 = %v, want 030 first", pending)
	}

	// Between the two steps the schema must serve both generations of
	// binaries: the old columns are still there, and the new objects exist.
	for _, probe := range []struct{ name, sql string }{
		{"fts column kept", `SELECT 1 FROM information_schema.columns WHERE table_name='messages' AND column_name='fts'`},
		{"html_body column kept", `SELECT 1 FROM information_schema.columns WHERE table_name='messages' AND column_name='html_body'`},
		{"mail_fts exists", `SELECT 1 FROM pg_proc WHERE proname='mail_fts'`},
		{"idx_messages_search valid", `SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE c.relname='idx_messages_search' AND i.indisvalid`},
		{"queue mailbox_id nullable", `SELECT 1 FROM information_schema.columns WHERE table_name='annotation_pass_required' AND column_name='mailbox_id' AND is_nullable='YES'`},
	} {
		var one int
		if err := db.Pool().QueryRow(ctx, probe.sql).Scan(&one); err != nil {
			t.Errorf("%s: %v", probe.name, err)
		}
	}

	rest, err := migrations.Apply(ctx, db.Pool())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(rest) != len(pending) {
		t.Fatalf("Apply applied %d, want the %d pending", len(rest), len(pending))
	}
	var one int
	if err := db.Pool().QueryRow(ctx,
		`SELECT 1 FROM information_schema.columns WHERE table_name='messages' AND column_name='fts'`).Scan(&one); err == nil {
		t.Error("fts column survived migration 030")
	}

	if _, err := migrations.ApplyUpTo(ctx, db.Pool(), 999); !errors.Is(err, migrations.ErrNoSuchVersion) {
		t.Fatalf("ApplyUpTo(999) = %v, want ErrNoSuchVersion", err)
	}
}

// TestNoTransactionMigrationRebuildsAnInvalidIndex: an interrupted CREATE
// INDEX CONCURRENTLY leaves an INVALID index under the target name. A rerun's
// IF NOT EXISTS would skip it and record the migration as applied with an
// index the planner never uses, so the runner drops it first.
func TestNoTransactionMigrationRebuildsAnInvalidIndex(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Flipping indisvalid is the only way to manufacture the state without
	// racing a real build; it needs a superuser, which the local test
	// cluster has and CI may not.
	var super bool
	if err := db.Pool().QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super); err != nil {
		t.Fatal(err)
	}
	if !super {
		t.Skip("needs a superuser to mark an index invalid")
	}
	if _, err := db.Pool().Exec(ctx,
		`UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'idx_messages_search'::regclass`); err != nil {
		t.Fatalf("mark invalid: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `DELETE FROM schema_versions WHERE version = 27`); err != nil {
		t.Fatalf("forget 027: %v", err)
	}

	applied, err := migrations.Apply(ctx, db.Pool())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(applied) != 1 || applied[0].Version != 27 {
		t.Fatalf("applied %v, want just 027", applied)
	}
	var valid bool
	if err := db.Pool().QueryRow(ctx,
		`SELECT i.indisvalid FROM pg_index i WHERE i.indexrelid = 'idx_messages_search'::regclass`).Scan(&valid); err != nil {
		t.Fatalf("read index: %v", err)
	}
	if !valid {
		t.Fatal("idx_messages_search is still invalid after the rerun")
	}
}

// TestNoTransactionMigrationsAreRerunnable: every statement of a
// no-transaction migration is idempotent, so running one again after it
// completed is a no-op rather than an error. That is what makes "fix the
// cause and rerun migrate up" the whole recovery procedure.
func TestNoTransactionMigrationsAreRerunnable(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if _, err := db.Pool().Exec(ctx, `DELETE FROM schema_versions WHERE version IN (27, 28, 29)`); err != nil {
		t.Fatalf("forget versions: %v", err)
	}
	applied, err := migrations.Apply(ctx, db.Pool())
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if len(applied) != 3 {
		t.Fatalf("rerun applied %d, want 3", len(applied))
	}
	// One covering unique index, still attached as the constraint.
	var n int
	if err := db.Pool().QueryRow(ctx, `
		SELECT count(*) FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		 WHERE i.indrelid = 'messages'::regclass AND i.indisunique AND c.relname <> 'messages_pkey'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("messages has %d unique non-pkey indexes after a rerun, want 1", n)
	}
	var conname string
	if err := db.Pool().QueryRow(ctx, `
		SELECT conname FROM pg_constraint WHERE conrelid = 'messages'::regclass AND contype = 'u'`).Scan(&conname); err != nil {
		t.Fatal(err)
	}
	if conname != "messages_folder_uid_key" {
		t.Fatalf("unique constraint is %q, want messages_folder_uid_key", conname)
	}
}
