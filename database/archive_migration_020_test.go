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
	"github.com/ptudor/epistula-mail/database/storage"
)

// TestArchiveMigrationGrants pins migration 020's grants. Tables created after
// a role's `GRANT ... ON ALL TABLES` are invisible to it, so without these the
// deployed epistula-api role could not write a classification and epistula-imap
// could not run the sorter. The grants follow what each role can already do:
// a sidecar writer gets the taxonomy and classifications, a message-state
// writer also gets the move journal, and a role that can do neither — the
// reader, say — gets nothing.
func TestArchiveMigrationGrants(t *testing.T) {
	bootstrap := os.Getenv("MAIL_DATABASE_TEST_PG")
	if bootstrap == "" {
		t.Skip("set MAIL_DATABASE_TEST_PG to run integration tests")
	}
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	api := "m020_api_" + hex.EncodeToString(suffix[:])
	imap := "m020_imap_" + hex.EncodeToString(suffix[:])
	reader := "m020_reader_" + hex.EncodeToString(suffix[:])
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, bootstrap)
		if err != nil {
			t.Errorf("drop test roles: %v", err)
			return
		}
		defer conn.Close(ctx)
		for _, role := range []string{api, imap, reader} {
			if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize()); err != nil {
				t.Errorf("drop role %s: %v", role, err)
			}
		}
	})

	db, _ := pgtest.OpenBare(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := db.Pool()
	all, err := migrations.All()
	if err != nil {
		t.Fatal(err)
	}
	var m020 string
	for _, m := range all {
		if m.Version == 20 {
			m020 = m.SQL
			continue
		}
		if m.Version > 20 {
			continue
		}
		if _, err := pool.Exec(ctx, m.SQL); err != nil {
			t.Fatalf("migration %d: %v", m.Version, err)
		}
	}
	if m020 == "" {
		t.Fatal("migration 020 is not embedded")
	}

	// The roles as the deploy guides create them, before migration 020.
	q := func(s string) string { return pgx.Identifier{s}.Sanitize() }
	for _, stmt := range []string{
		"CREATE ROLE " + q(api),
		"CREATE ROLE " + q(imap),
		"CREATE ROLE " + q(reader),
		"GRANT SELECT ON messages TO " + q(api) + ", " + q(reader),
		"GRANT SELECT, INSERT, UPDATE ON message_annotations TO " + q(api),
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO " + q(imap),
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if _, err := pool.Exec(ctx, m020); err != nil {
		t.Fatalf("migration 020: %v", err)
	}

	has := func(role, table, priv string) bool {
		t.Helper()
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT has_table_privilege($1, $2, $3)`, role, table, priv).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	for _, c := range []struct {
		role, table, priv string
		want              bool
	}{
		{api, "archive_categories", "SELECT", true},
		{api, "archive_categories", "INSERT", false},
		{api, "message_classifications", "INSERT", true},
		{api, "message_classifications", "UPDATE", true},
		{api, "message_classifications", "DELETE", false},
		{api, "archive_moves", "SELECT", false},
		{imap, "archive_categories", "SELECT", true},
		{imap, "message_classifications", "INSERT", true},
		{imap, "archive_moves", "INSERT", true},
		{imap, "archive_moves", "UPDATE", true},
		{reader, "archive_categories", "SELECT", false},
		{reader, "message_classifications", "SELECT", false},
	} {
		if got := has(c.role, c.table, c.priv); got != c.want {
			t.Errorf("%s %s on %s = %v, want %v", c.role, c.priv, c.table, got, c.want)
		}
	}
	var seq bool
	if err := pool.QueryRow(ctx, `SELECT has_sequence_privilege($1, 'archive_moves_id_seq', 'USAGE')`, imap).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if !seq {
		t.Error("the message-state writer cannot allocate archive_moves ids")
	}
}

// TestArchiveMigrationPermission: both bootstraps accept the new
// write_classification permission and still refuse an unknown one.
func TestArchiveMigrationPermission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	migrated, _ := pgtest.Open(t)
	bare, _ := pgtest.OpenBare(t)
	schemaSQL, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bare.Pool().Exec(ctx, string(schemaSQL)); err != nil {
		t.Fatalf("apply schema.sql: %v", err)
	}
	for name, db := range map[string]*storage.DB{"migrations": migrated, "schema.sql": bare} {
		insert := func(perm string) error {
			_, err := db.Pool().Exec(ctx,
				`INSERT INTO api_tokens (name, token_hash, permissions, scope_all_mailboxes)
				 VALUES ($1, 'x', ARRAY[$2], true)`, "t-"+perm, perm)
			return err
		}
		if err := insert("write_classification"); err != nil {
			t.Errorf("%s: write_classification refused: %v", name, err)
		}
		if err := insert("move_mail"); err == nil {
			t.Errorf("%s: an unknown permission was accepted", name)
		}
	}
}
