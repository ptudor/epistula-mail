package main

import (
	"context"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestGCDoesNotReapAMovedTenantDuringMaintenance is the RA6X-013 data-loss
// case. A rename is two-phase: the blob tree moves to <storage_root>/<new>,
// then the row catches up. In between, that tree's directory name resolves to
// no mailbox — and "a tenant directory with no mailbox row is definitionally
// unreferenced" is exactly the rule GC uses to reap a deleted account. Applied
// here it deletes a live account's entire archive.
//
// While any mailbox is quiesced, GC must therefore leave unresolvable tenants
// alone. It cannot tell which one is the in-flight rename: the maintenance flag
// lives on the mailbox row, which still carries the OLD name.
func TestGCDoesNotReapAMovedTenantDuringMaintenance(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}

	// A live account, quiesced for a rename to "newname".
	var mbID int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash, maintenance_at)
		 VALUES ('oldname', 'x', now()) RETURNING id`,
	).Scan(&mbID); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}

	// Its blobs have already been moved to the destination name, which no
	// mailbox row claims yet.
	raw := []byte(gcTestMessage)
	blobDate := time.Date(2003, 6, 15, 10, 0, 0, 0, time.UTC)
	bucket := blob.BucketFromTime(blobDate)
	_, movedPath := writeTestBlob(t, store, "newname", bucket, raw)
	backdateFile(t, movedPath, 48*time.Hour)

	// A genuinely orphaned tenant, to prove the pass is not simply disabled.
	_, orphanPath := writeTestBlob(t, store, "ghost", bucket, raw)
	backdateFile(t, orphanPath, 48*time.Hour)

	if code := gcMark(ctx, db, store, 24*time.Hour); code != EX_OK {
		t.Fatalf("gcMark: exit %d", code)
	}
	makeEligible(t, ctx, db)
	if code := gcSweep(ctx, db, store, time.Hour); code != EX_OK {
		t.Fatalf("gcSweep: exit %d", code)
	}

	if !fileExists(t, movedPath) {
		t.Fatal("GC reaped a live mailbox's blobs while its rename was in flight")
	}
	if !fileExists(t, orphanPath) {
		t.Fatal("the unrelated orphan was reaped during maintenance; the pass must defer both, " +
			"since it cannot tell them apart")
	}

	// Once the rename completes and maintenance is released, the real orphan
	// is collected on the next pass.
	if _, err := db.Pool().Exec(ctx,
		`UPDATE mailboxes SET name = 'newname', maintenance_at = NULL WHERE id = $1`, mbID); err != nil {
		t.Fatalf("finish rename: %v", err)
	}
	if code := gcMark(ctx, db, store, 24*time.Hour); code != EX_OK {
		t.Fatalf("gcMark (post-rename): exit %d", code)
	}
	makeEligible(t, ctx, db)
	if code := gcSweep(ctx, db, store, time.Hour); code != EX_OK {
		t.Fatalf("gcSweep (post-rename): exit %d", code)
	}
	if fileExists(t, orphanPath) {
		t.Fatal("the deferred orphan was never reaped after maintenance ended")
	}
}

// TestGCSkipsAQuiescedMailboxsOwnTenant pins the simpler half: a mailbox that
// still resolves normally but is quiesced must not have its tree touched
// either, since its blobs may be mid-copy.
func TestGCSkipsAQuiescedMailboxsOwnTenant(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO mailboxes (name, password_hash, maintenance_at) VALUES ('busy', 'x', now())`,
	); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}

	raw := []byte(gcTestMessage)
	bucket := blob.BucketFromTime(time.Date(2003, 6, 15, 10, 0, 0, 0, time.UTC))
	_, path := writeTestBlob(t, store, "busy", bucket, raw)
	backdateFile(t, path, 48*time.Hour)

	if code := gcMark(ctx, db, store, 24*time.Hour); code != EX_OK {
		t.Fatalf("gcMark: exit %d", code)
	}
	if n := candidateCount(t, ctx, db); n != 0 {
		t.Fatalf("a quiesced mailbox's blob was marked: %d candidates, want 0", n)
	}
	if !fileExists(t, path) {
		t.Fatal("a quiesced mailbox's blob was removed")
	}
}
