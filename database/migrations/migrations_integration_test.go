package migrations_test

import (
	"context"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/migrations"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestApplyIsIdempotent runs migrations.Apply against a database that's
// already been migrated (pgtest.Open ran it once at construction). A
// second Apply must be a no-op and must not error.
func TestApplyIsIdempotent(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	applied, err := migrations.Apply(ctx, db.Pool())
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("second Apply applied %d migrations, want 0 (idempotent)", len(applied))
	}
}

// TestAppliedVersionsMatchAll asserts every embedded migration is recorded
// in schema_versions after pgtest.Open finishes its initial Apply.
func TestAppliedVersionsMatchAll(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	all, err := migrations.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	applied, err := migrations.AppliedVersions(ctx, db.Pool())
	if err != nil {
		t.Fatalf("AppliedVersions: %v", err)
	}
	for _, m := range all {
		if _, ok := applied[m.Version]; !ok {
			t.Errorf("migration %03d (%s) not recorded in schema_versions", m.Version, m.Description)
		}
	}
	if len(applied) != len(all) {
		t.Errorf("applied=%d, all=%d", len(applied), len(all))
	}
}

// TestSchemaHasDomainACL is a smoke test that migration 002 created the
// table the resolver depends on.
func TestSchemaHasDomainACL(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var exists bool
	if err := db.Pool().QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM information_schema.tables
			 WHERE table_schema = current_schema() AND table_name = 'domain_acl'
		)`).Scan(&exists); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !exists {
		t.Error("domain_acl table not present after migrations applied")
	}
}

// TestSchemaHasBlobDateColumn is a smoke test that migration 003 added
// the raw_blob_date column.
func TestSchemaHasBlobDateColumn(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var exists bool
	if err := db.Pool().QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM information_schema.columns
			 WHERE table_schema = current_schema()
			   AND table_name = 'messages'
			   AND column_name = 'raw_blob_date'
		)`).Scan(&exists); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !exists {
		t.Error("messages.raw_blob_date column not present after migrations applied")
	}
}

func TestSchemaDropsLegacyAdminTables(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, table := range []string{"admin_users", "admin_sessions"} {
		var exists bool
		if err := db.Pool().QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM information_schema.tables
				 WHERE table_schema = current_schema() AND table_name = $1
			)`, table).Scan(&exists); err != nil {
			t.Fatalf("probe %s: %v", table, err)
		}
		if exists {
			t.Errorf("%s should not be present after migrations applied", table)
		}
	}
}
