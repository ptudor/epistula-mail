package main

import (
	"context"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/ingest"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// Integration tests for the GC ↔ ingest race protocol (advisory locks,
// generation gate, EnsureBlobs rewrite). Skipped unless MAIL_DATABASE_TEST_PG
// is set; see pgtest.

func gcTestParser() *ingest.Parser {
	return ingest.New(ingest.Limits{
		MaxMessageBytes:       50 << 20,
		MaxMimeDepth:          10,
		MaxMimeParts:          200,
		MaxHeaderBytes:        16 << 10,
		MaxHeaderSectionBytes: 256 << 10,
		MaxTransferExpansion:  10,
	})
}

const gcTestMessage = "From: sender@gc-test.invalid\r\n" +
	"To: rcpt@gc-test.invalid\r\n" +
	"Subject: gc race fixture\r\n" +
	"Date: Mon, 15 Jun 2003 10:00:00 +0000\r\n" +
	"Message-ID: <gc-fixture@gc-test.invalid>\r\n" +
	"\r\n" +
	"Body bytes for the GC protocol tests.\r\n"

// writeTestBlob writes content into the store under (tenant, bucket) and
// returns the sha hex and final path.
func writeTestBlob(t *testing.T, store *blob.Store, tenant blob.Tenant, bucket blob.Bucket, content []byte) (string, string) {
	t.Helper()
	w, err := store.NewWriter(blob.KindRaw, tenant, bucket)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sha, _, _, err := w.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	path, err := store.PathFor(blob.KindRaw, tenant, bucket, sha)
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	return sha, path
}

func backdateFile(t *testing.T, path string, age time.Duration) {
	t.Helper()
	old := time.Now().Add(-age)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
}

func gcMustMailbox(t *testing.T, ctx context.Context, db *storage.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ($1, 'x') RETURNING id`,
		name,
	).Scan(&id); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}
	return id
}

func candidateCount(t *testing.T, ctx context.Context, db *storage.DB) int {
	t.Helper()
	var n int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM gc_candidates`).Scan(&n); err != nil {
		t.Fatalf("count candidates: %v", err)
	}
	return n
}

func fileExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatalf("stat %s: %v", path, err)
	return false
}

// makeEligible rewinds a candidate's first sighting so the grace cutoff and
// the generation gate (generation_marked > epoch(first_seen_at)) both pass,
// exactly the state a real candidate reaches after a later mark run
// re-confirms it.
func makeEligible(t *testing.T, ctx context.Context, db *storage.DB) {
	t.Helper()
	if _, err := db.Pool().Exec(ctx,
		`UPDATE gc_candidates SET first_seen_at = now() - interval '2 days'`,
	); err != nil {
		t.Fatalf("backdate candidates: %v", err)
	}
}

func TestGCMarkSkipsYoungBlobs(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}
	bucket := blob.Bucket("2003/06/15")
	// Orphan blob under a tenant with no mailbox row — exactly the
	// deleted-mailbox / stray-subtree case the mark pass treats as reapable.
	_, path := writeTestBlob(t, store, "gc-mark", bucket, []byte(gcTestMessage))

	// Freshly written file: a 24h grace must skip it even though it is
	// unreferenced — it may belong to an uncommitted delivery.
	if code := gcMark(ctx, db, store, 24*time.Hour); code != EX_OK {
		t.Fatalf("gcMark: exit %d", code)
	}
	if n := candidateCount(t, ctx, db); n != 0 {
		t.Fatalf("young blob was marked: %d candidates, want 0", n)
	}

	// Same blob, two days old: now it is a legitimate orphan.
	backdateFile(t, path, 48*time.Hour)
	if code := gcMark(ctx, db, store, 24*time.Hour); code != EX_OK {
		t.Fatalf("gcMark: exit %d", code)
	}
	if n := candidateCount(t, ctx, db); n != 1 {
		t.Fatalf("old orphan not marked: %d candidates, want 1", n)
	}
}

func TestGCSweepRequiresReconfirmation(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}
	bucket := blob.Bucket("2003/06/15")
	_, path := writeTestBlob(t, store, "gc-reconfirm", bucket, []byte(gcTestMessage))
	backdateFile(t, path, 48*time.Hour)

	if code := gcMark(ctx, db, store, 24*time.Hour); code != EX_OK {
		t.Fatalf("gcMark: exit %d", code)
	}

	// Candidate is past the grace cutoff but has only its first sighting:
	// generation_marked == the same mark run that inserted it. Sweep must
	// refuse — only a LATER mark run's re-confirmation makes it eligible.
	if _, err := db.Pool().Exec(ctx,
		`UPDATE gc_candidates
		    SET first_seen_at = now() - interval '2 days',
		        generation_marked = floor(extract(epoch FROM now() - interval '2 days'))::bigint`,
	); err != nil {
		t.Fatalf("simulate single old mark: %v", err)
	}
	if code := gcSweep(ctx, db, store, time.Hour); code != EX_OK {
		t.Fatalf("gcSweep: exit %d", code)
	}
	if !fileExists(t, path) {
		t.Fatal("sweep deleted a singly-marked candidate (generation gate broken)")
	}

	// A second mark run re-confirms (generation_marked advances past the
	// first sighting). Now the sweep may delete.
	if code := gcMark(ctx, db, store, 24*time.Hour); code != EX_OK {
		t.Fatalf("gcMark re-confirm: exit %d", code)
	}
	if code := gcSweep(ctx, db, store, time.Hour); code != EX_OK {
		t.Fatalf("gcSweep: exit %d", code)
	}
	if fileExists(t, path) {
		t.Fatal("sweep did not delete a re-confirmed eligible orphan")
	}
	if n := candidateCount(t, ctx, db); n != 0 {
		t.Fatalf("candidate row not cleared after sweep: %d", n)
	}
}

func TestIngestClearsGCCandidate(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}

	raw := []byte(gcTestMessage)
	msg, err := gcTestParser().Parse(raw)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	const tenant blob.Tenant = "gc-resurrect"
	blobDate := time.Date(2003, 6, 15, 10, 0, 0, 0, time.UTC)
	bucket := blob.BucketFromTime(blobDate)
	sha, path := writeTestBlob(t, store, tenant, bucket, raw)
	backdateFile(t, path, 48*time.Hour)

	// Simulate an eligible candidate for the blob (orphaned, re-confirmed).
	shaBytes, _ := hex.DecodeString(sha)
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO gc_candidates (tenant, sha256, kind, bucket, first_seen_at, generation_marked)
		 VALUES ($1, $2, 'raw', $3, now() - interval '2 days', $4)`,
		string(tenant), shaBytes, string(bucket), time.Now().Unix(),
	); err != nil {
		t.Fatalf("insert candidate: %v", err)
	}

	mboxID := gcMustMailbox(t, ctx, db, string(tenant))
	if _, err := db.Ingest(ctx, storage.IngestParams{
		MailboxID:    mboxID,
		MailboxName:  string(tenant),
		FolderName:   "INBOX",
		EnvelopeTo:   "rcpt@gc-test.invalid",
		RawSHA256Hex: sha,
		RawSize:      int64(len(raw)),
		RawBlobDate:  blobDate,
		Message:      msg,
		InternalDate: &blobDate,
		EnsureBlobs:  ensureBlobsFunc(store, tenant, bucket, sha, raw, nil, nil),
	}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	if n := candidateCount(t, ctx, db); n != 0 {
		t.Fatalf("ingest did not clear the gc candidate: %d rows", n)
	}
	if !fileExists(t, path) {
		t.Fatal("blob missing after ingest")
	}

	// A later sweep with a stale candidate must take the resurrected path:
	// keep the file, drop the row.
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO gc_candidates (tenant, sha256, kind, bucket, first_seen_at, generation_marked)
		 VALUES ($1, $2, 'raw', $3, now() - interval '2 days', $4)`,
		string(tenant), shaBytes, string(bucket), time.Now().Unix(),
	); err != nil {
		t.Fatalf("insert stale candidate: %v", err)
	}
	if code := gcSweep(ctx, db, store, time.Hour); code != EX_OK {
		t.Fatalf("gcSweep: exit %d", code)
	}
	if !fileExists(t, path) {
		t.Fatal("sweep deleted a referenced blob")
	}
	if n := candidateCount(t, ctx, db); n != 0 {
		t.Fatalf("sweep did not clear the resurrected candidate: %d rows", n)
	}
}

func TestEnsureBlobsRewritesAfterSweep(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}

	raw := []byte(gcTestMessage)
	msg, err := gcTestParser().Parse(raw)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	const tenant blob.Tenant = "gc-rewrite"
	blobDate := time.Date(2003, 6, 15, 10, 0, 0, 0, time.UTC)
	bucket := blob.BucketFromTime(blobDate)
	sha, path := writeTestBlob(t, store, tenant, bucket, raw)
	backdateFile(t, path, 48*time.Hour)

	shaBytes, _ := hex.DecodeString(sha)
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO gc_candidates (tenant, sha256, kind, bucket, first_seen_at, generation_marked)
		 VALUES ($1, $2, 'raw', $3, now() - interval '2 days', $4)`,
		string(tenant), shaBytes, string(bucket), time.Now().Unix(),
	); err != nil {
		t.Fatalf("insert candidate: %v", err)
	}

	// Sweep wins the race: the file is unlinked before ingest runs. The
	// tenant has no mailbox yet, so it is unreferenced and reapable.
	if code := gcSweep(ctx, db, store, time.Hour); code != EX_OK {
		t.Fatalf("gcSweep: exit %d", code)
	}
	if fileExists(t, path) {
		t.Fatal("sweep did not delete the orphan")
	}

	// Ingest of the same content must rewrite the blob via EnsureBlobs —
	// the committed row may never point at a missing file.
	mboxID := gcMustMailbox(t, ctx, db, string(tenant))
	if _, err := db.Ingest(ctx, storage.IngestParams{
		MailboxID:    mboxID,
		MailboxName:  string(tenant),
		FolderName:   "INBOX",
		EnvelopeTo:   "rcpt@gc-test.invalid",
		RawSHA256Hex: sha,
		RawSize:      int64(len(raw)),
		RawBlobDate:  blobDate,
		Message:      msg,
		InternalDate: &blobDate,
		EnsureBlobs:  ensureBlobsFunc(store, tenant, bucket, sha, raw, nil, nil),
	}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !fileExists(t, path) {
		t.Fatal("EnsureBlobs did not rewrite the swept blob")
	}
}

func TestReconcileQuotasWaitsForInFlightDelivery(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}

	mboxID := gcMustMailbox(t, ctx, db, "reconcile")
	var folderID int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
		 VALUES ($1, 'INBOX', 1, 10) RETURNING id`,
		mboxID,
	).Scan(&folderID); err != nil {
		t.Fatalf("insert folder: %v", err)
	}

	// Seed one committed message of 1000 bytes but a drifted used_bytes.
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO messages (folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date)
		 VALUES ($1, 1, decode('aa', 'hex'), CURRENT_DATE, 1000, now())`,
		folderID,
	); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`UPDATE mailboxes SET used_bytes = 999999 WHERE id = $1`, mboxID,
	); err != nil {
		t.Fatalf("drift used_bytes: %v", err)
	}

	// Simulate an in-flight delivery: messages row + used_bytes increment
	// written but not committed, holding the mailbox row lock.
	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after the commit below
	if _, err := tx.Exec(ctx,
		`INSERT INTO messages (folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date)
		 VALUES ($1, 2, decode('bb', 'hex'), CURRENT_DATE, 500, now())`,
		folderID,
	); err != nil {
		t.Fatalf("in-flight insert: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE mailboxes SET used_bytes = used_bytes + 500 WHERE id = $1`, mboxID,
	); err != nil {
		t.Fatalf("in-flight increment: %v", err)
	}

	recDone := make(chan int, 1)
	go func() {
		recDone <- gcReconcileQuotas(ctx, db)
	}()

	// The reconcile must block on the mailbox row lock while the delivery
	// is in flight — the old bulk UPDATE would have raced past it with a
	// stale SUM.
	select {
	case code := <-recDone:
		t.Fatalf("reconcile finished (exit %d) while a delivery held the row lock", code)
	case <-time.After(500 * time.Millisecond):
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit in-flight delivery: %v", err)
	}

	select {
	case code := <-recDone:
		if code != EX_OK {
			t.Fatalf("gcReconcileQuotas: exit %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reconcile did not finish after the delivery committed")
	}

	var used int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT used_bytes FROM mailboxes WHERE id = $1`, mboxID,
	).Scan(&used); err != nil {
		t.Fatalf("read used_bytes: %v", err)
	}
	if used != 1500 {
		t.Fatalf("used_bytes = %d, want 1500 (1000 committed + 500 from the in-flight delivery)", used)
	}
}

func TestSweepBlocksOnConcurrentIngestLock(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}

	const tenant blob.Tenant = "gc-inflight"
	raw := []byte(gcTestMessage)
	blobDate := time.Date(2003, 6, 15, 10, 0, 0, 0, time.UTC)
	bucket := blob.BucketFromTime(blobDate)
	sha, path := writeTestBlob(t, store, tenant, bucket, raw)
	backdateFile(t, path, 48*time.Hour)

	shaBytes, _ := hex.DecodeString(sha)
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO gc_candidates (tenant, sha256, kind, bucket, first_seen_at, generation_marked)
		 VALUES ($1, $2, 'raw', $3, now() - interval '2 days', $4)`,
		string(tenant), shaBytes, string(bucket), time.Now().Unix(),
	); err != nil {
		t.Fatalf("insert candidate: %v", err)
	}

	mboxID := gcMustMailbox(t, ctx, db, string(tenant))
	var folderID int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
		 VALUES ($1, 'INBOX', 1, 2) RETURNING id`,
		mboxID,
	).Scan(&folderID); err != nil {
		t.Fatalf("insert folder: %v", err)
	}

	// Simulate an in-flight ingest: a transaction holding the blob's
	// advisory lock with the messages row written but not yet committed.
	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after the commit below
	key := storage.BlobAdvisoryLockKey(string(tenant), "raw", string(bucket), sha)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
		t.Fatalf("advisory lock: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO messages (folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date)
		 VALUES ($1, 1, $2, $3, $4, $5)`,
		folderID, shaBytes, blobDate, int64(len(raw)), blobDate,
	); err != nil {
		t.Fatalf("insert message: %v", err)
	}

	sweepDone := make(chan int, 1)
	go func() {
		sweepDone <- gcSweep(ctx, db, store, time.Hour)
	}()

	// While the ingest transaction holds the lock the sweep must not have
	// unlinked the blob.
	select {
	case code := <-sweepDone:
		t.Fatalf("sweep finished (exit %d) while ingest held the advisory lock", code)
	case <-time.After(500 * time.Millisecond):
	}
	if !fileExists(t, path) {
		t.Fatal("sweep unlinked the blob while the ingest lock was held")
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	select {
	case code := <-sweepDone:
		if code != EX_OK {
			t.Fatalf("gcSweep: exit %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sweep did not finish after the ingest lock was released")
	}

	// The sweep saw the committed row: blob kept, candidate resurrected away.
	if !fileExists(t, path) {
		t.Fatal("sweep deleted a blob referenced by the committed ingest")
	}
	if n := candidateCount(t, ctx, db); n != 0 {
		t.Fatalf("candidate not cleared after resurrection: %d rows", n)
	}
}

// gcMustFolder creates a folder in a mailbox and returns its id.
func gcMustFolder(t *testing.T, ctx context.Context, db *storage.DB, mailboxID int64) int64 {
	t.Helper()
	var folderID int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
		 VALUES ($1, 'INBOX', 1, 2) RETURNING id`, mailboxID,
	).Scan(&folderID); err != nil {
		t.Fatalf("insert folder: %v", err)
	}
	return folderID
}

// TestGCCrossTenantIsolation is the core isolation invariant: identical content
// delivered to two mailboxes is two physical files, and the GC reference check
// is scoped per mailbox — so when one mailbox drops its reference, ONLY that
// mailbox's file is reaped, even though the other mailbox still holds an
// identical-content+date blob. A non-scoped check would falsely keep the orphan
// (because the other mailbox "references" the same sha+date).
func TestGCCrossTenantIsolation(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}

	raw := []byte(gcTestMessage)
	blobDate := time.Date(2003, 6, 15, 10, 0, 0, 0, time.UTC)
	bucket := blob.BucketFromTime(blobDate)

	shaA, pathA := writeTestBlob(t, store, "alice", bucket, raw)
	shaB, pathB := writeTestBlob(t, store, "bob", bucket, raw)
	if shaA != shaB {
		t.Fatalf("content hash differs across tenants: %s vs %s", shaA, shaB)
	}
	if pathA == pathB {
		t.Fatalf("tenant paths collided: %s", pathA)
	}
	backdateFile(t, pathA, 48*time.Hour)
	backdateFile(t, pathB, 48*time.Hour)
	shaBytes, _ := hex.DecodeString(shaA)

	// Both mailboxes exist with a folder; only BOB keeps a referencing
	// message. ALICE expunged her copy, so her on-disk file is an orphan.
	aliceID := gcMustMailbox(t, ctx, db, "alice")
	gcMustFolder(t, ctx, db, aliceID)
	bobID := gcMustMailbox(t, ctx, db, "bob")
	bobFolder := gcMustFolder(t, ctx, db, bobID)
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO messages (folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date)
		 VALUES ($1, 1, $2, $3, $4, $5)`,
		bobFolder, shaBytes, blobDate, int64(len(raw)), blobDate,
	); err != nil {
		t.Fatalf("insert bob message: %v", err)
	}

	if code := gcMark(ctx, db, store, 24*time.Hour); code != EX_OK {
		t.Fatalf("gcMark: exit %d", code)
	}
	// Only alice's orphan is a candidate; bob's file is referenced in bob's
	// own mailbox and must not be marked.
	if n := candidateCount(t, ctx, db); n != 1 {
		t.Fatalf("after mark: %d candidates, want 1 (alice's orphan only)", n)
	}
	makeEligible(t, ctx, db)
	if code := gcSweep(ctx, db, store, time.Hour); code != EX_OK {
		t.Fatalf("gcSweep: exit %d", code)
	}

	if fileExists(t, pathA) {
		t.Error("alice's orphan blob was NOT reaped")
	}
	if !fileExists(t, pathB) {
		t.Error("bob's referenced blob was wrongly reaped — cross-tenant scoping is broken")
	}
}

// TestGCReapsDeletedMailboxSubtree covers the unresolvable-tenant path: after a
// mailbox is deleted (its rows cascade away) its on-disk blobs remain, their
// tenant name no longer resolves to a mailbox, and GC must treat them as
// unreferenced and reap them.
func TestGCReapsDeletedMailboxSubtree(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}

	raw := []byte(gcTestMessage)
	blobDate := time.Date(2003, 6, 15, 10, 0, 0, 0, time.UTC)
	bucket := blob.BucketFromTime(blobDate)
	_, path := writeTestBlob(t, store, "ghost", bucket, raw)
	backdateFile(t, path, 48*time.Hour)

	// "ghost" is never given a mailbox row (modeling a mailbox deleted after
	// its blobs were written). Mark must flag the orphan, sweep must reap it.
	if code := gcMark(ctx, db, store, 24*time.Hour); code != EX_OK {
		t.Fatalf("gcMark: exit %d", code)
	}
	if n := candidateCount(t, ctx, db); n != 1 {
		t.Fatalf("deleted-mailbox orphan not marked: %d candidates, want 1", n)
	}
	makeEligible(t, ctx, db)
	if code := gcSweep(ctx, db, store, time.Hour); code != EX_OK {
		t.Fatalf("gcSweep: exit %d", code)
	}
	if fileExists(t, path) {
		t.Fatal("deleted-mailbox orphan was not reaped")
	}
	if n := candidateCount(t, ctx, db); n != 0 {
		t.Fatalf("candidate not cleared: %d rows", n)
	}
}
