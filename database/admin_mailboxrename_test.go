package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// writeRenameConfig is writeAdminConfig with a caller-chosen storage root, so
// a test can stage the blob tenant directories mailbox-rename inspects.
func writeRenameConfig(t *testing.T, dsn, root string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "epistula-database.toml")
	body := "production = false\n\n[postgres]\ndsn = \"" + dsn + "\"\n\n[storage]\nroot = \"" + root + "\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// mustTenantBlob stages a tenant subtree that actually holds a blob — the
// only state that makes a rename unsafe.
func mustTenantBlob(t *testing.T, root, tenant string) {
	t.Helper()
	dir := filepath.Join(root, tenant, "raw", "2026", "08", "22", "ab", "cd")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir blob dir for %q: %v", tenant, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ab"+"cd"+"00.eml"), []byte("raw"), 0o640); err != nil {
		t.Fatalf("write blob for %q: %v", tenant, err)
	}
}

// mustTenantDir stages an EMPTY blob tenant subtree — what `zfs create` of a
// successor dataset leaves behind.
func mustTenantDir(t *testing.T, root, tenant string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, tenant, "raw"), 0o750); err != nil {
		t.Fatalf("mkdir tenant %q: %v", tenant, err)
	}
}

func mailboxName(t *testing.T, ctx context.Context, db *storage.DB, id int64) string {
	t.Helper()
	var name string
	if err := db.Pool().QueryRow(ctx, `SELECT name FROM mailboxes WHERE id = $1`, id).Scan(&name); err != nil {
		t.Fatalf("read mailbox name: %v", err)
	}
	return name
}

// TestMailboxRenameRefusesUntilTenantDirMoved is the central safety property:
// the mailbox name IS the on-disk blob tenant, so renaming the row while the
// blobs still sit under the old directory would split the store. The command
// must refuse and leave Postgres untouched.
func TestMailboxRenameRefusesUntilTenantDirMoved(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	id := gcMustMailbox(t, ctx, db, "oldbox")
	root := t.TempDir()
	mustTenantBlob(t, root, "oldbox") // blobs still under the old name
	cfg := writeRenameConfig(t, dsn, root)

	code := adminMailboxRename([]string{"-config", cfg, "-from", "oldbox", "-to", "newbox", "-yes"})
	if code != EX_USAGE {
		t.Fatalf("rename with unmoved tenant dir exit=%d, want EX_USAGE(%d)", code, EX_USAGE)
	}
	if got := mailboxName(t, ctx, db, id); got != "oldbox" {
		t.Fatalf("mailbox renamed despite refusal: got %q, want %q", got, "oldbox")
	}
}

// TestMailboxRenameRefusesWhenBothTenantDirsExist covers the ambiguous case:
// the old name still holds blobs AND the new tree is already there, so
// picking either one would strand the other's.
func TestMailboxRenameRefusesWhenBothTenantDirsExist(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	id := gcMustMailbox(t, ctx, db, "oldbox")
	root := t.TempDir()
	mustTenantBlob(t, root, "oldbox")
	mustTenantDir(t, root, "newbox")
	cfg := writeRenameConfig(t, dsn, root)

	code := adminMailboxRename([]string{"-config", cfg, "-from", "oldbox", "-to", "newbox", "-yes"})
	if code != EX_USAGE {
		t.Fatalf("rename with both tenant dirs exit=%d, want EX_USAGE(%d)", code, EX_USAGE)
	}
	if got := mailboxName(t, ctx, db, id); got != "oldbox" {
		t.Fatalf("mailbox renamed despite refusal: got %q, want %q", got, "oldbox")
	}
}

// TestMailboxRenameCarriesContentAndRescopesTokens is the happy path: the
// tenant dir has already moved, so the row rename proceeds. Folders, messages
// and aliases must follow (they hang off mailbox_id), and every api_token scope
// must come through unchanged: since migration 012 a scope holds durable
// mailbox ids rather than names, so the rename has nothing to rewrite
// (RA6X-012).
func TestMailboxRenameCarriesContentAndRescopesTokens(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mbID := gcMustMailbox(t, ctx, db, "oldbox")

	var folderID int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
		 VALUES ($1, 'INBOX', 1, 2) RETURNING id`, mbID,
	).Scan(&folderID); err != nil {
		t.Fatalf("insert folder: %v", err)
	}
	// raw_blob_date is pinned to the UTC day, not left to the column's
	// CURRENT_DATE default: blob paths bucket by UTC, while CURRENT_DATE
	// follows the database session's timezone, so the two disagree for part of
	// every day and the fixture's blob would be sought under the wrong date.
	// (Real ingest already normalizes to UTC — R-064.)
	blobDay := time.Now().UTC()
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO messages (folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date, subject)
		 VALUES ($1, 1, decode($3, 'hex'), $2, 42, now(), 'kept')`, folderID, blobDay, renameFixtureSHA(),
	); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	var domainID int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO domains (name) VALUES ('rename.invalid') RETURNING id`,
	).Scan(&domainID); err != nil {
		t.Fatalf("insert domain: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO aliases (domain_id, localpart, mailbox_id) VALUES ($1, 'postmaster', $2)`,
		domainID, mbID,
	); err != nil {
		t.Fatalf("insert alias: %v", err)
	}

	// Three tokens: one live and scoped to this mailbox, one revoked and
	// scoped to it (history), one live wildcard. Since migration 012 scope is
	// a durable mailbox ID, so NONE of them may change: the rename does not
	// change the id, and a token scoped to this account stays scoped to this
	// account (RA6X-012).
	for _, tok := range []struct {
		name    string
		scope   []int64
		all     bool
		perms   string
		revoked bool
	}{
		{name: "live-scoped", scope: []int64{mbID}, perms: "{read_metadata}"},
		{name: "revoked-scoped", scope: []int64{mbID}, perms: "{read_metadata}", revoked: true},
		{name: "live-wildcard", scope: []int64{}, all: true, perms: "{read_metadata}"},
	} {
		revokedAt := "NULL"
		if tok.revoked {
			revokedAt = "now()"
		}
		if _, err := db.Pool().Exec(ctx,
			`INSERT INTO api_tokens (name, token_hash, scope_all_mailboxes, scope_mailbox_ids, permissions, revoked_at)
			 VALUES ($1, 'x', $2, $3::bigint[], $4::text[], `+revokedAt+`)`,
			tok.name, tok.all, tok.scope, tok.perms,
		); err != nil {
			t.Fatalf("insert token %s: %v", tok.name, err)
		}
	}

	root := t.TempDir()
	mustTenantDir(t, root, "newbox") // operator already moved it
	// The rename verifies that every blob the database says this mailbox owns
	// is present under the DESTINATION tree before it commits (RA6X-013), so
	// the fixture's message needs its blob to have arrived with the move.
	mustSeededBlob(t, root, "newbox", renameFixtureSHA(), blobDay)
	cfg := writeRenameConfig(t, dsn, root)

	mustQuiesce(t, cfg, "oldbox")
	if code := adminMailboxRename([]string{"-config", cfg, "-from", "oldbox", "-to", "newbox", "-yes"}); code != EX_OK {
		t.Fatalf("rename exit=%d, want EX_OK", code)
	}

	if got := mailboxName(t, ctx, db, mbID); got != "newbox" {
		t.Fatalf("mailbox name = %q, want %q", got, "newbox")
	}

	// Content follows the id, so it must still resolve through the new name.
	var folders, messages, aliases int64
	if err := db.Pool().QueryRow(ctx, `
		SELECT (SELECT count(*) FROM folders  WHERE mailbox_id = m.id),
		       (SELECT count(*) FROM messages WHERE folder_id IN (SELECT id FROM folders WHERE mailbox_id = m.id)),
		       (SELECT count(*) FROM aliases  WHERE mailbox_id = m.id)
		  FROM mailboxes m WHERE m.name = 'newbox'`,
	).Scan(&folders, &messages, &aliases); err != nil {
		t.Fatalf("recount: %v", err)
	}
	if folders != 1 || messages != 1 || aliases != 1 {
		t.Fatalf("content lost: folders=%d messages=%d aliases=%d, want 1/1/1", folders, messages, aliases)
	}

	// Every scope is byte-identical to what it was before the rename.
	for _, want := range []struct {
		token string
		all   bool
		scope string
	}{
		{token: "live-scoped", scope: fmt.Sprintf("{%d}", mbID)},
		{token: "revoked-scoped", scope: fmt.Sprintf("{%d}", mbID)},
		{token: "live-wildcard", all: true, scope: "{}"},
	} {
		var gotAll bool
		var got string
		if err := db.Pool().QueryRow(ctx,
			`SELECT scope_all_mailboxes, scope_mailbox_ids::text FROM api_tokens WHERE name = $1`, want.token,
		).Scan(&gotAll, &got); err != nil {
			t.Fatalf("read token %s: %v", want.token, err)
		}
		if gotAll != want.all || got != want.scope {
			t.Fatalf("token %s scope = (all=%v, %s), want (all=%v, %s)",
				want.token, gotAll, got, want.all, want.scope)
		}
	}
}

// TestMailboxRenameNoTenantDirIsAllowed: a mailbox that has never stored a
// message has no tenant subtree at all, so there is nothing to move and the
// rename should just work.
func TestMailboxRenameNoTenantDirIsAllowed(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	id := gcMustMailbox(t, ctx, db, "emptybox")
	cfg := writeRenameConfig(t, dsn, t.TempDir())

	mustQuiesce(t, cfg, "emptybox")
	if code := adminMailboxRename([]string{"-config", cfg, "-from", "EmptyBox ", "-to", "renamedbox", "-yes"}); code != EX_OK {
		t.Fatalf("rename exit=%d, want EX_OK", code)
	}
	if got := mailboxName(t, ctx, db, id); got != "renamedbox" {
		t.Fatalf("mailbox name = %q, want %q", got, "renamedbox")
	}
}

// TestMailboxRenameRejectsBadInput covers the argument-validation gate, which
// runs before any config load or database connection.
func TestMailboxRenameRejectsBadInput(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	gcMustMailbox(t, ctx, db, "abox")
	gcMustMailbox(t, ctx, db, "bbox")
	cfg := writeRenameConfig(t, dsn, t.TempDir())

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"missing -to", []string{"-config", cfg, "-from", "abox", "-yes"}},
		{"missing -from", []string{"-config", cfg, "-to", "cbox", "-yes"}},
		{"same name", []string{"-config", cfg, "-from", "abox", "-to", "ABox", "-yes"}},
		{"missing -yes", []string{"-config", cfg, "-from", "abox", "-to", "cbox"}},
		{"invalid target charset", []string{"-config", cfg, "-from", "abox", "-to", "../escape", "-yes"}},
		{"target leads with dash", []string{"-config", cfg, "-from", "abox", "-to", "_nope", "-yes"}},
		{"source not found", []string{"-config", cfg, "-from", "nosuchbox", "-to", "cbox", "-yes"}},
		{"target already exists", []string{"-config", cfg, "-from", "abox", "-to", "bbox", "-yes"}},
	} {
		if code := adminMailboxRename(tc.args); code != EX_USAGE {
			t.Errorf("%s: exit=%d, want EX_USAGE(%d)", tc.name, code, EX_USAGE)
		}
	}

	// None of the above may have mutated anything.
	var n int
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM mailboxes WHERE name IN ('abox', 'bbox')`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("mailbox rows changed after rejected renames: %d, want 2", n)
	}
}

// TestMailboxRenameAllowsEmptySuccessorDataset is the deployment case that
// motivated testing for blobs rather than for a directory: on a
// per-mailbox-dataset host the operator moves the populated tenant aside with
// `zfs rename` and immediately creates an empty dataset back under the old
// name, ready for the mailbox that will take it. That empty tree strands
// nothing, so the rename must proceed rather than refuse.
func TestMailboxRenameAllowsEmptySuccessorDataset(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	id := gcMustMailbox(t, ctx, db, "oldbox")
	root := t.TempDir()
	mustTenantBlob(t, root, "newbox") // the moved-aside, populated tree
	mustTenantDir(t, root, "oldbox")  // freshly created, empty successor
	cfg := writeRenameConfig(t, dsn, root)

	mustQuiesce(t, cfg, "oldbox")
	if code := adminMailboxRename([]string{"-config", cfg, "-from", "oldbox", "-to", "newbox", "-yes"}); code != EX_OK {
		t.Fatalf("rename with empty successor dataset exit=%d, want EX_OK", code)
	}
	if got := mailboxName(t, ctx, db, id); got != "newbox" {
		t.Fatalf("mailbox name = %q, want %q", got, "newbox")
	}
}

// mustQuiesce puts the mailbox into maintenance, which mailbox-rename now
// requires: the rename is inherently two-phase (move the tenant tree, then
// update the row) and every writer plus GC has to be held off across both
// (RA6X-013).
func mustQuiesce(t *testing.T, cfg, name string) {
	t.Helper()
	if code := adminMailboxMaintenance([]string{"-config", cfg, "-name", name, "-on"}); code != EX_OK {
		t.Fatalf("mailbox-maintenance -on %s: exit %d", name, code)
	}
}

// mustSeededBlob places a raw blob at the canonical path for shaHex under
// tenant, in the UTC bucket for `when` — which must be the same value the
// fixture stored in messages.raw_blob_date.
func mustSeededBlob(t *testing.T, root, tenant, shaHex string, when time.Time) {
	t.Helper()
	store := blob.NewStore(root)
	path, err := store.PathFor(blob.KindRaw, blob.Tenant(tenant), blob.BucketFromTime(when.UTC()), shaHex)
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("r", 42)), 0o640); err != nil {
		t.Fatalf("write blob %s: %v", path, err)
	}
}

func renameFixtureSHA() string {
	sum := sha256.Sum256([]byte(strings.Repeat("r", 42)))
	return hex.EncodeToString(sum[:])
}
