package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// TestSubmitPermissionMigration025: both bootstraps accept migration 025's
// submit_message permission, keep accepting every earlier one, and still
// refuse an unknown permission. A token minted by `admin api-token-add
// -permission submit_message` against a database that missed the migration
// would otherwise fail on the CHECK constraint at mint time, and a
// schema.sql bootstrap that drifted from the migration would refuse it.
func TestSubmitPermissionMigration025(t *testing.T) {
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
		insert := func(perms ...string) error {
			_, err := db.Pool().Exec(ctx,
				`INSERT INTO api_tokens (name, token_hash, permissions, scope_all_mailboxes)
				 VALUES ($1, 'x', $2, true)`, "t-"+perms[0], perms)
			return err
		}
		if err := insert("submit_message"); err != nil {
			t.Errorf("%s: submit_message refused: %v", name, err)
		}
		if err := insert("read_metadata", "read_content", "write_annotation", "write_classification"); err != nil {
			t.Errorf("%s: an earlier permission set was refused: %v", name, err)
		}
		if err := insert("send_mail"); err == nil {
			t.Errorf("%s: an unknown permission was accepted", name)
		}
		var applied bool
		if err := db.Pool().QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_versions WHERE version = 25)`).Scan(&applied); err != nil {
			t.Fatal(err)
		}
		if !applied {
			t.Errorf("%s: schema_versions does not record migration 25", name)
		}
	}
}
