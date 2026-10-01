package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// TestIngestRefusesWriteToAQuiescedMailbox is the RA6X-013 write barrier: while
// an operator is moving a mailbox's blob tenant tree, no write may be
// acknowledged for it. Delivery, IMAP APPEND and import all go through
// storage.Ingest, so one check covers all three.
func TestIngestRefusesWriteToAQuiescedMailbox(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	mbID := mustMailbox(t, ctx, db, "movingbox")
	store, params := ingestFixture(t, mbID, "movingbox")

	// Baseline: the write works.
	if _, err := db.Ingest(ctx, params); err != nil {
		t.Fatalf("baseline ingest: %v", err)
	}

	if _, err := db.Pool().Exec(ctx,
		`UPDATE mailboxes SET maintenance_at = now() WHERE id = $1`, mbID); err != nil {
		t.Fatalf("set maintenance: %v", err)
	}

	second := params
	second.RawSHA256Hex = writeBlobFixture(t, store, "movingbox", "Subject: second\r\n\r\nbody two\r\n")
	_, err := db.Ingest(ctx, second)
	if err == nil {
		t.Fatal("a write to a quiesced mailbox must be refused")
	}
	if !strings.Contains(err.Error(), "quiesced") {
		t.Fatalf("expected a maintenance error, got %v", err)
	}

	// Releasing maintenance restores service.
	if _, err := db.Pool().Exec(ctx,
		`UPDATE mailboxes SET maintenance_at = NULL WHERE id = $1`, mbID); err != nil {
		t.Fatalf("clear maintenance: %v", err)
	}
	if _, err := db.Ingest(ctx, second); err != nil {
		t.Fatalf("ingest after release: %v", err)
	}
}

// TestIngestRefusesAStaleMailboxName is the identity revalidation: a caller
// holding a mailbox name resolved before a rename — which is every
// authenticated IMAP session, since it resolves once at LOGIN — must not be
// able to commit a row whose blob was written under the old tenant. That
// message would be acknowledged and permanently unreadable.
func TestIngestRefusesAStaleMailboxName(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	mbID := mustMailbox(t, ctx, db, "oldname")
	_, params := ingestFixture(t, mbID, "oldname")

	// The rename happens while the caller still holds "oldname".
	if _, err := db.Pool().Exec(ctx,
		`UPDATE mailboxes SET name = 'newname' WHERE id = $1`, mbID); err != nil {
		t.Fatalf("rename: %v", err)
	}

	_, err := db.Ingest(ctx, params)
	if err == nil {
		t.Fatal("an ingest using a stale mailbox name must be refused")
	}
	if !strings.Contains(err.Error(), "name changed") {
		t.Fatalf("expected an identity-changed error, got %v", err)
	}

	// Nothing was committed, so the retry after re-resolving succeeds.
	var n int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM messages m JOIN folders f ON f.id = m.folder_id WHERE f.mailbox_id = $1`,
		mbID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d message(s) committed under a stale identity", n)
	}
}

// TestIngestRefusesADeletedMailbox pins the third form of the same check: the
// mailbox is gone entirely.
func TestIngestRefusesADeletedMailbox(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	mbID := mustMailbox(t, ctx, db, "goner")
	_, params := ingestFixture(t, mbID, "goner")

	if _, err := db.Pool().Exec(ctx, `DELETE FROM mailboxes WHERE id = $1`, mbID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.Ingest(ctx, params); err == nil {
		t.Fatal("an ingest into a deleted mailbox must be refused")
	}
}

// TestMailboxRenameRequiresMaintenance pins that the rename refuses to run
// without the barrier. "Stop Postfix" is not enough — it leaves IMAP sessions,
// imports and GC free to act on the database/filesystem disagreement.
func TestMailboxRenameRequiresMaintenance(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	mustMailbox(t, ctx, db, "oldbox")
	root := t.TempDir()
	mustTenantDir(t, root, "newbox")
	cfg := writeRenameConfig(t, dsn, root)

	if code := adminMailboxRename([]string{"-config", cfg, "-from", "oldbox", "-to", "newbox", "-yes"}); code != EX_USAGE {
		t.Fatalf("rename without maintenance exit=%d, want EX_USAGE", code)
	}
	if name := mailboxNameByID(t, ctx, db, "oldbox"); name != "oldbox" {
		t.Fatalf("mailbox was renamed anyway (now %q)", name)
	}

	// With maintenance on, the same rename proceeds.
	if code := adminMailboxMaintenance([]string{"-config", cfg, "-name", "oldbox", "-on"}); code != EX_OK {
		t.Fatalf("maintenance -on exit=%d, want EX_OK", code)
	}
	if code := adminMailboxRename([]string{"-config", cfg, "-from", "oldbox", "-to", "newbox", "-yes"}); code != EX_OK {
		t.Fatalf("rename with maintenance exit=%d, want EX_OK", code)
	}

	// A successful rename releases the barrier in the same transaction.
	var maintenanceAt *time.Time
	if err := db.Pool().QueryRow(ctx,
		`SELECT maintenance_at FROM mailboxes WHERE name = 'newbox'`).Scan(&maintenanceAt); err != nil {
		t.Fatalf("read maintenance: %v", err)
	}
	if maintenanceAt != nil {
		t.Fatal("a successful rename must release maintenance")
	}
}

// TestMailboxRenameRefusesPopulatedAccountWithNoTree covers an unmounted
// storage root with populated database rows: with neither tenant tree
// present, the filesystem alone cannot distinguish an account that has never
// stored a message from a storage root that failed to mount. The database can,
// so it must be asked instead of inferring "no blobs to move" from absence.
func TestMailboxRenameRefusesPopulatedAccountWithNoTree(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	mbID := mustMailbox(t, ctx, db, "populated")
	_, params := ingestFixture(t, mbID, "populated")
	if _, err := db.Ingest(ctx, params); err != nil {
		t.Fatalf("seed message: %v", err)
	}

	// A DIFFERENT, empty root: exactly what an unmounted dataset looks like.
	unmounted := t.TempDir()
	cfg := writeRenameConfig(t, dsn, unmounted)
	if code := adminMailboxMaintenance([]string{"-config", cfg, "-name", "populated", "-on"}); code != EX_OK {
		t.Fatalf("maintenance -on exit=%d", code)
	}

	if code := adminMailboxRename([]string{"-config", cfg, "-from", "populated", "-to", "renamed", "-yes"}); code != EX_CONFIG {
		t.Fatalf("rename with an unmounted root exit=%d, want EX_CONFIG", code)
	}
	if name := mailboxNameByID(t, ctx, db, "populated"); name != "populated" {
		t.Fatalf("mailbox was renamed against an unmounted root (now %q)", name)
	}
}

// TestMailboxRenameVerifiesBlobsAtDestination pins that a partial move is
// caught before the row is committed, rather than discovered later as a user's
// missing mail.
func TestMailboxRenameVerifiesBlobsAtDestination(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	root := t.TempDir()
	mbID := mustMailbox(t, ctx, db, "src")
	store := blob.NewStore(root)
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}
	params := ingestParamsFor(t, store, mbID, "src", "Subject: keepme\r\n\r\nthe only copy\r\n")
	if _, err := db.Ingest(ctx, params); err != nil {
		t.Fatalf("seed message: %v", err)
	}

	// Simulate a move that created the destination but did not carry the
	// blobs across.
	if err := os.RemoveAll(filepath.Join(root, "src")); err != nil {
		t.Fatalf("remove source tree: %v", err)
	}
	mustTenantDir(t, root, "dst")

	cfg := writeRenameConfig(t, dsn, root)
	if code := adminMailboxMaintenance([]string{"-config", cfg, "-name", "src", "-on"}); code != EX_OK {
		t.Fatalf("maintenance -on exit=%d", code)
	}
	if code := adminMailboxRename([]string{"-config", cfg, "-from", "src", "-to", "dst", "-yes"}); code != EX_USAGE {
		t.Fatalf("rename with an incomplete move exit=%d, want EX_USAGE", code)
	}
	if name := mailboxNameByID(t, ctx, db, "src"); name != "src" {
		t.Fatalf("mailbox was renamed despite missing blobs (now %q)", name)
	}

	// Complete the move; now it is allowed.
	if err := os.Rename(filepath.Join(root, "dst"), filepath.Join(root, "dst.empty")); err != nil {
		t.Fatalf("clear stub: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(root, "dst.empty")); err != nil {
		t.Fatalf("remove stub: %v", err)
	}
	rebuildTenantTree(t, "dst", store, params)

	if code := adminMailboxRename([]string{"-config", cfg, "-from", "src", "-to", "dst", "-yes"}); code != EX_OK {
		t.Fatalf("rename with a complete move exit=%d, want EX_OK", code)
	}
}

// --- helpers ---------------------------------------------------------------

func mailboxNameByID(t *testing.T, ctx context.Context, db *storage.DB, anyName string) string {
	t.Helper()
	var got string
	err := db.Pool().QueryRow(ctx, `SELECT name FROM mailboxes WHERE name = $1`, anyName).Scan(&got)
	if err != nil {
		return ""
	}
	return got
}

// ingestFixture builds a blob store and IngestParams for one small message
// already written under the tenant.
func ingestFixture(t *testing.T, mailboxID int64, tenant string) (*blob.Store, storage.IngestParams) {
	t.Helper()
	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}
	return store, ingestParamsFor(t, store, mailboxID, tenant, "Subject: one\r\n\r\nbody one\r\n")
}

func ingestParamsFor(t *testing.T, store *blob.Store, mailboxID int64, tenant, raw string) storage.IngestParams {
	t.Helper()
	msg, err := gcTestParser().Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	when := time.Now().UTC()
	sha := writeBlobAt(t, store, tenant, when, []byte(raw))
	return storage.IngestParams{
		MailboxID:    mailboxID,
		MailboxName:  tenant,
		FolderName:   "INBOX",
		EnvelopeTo:   tenant + "@example.invalid",
		RawSHA256Hex: sha,
		RawSize:      int64(len(raw)),
		RawBlobDate:  when,
		Message:      msg,
	}
}

func writeBlobFixture(t *testing.T, store *blob.Store, tenant, raw string) string {
	t.Helper()
	return writeBlobAt(t, store, tenant, time.Now().UTC(), []byte(raw))
}

func writeBlobAt(t *testing.T, store *blob.Store, tenant string, when time.Time, raw []byte) string {
	t.Helper()
	w, err := store.NewWriter(blob.KindRaw, blob.Tenant(tenant), blob.BucketFromTime(when))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sha, _, _, err := w.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	return sha
}

// rebuildTenantTree writes the message's blob under a new tenant name, standing
// in for the operator's completed `zfs rename`.
func rebuildTenantTree(t *testing.T, to string, store *blob.Store, params storage.IngestParams) {
	t.Helper()
	raw := []byte("Subject: keepme\r\n\r\nthe only copy\r\n")
	writeBlobAt(t, store, to, params.RawBlobDate, raw)
}
