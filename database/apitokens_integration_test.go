package main

import (
	"context"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/auth"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestAPITokenSchemaSemantics locks down the migration-006 contract the
// admin CLI and epistula-api both rely on: live-name uniqueness with revoke
// freeing the name, permission whitelisting at the CHECK level, and the
// stored hash verifying the minted secret.
func TestAPITokenSchemaSemantics(t *testing.T) {
	db, _ := pgtest.Open(t)
	pool := db.Pool()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	secret, err := auth.GenerateAPITokenSecret()
	if err != nil {
		t.Fatalf("GenerateAPITokenSecret: %v", err)
	}
	hash, err := auth.HashPassword(secret, auth.DefaultParams())
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	// Scope is durable mailbox IDs (migration 012, RA6X-012).
	var mailboxID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('jdoe', 'x') RETURNING id`,
	).Scan(&mailboxID); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}

	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO api_tokens (name, token_hash, scope_mailbox_ids, permissions)
		 VALUES ('summarizer', $1, $2, $3) RETURNING id`,
		hash, []int64{mailboxID}, []string{auth.PermissionReadContent, auth.PermissionWriteAnnotation},
	).Scan(&id); err != nil {
		t.Fatalf("insert token: %v", err)
	}

	// The stored hash verifies the secret embedded in the presented form.
	presented := auth.FormatAPIToken(id, secret)
	gotID, gotSecret, err := auth.ParseAPIToken(presented)
	if err != nil || gotID != id {
		t.Fatalf("ParseAPIToken(%q) = (%d, _, %v), want id %d", presented, gotID, err, id)
	}
	var storedHash string
	if err := pool.QueryRow(ctx,
		`SELECT token_hash FROM api_tokens WHERE id = $1`, gotID,
	).Scan(&storedHash); err != nil {
		t.Fatalf("hash lookup: %v", err)
	}
	if ok, err := auth.VerifyPassword(gotSecret, storedHash); err != nil || !ok {
		t.Fatalf("VerifyPassword = (%v, %v), want (true, nil)", ok, err)
	}

	// A second live token with the same name is refused.
	_, err = pool.Exec(ctx,
		`INSERT INTO api_tokens (name, token_hash, scope_all_mailboxes, permissions)
		 VALUES ('summarizer', 'x', true, $1)`,
		[]string{auth.PermissionReadMetadata},
	)
	if err == nil {
		t.Fatal("duplicate live token name must violate idx_api_tokens_live_name")
	}

	// Revoking frees the name for a re-mint; the revoked row remains.
	if _, err := pool.Exec(ctx,
		`UPDATE api_tokens SET revoked_at = now() WHERE id = $1`, id,
	); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO api_tokens (name, token_hash, scope_all_mailboxes, permissions)
		 VALUES ('summarizer', 'y', true, $1)`,
		[]string{auth.PermissionReadMetadata},
	); err != nil {
		t.Fatalf("re-mint after revoke: %v", err)
	}

	// Unknown permission values are refused by the CHECK constraint.
	_, err = pool.Exec(ctx,
		`INSERT INTO api_tokens (name, token_hash, scope_all_mailboxes, permissions)
		 VALUES ('bad-perms', 'z', true, '{root}')`)
	if err == nil {
		t.Fatal("permissions outside the whitelist must violate the CHECK constraint")
	}

	// A live token with neither the wildcard nor any mailbox id authorizes
	// nothing, and must not be storable — a future reader could mistake an
	// empty scope for "unrestricted" (RA6X-012).
	_, err = pool.Exec(ctx,
		`INSERT INTO api_tokens (name, token_hash, permissions)
		 VALUES ('empty-scope', 'z', $1)`,
		[]string{auth.PermissionReadMetadata},
	)
	if err == nil {
		t.Fatal("an empty live scope must violate api_tokens_scope_nonempty")
	}

	// A REVOKED token may hold an empty scope: revoking is always allowed,
	// including for a token whose last mailbox has just been deleted, and the
	// historical row must stay storable.
	if _, err := pool.Exec(ctx,
		`INSERT INTO api_tokens (name, token_hash, permissions, revoked_at)
		 VALUES ('empty-scope-revoked', 'z', $1, now())`,
		[]string{auth.PermissionReadMetadata},
	); err != nil {
		t.Fatalf("a revoked token with an empty scope must be storable: %v", err)
	}
}

// TestMessageAnnotationsUpsert locks down the (message_id, model) keyed
// sidecar: idempotent replace per model, independent rows across models,
// and ON DELETE CASCADE from messages.
func TestMessageAnnotationsUpsert(t *testing.T) {
	db, _ := pgtest.Open(t)
	pool := db.Pool()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var mailboxID, folderID, messageID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('annot', 'x') RETURNING id`,
	).Scan(&mailboxID); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
		 VALUES ($1, 'INBOX', 1, 2) RETURNING id`, mailboxID,
	).Scan(&folderID); err != nil {
		t.Fatalf("insert folder: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO messages (folder_id, uid, raw_sha256, raw_size, internal_date, headers, bodystructure)
		 VALUES ($1, 1, '\x00'::bytea, 10, now(), '{}', '{}') RETURNING id`, folderID,
	).Scan(&messageID); err != nil {
		t.Fatalf("insert message: %v", err)
	}

	upsert := `INSERT INTO message_annotations (message_id, model, tags, category, summary, tokens_in, tokens_out)
	           VALUES ($1, $2, $3, $4, $5, $6, $7)
	           ON CONFLICT (message_id, model) DO UPDATE
	             SET tags = EXCLUDED.tags, category = EXCLUDED.category,
	                 summary = EXCLUDED.summary, tokens_in = EXCLUDED.tokens_in,
	                 tokens_out = EXCLUDED.tokens_out, created_at = now()`

	if _, err := pool.Exec(ctx, upsert, messageID, "haiku-4.5",
		[]string{"receipts", "travel"}, "Archive/Receipts", "a receipt", 1200, 80); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if _, err := pool.Exec(ctx, upsert, messageID, "haiku-4.5",
		[]string{"receipts"}, "Archive/Receipts", "really a receipt", 1300, 90); err != nil {
		t.Fatalf("replace upsert: %v", err)
	}
	if _, err := pool.Exec(ctx, upsert, messageID, "fable-5",
		[]string{"travel"}, nil, nil, nil, nil); err != nil {
		t.Fatalf("second-model upsert: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM message_annotations WHERE message_id = $1`, messageID,
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("annotation rows = %d, want 2 (one per model)", count)
	}
	var summary string
	if err := pool.QueryRow(ctx,
		`SELECT summary FROM message_annotations WHERE message_id = $1 AND model = 'haiku-4.5'`,
		messageID,
	).Scan(&summary); err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary != "really a receipt" {
		t.Errorf("summary = %q; replace-on-conflict did not apply", summary)
	}

	// Deleting the message cascades the sidecar.
	if _, err := pool.Exec(ctx, `DELETE FROM messages WHERE id = $1`, messageID); err != nil {
		t.Fatalf("delete message: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM message_annotations WHERE message_id = $1`, messageID,
	).Scan(&count); err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if count != 0 {
		t.Errorf("annotations after message delete = %d, want 0 (ON DELETE CASCADE)", count)
	}
}
