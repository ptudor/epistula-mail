package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// TestMailboxDeleteRetiresTokenGrants is the CLI half of RA6X-012. Deleting an
// account must not leave a live credential believing it is scoped to it: the
// dead entry is removed, and a token whose LAST mailbox just went away is
// revoked rather than left holding an empty grant.
//
// Unrelated scopes and wildcard tokens are untouched.
func TestMailboxDeleteRetiresTokenGrants(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	victim := mustMailbox(t, ctx, db, "victim")
	keeper := mustMailbox(t, ctx, db, "keeper")

	mustToken(t, ctx, db, "only-victim", false, []int64{victim})
	mustToken(t, ctx, db, "victim-and-keeper", false, []int64{victim, keeper})
	mustToken(t, ctx, db, "only-keeper", false, []int64{keeper})
	mustToken(t, ctx, db, "wildcard", true, []int64{})

	cfg := writeRenameConfig(t, dsn, t.TempDir())
	if code := adminMailboxDelete([]string{"-config", cfg, "-name", "victim", "-yes"}); code != EX_OK {
		t.Fatalf("mailbox-delete exit=%d, want EX_OK", code)
	}

	for _, want := range []struct {
		token   string
		all     bool
		scope   string
		revoked bool
	}{
		// Its only grant is gone, so it authorizes nothing: revoked.
		{token: "only-victim", scope: "{}", revoked: true},
		// Keeps its other grant, stays live.
		{token: "victim-and-keeper", scope: idArray(keeper), revoked: false},
		// Untouched.
		{token: "only-keeper", scope: idArray(keeper), revoked: false},
		{token: "wildcard", all: true, scope: "{}", revoked: false},
	} {
		var gotAll, gotRevoked bool
		var gotScope string
		if err := db.Pool().QueryRow(ctx,
			`SELECT scope_all_mailboxes, scope_mailbox_ids::text, revoked_at IS NOT NULL
			   FROM api_tokens WHERE name = $1`, want.token,
		).Scan(&gotAll, &gotScope, &gotRevoked); err != nil {
			t.Fatalf("read token %s: %v", want.token, err)
		}
		if gotAll != want.all || gotScope != want.scope || gotRevoked != want.revoked {
			t.Errorf("token %s = (all=%v, scope=%s, revoked=%v), want (all=%v, scope=%s, revoked=%v)",
				want.token, gotAll, gotScope, gotRevoked, want.all, want.scope, want.revoked)
		}
	}

	// Recreating the name yields a different id, which no old token names.
	replacement := mustMailbox(t, ctx, db, "victim")
	if replacement == victim {
		t.Fatalf("recreated mailbox reused id %d; scope identity would not be durable", victim)
	}
	var stale int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM api_tokens
		  WHERE revoked_at IS NULL AND $1 = ANY(scope_mailbox_ids)`, replacement,
	).Scan(&stale); err != nil {
		t.Fatalf("count stale grants: %v", err)
	}
	if stale != 0 {
		t.Fatalf("%d live token(s) are scoped to the replacement account", stale)
	}
}

// TestAPITokenAddRejectsUnknownMailbox pins that a scope typo is caught at mint
// time. With scope stored as IDs, an unresolvable name would otherwise become a
// silently narrower (or empty) grant.
func TestAPITokenAddRejectsUnknownMailbox(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	mustMailbox(t, ctx, db, "real")
	cfg := writeRenameConfig(t, dsn, t.TempDir())

	code := adminAPITokenAdd([]string{
		"-config", cfg, "-name", "typo", "-mailboxes", "real,nosuchbox",
		"-permission", "read_metadata",
	})
	if code != EX_USAGE {
		t.Fatalf("api-token-add exit=%d, want EX_USAGE for an unknown mailbox", code)
	}

	var n int64
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM api_tokens WHERE name = 'typo'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatal("a rejected api-token-add committed a token row")
	}
}

// TestAPITokenAddStoresDurableIDs pins that a successful mint records IDs, and
// that the wildcard uses its own flag rather than a magic name.
func TestAPITokenAddStoresDurableIDs(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	id := mustMailbox(t, ctx, db, "real")
	cfg := writeRenameConfig(t, dsn, t.TempDir())

	if code := adminAPITokenAdd([]string{
		"-config", cfg, "-name", "scoped", "-mailboxes", "real", "-permission", "read_metadata",
	}); code != EX_OK {
		t.Fatalf("api-token-add (scoped) exit=%d, want EX_OK", code)
	}
	if code := adminAPITokenAdd([]string{
		"-config", cfg, "-name", "everything", "-mailboxes", "*", "-permission", "read_metadata",
	}); code != EX_OK {
		t.Fatalf("api-token-add (wildcard) exit=%d, want EX_OK", code)
	}

	var all bool
	var scope string
	if err := db.Pool().QueryRow(ctx,
		`SELECT scope_all_mailboxes, scope_mailbox_ids::text FROM api_tokens WHERE name = 'scoped'`,
	).Scan(&all, &scope); err != nil {
		t.Fatalf("read scoped token: %v", err)
	}
	if all || scope != idArray(id) {
		t.Fatalf("scoped token = (all=%v, %s), want (all=false, %s)", all, scope, idArray(id))
	}

	if err := db.Pool().QueryRow(ctx,
		`SELECT scope_all_mailboxes, scope_mailbox_ids::text FROM api_tokens WHERE name = 'everything'`,
	).Scan(&all, &scope); err != nil {
		t.Fatalf("read wildcard token: %v", err)
	}
	if !all || scope != "{}" {
		t.Fatalf("wildcard token = (all=%v, %s), want (all=true, {})", all, scope)
	}
}

func mustMailbox(t *testing.T, ctx context.Context, db *storage.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ($1, 'x') RETURNING id`, name,
	).Scan(&id); err != nil {
		t.Fatalf("insert mailbox %s: %v", name, err)
	}
	return id
}

func mustToken(t *testing.T, ctx context.Context, db *storage.DB, name string, all bool, scope []int64) {
	t.Helper()
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO api_tokens (name, token_hash, scope_all_mailboxes, scope_mailbox_ids, permissions)
		 VALUES ($1, 'x', $2, $3::bigint[], '{read_metadata}')`,
		name, all, scope,
	); err != nil {
		t.Fatalf("insert token %s: %v", name, err)
	}
}

func idArray(ids ...int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = formatMailboxIDs([]int64{id})
	}
	return "{" + strings.Join(parts, ",") + "}"
}
