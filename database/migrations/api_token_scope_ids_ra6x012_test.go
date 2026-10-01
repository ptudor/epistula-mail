package migrations_test

import (
	"context"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/migrations"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestMigration012BindsScopeToDurableIDs pins the RA6X-012 backfill against a
// database that already carries name-scoped tokens: wildcard semantics are
// preserved, live names resolve to durable IDs, a name that no longer exists
// contributes nothing, and a live token left with no grant at all is revoked
// rather than stored with an empty scope.
func TestMigration012BindsScopeToDurableIDs(t *testing.T) {
	db, _ := pgtest.OpenBare(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	all, err := migrations.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}

	// Apply everything up to and including 011 — the schema as it stood when
	// scope was a list of names.
	for _, m := range all {
		if m.Version >= 12 {
			break
		}
		if _, err := db.Pool().Exec(ctx, m.SQL); err != nil {
			t.Fatalf("apply migration %d: %v", m.Version, err)
		}
		if _, err := db.Pool().Exec(ctx,
			`INSERT INTO schema_versions (version, description) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			m.Version, m.Description,
		); err != nil {
			t.Fatalf("record migration %d: %v", m.Version, err)
		}
	}

	var aliceID, bobID int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('alice', 'x') RETURNING id`,
	).Scan(&aliceID); err != nil {
		t.Fatalf("insert alice: %v", err)
	}
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('bob', 'x') RETURNING id`,
	).Scan(&bobID); err != nil {
		t.Fatalf("insert bob: %v", err)
	}

	// Name-scoped tokens as migration 006 stored them. "ghost" names an
	// account that was deleted at some point — the exact state that made the
	// old scheme dangerous.
	for _, tok := range []struct {
		name    string
		scope   string
		revoked bool
	}{
		{"wildcard", "{*}", false},
		{"alice-only", "{alice}", false},
		{"alice-and-ghost", "{alice,ghost}", false},
		{"ghost-only", "{ghost}", false},
		{"ghost-only-revoked", "{ghost}", true},
	} {
		revokedAt := "NULL"
		if tok.revoked {
			revokedAt = "now()"
		}
		if _, err := db.Pool().Exec(ctx,
			`INSERT INTO api_tokens (name, token_hash, scope_mailboxes, permissions, revoked_at)
			 VALUES ($1, 'x', $2::text[], '{read_metadata}', `+revokedAt+`)`,
			tok.name, tok.scope,
		); err != nil {
			t.Fatalf("insert token %s: %v", tok.name, err)
		}
	}

	// Now apply 012.
	if _, err := migrations.Apply(ctx, db.Pool()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	for _, want := range []struct {
		token   string
		all     bool
		scope   []int64
		revoked bool
	}{
		{token: "wildcard", all: true, scope: []int64{}},
		{token: "alice-only", scope: []int64{aliceID}},
		// The live name survives; the deleted one contributes nothing and must
		// never be reinterpreted as access to a future "ghost".
		{token: "alice-and-ghost", scope: []int64{aliceID}},
		// Nothing left to authorize: revoked, not left with an empty grant.
		{token: "ghost-only", scope: []int64{}, revoked: true},
		// Already history; stays revoked, and an empty scope is storable.
		{token: "ghost-only-revoked", scope: []int64{}, revoked: true},
	} {
		var gotAll, gotRevoked bool
		var gotScope []int64
		if err := db.Pool().QueryRow(ctx,
			`SELECT scope_all_mailboxes, scope_mailbox_ids, revoked_at IS NOT NULL
			   FROM api_tokens WHERE name = $1`, want.token,
		).Scan(&gotAll, &gotScope, &gotRevoked); err != nil {
			t.Fatalf("read token %s: %v", want.token, err)
		}
		if gotAll != want.all || gotRevoked != want.revoked || !equalIDs(gotScope, want.scope) {
			t.Errorf("token %s = (all=%v, scope=%v, revoked=%v), want (all=%v, scope=%v, revoked=%v)",
				want.token, gotAll, gotScope, gotRevoked, want.all, want.scope, want.revoked)
		}
	}

	// The name column is gone, so nothing can read scope by name any more.
	var exists bool
	if err := db.Pool().QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			 WHERE table_schema = 'public' AND table_name = 'api_tokens'
			   AND column_name = 'scope_mailboxes')`).Scan(&exists); err != nil {
		t.Fatalf("column check: %v", err)
	}
	if exists {
		t.Error("api_tokens.scope_mailboxes still exists; a name-keyed reader could survive")
	}
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
