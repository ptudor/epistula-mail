package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// TestUIDValiditySequenceMonotonic is the R-062 verification: a folder deleted
// and recreated within the same wall-clock second must NOT reuse its
// uidvalidity. Before the fix both writers seeded from time.Now().Unix(), so a
// same-second recreate handed a client the identical (UIDVALIDITY, uidnext=1)
// pair — silently rebinding cached UIDs to new messages. The shared
// folder_uidvalidity_seq (migration 009) makes every mint strictly increasing.
func TestUIDValiditySequenceMonotonic(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mboxID := gcMustMailbox(t, ctx, db, "uidvalidity-box")

	readUV := func(folderID int64) int64 {
		var uv int64
		if err := db.Pool().QueryRow(ctx,
			`SELECT uidvalidity FROM folders WHERE id = $1`, folderID,
		).Scan(&uv); err != nil {
			t.Fatalf("read uidvalidity: %v", err)
		}
		return uv
	}

	id1, err := db.LookupOrCreateFolder(ctx, mboxID, "Recreated")
	if err != nil {
		t.Fatalf("create folder (1st): %v", err)
	}
	uv1 := readUV(id1)

	// Delete + recreate happen microseconds apart — well within one second, the
	// exact window the old time.Now().Unix() seed collided in.
	if _, err := db.Pool().Exec(ctx, `DELETE FROM folders WHERE id = $1`, id1); err != nil {
		t.Fatalf("delete folder: %v", err)
	}
	id2, err := db.LookupOrCreateFolder(ctx, mboxID, "Recreated")
	if err != nil {
		t.Fatalf("create folder (2nd): %v", err)
	}
	uv2 := readUV(id2)

	if uv2 <= uv1 {
		t.Fatalf("recreated folder uidvalidity did not advance: old=%d new=%d "+
			"(same-second delete/recreate must mint a strictly greater value)", uv1, uv2)
	}
}

// TestIngestNormalizesBlobDateToUTC is the R-064 verification: Ingest must
// store raw_blob_date as the UTC calendar date the blob physically lives under,
// not the wall-clock date of the caller's zoned time.Time. A +09:00 time at
// 08:00 local is 23:00 the PREVIOUS day in UTC, so the on-disk bucket and the
// stored DATE would diverge without the normalization — readers would 404 the
// blob and gc mark would flag a referenced blob as garbage.
func TestIngestNormalizesBlobDateToUTC(t *testing.T) {
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

	const tenant blob.Tenant = "utc-normalize"
	// 2003-06-15 08:00 +09:00 == 2003-06-14 23:00 UTC. Local date is the 15th,
	// UTC date (the on-disk bucket) is the 14th.
	jst := time.FixedZone("JST", 9*3600)
	zoned := time.Date(2003, 6, 15, 8, 0, 0, 0, jst)
	bucket := blob.BucketFromTime(zoned) // UTC → "2003/06/14"
	if got := string(bucket); got != "2003/06/14" {
		t.Fatalf("bucket precondition wrong: got %q, want 2003/06/14", got)
	}
	sha, path := writeTestBlob(t, store, tenant, bucket, raw)
	backdateFile(t, path, 48*time.Hour)

	mboxID := gcMustMailbox(t, ctx, db, string(tenant))
	if _, err := db.Ingest(ctx, storage.IngestParams{
		MailboxID:    mboxID,
		MailboxName:  string(tenant),
		FolderName:   "INBOX",
		EnvelopeTo:   "rcpt@gc-test.invalid",
		RawSHA256Hex: sha,
		RawSize:      int64(len(raw)),
		RawBlobDate:  zoned, // deliberately zoned, NOT pre-normalized
		Message:      msg,
		InternalDate: &zoned,
		EnsureBlobs:  ensureBlobsFunc(store, tenant, bucket, sha, raw, nil, nil),
	}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// The stored DATE must equal the UTC bucket the blob is filed under.
	var stored time.Time
	if err := db.Pool().QueryRow(ctx,
		`SELECT raw_blob_date FROM messages WHERE raw_sha256 = decode($1, 'hex')`, sha,
	).Scan(&stored); err != nil {
		t.Fatalf("read raw_blob_date: %v", err)
	}
	if got := stored.Format("2006/01/02"); got != string(bucket) {
		t.Fatalf("raw_blob_date = %s, want %s (must match the UTC on-disk bucket, not the zoned local date)", got, bucket)
	}

	// gc mark must therefore see the blob as REFERENCED: bucket == column, so
	// no gc_candidates row is created for it.
	if code := gcMark(ctx, db, store, time.Hour); code != EX_OK {
		t.Fatalf("gcMark: exit %d", code)
	}
	if n := candidateCount(t, ctx, db); n != 0 {
		t.Fatalf("gc mark flagged a referenced blob (%d candidates) — bucket/column diverged", n)
	}
	if !fileExists(t, path) {
		t.Fatal("blob missing after ingest+mark")
	}
}

// TestGlobalDateIndexPresent is the R-018 verification: migration 009 adds the
// index backing the global (internal_date DESC, id DESC) sort used by
// /v1/search and /v1/export. The full EXPLAIN-on-100k-rows plan check is an
// operational benchmark, not a unit test; here we assert the index exists with
// the exact column order the readers ORDER BY.
func TestGlobalDateIndexPresent(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var def string
	err := db.Pool().QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes
		  WHERE schemaname = current_schema() AND indexname = 'idx_messages_date_id'`,
	).Scan(&def)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("idx_messages_date_id missing after migrations (R-018)")
	}
	if err != nil {
		t.Fatalf("query index: %v", err)
	}
	// pg_get_indexdef normalizes the column list to a canonical form; assert
	// the exact leading order the readers ORDER BY (internal_date first).
	norm := strings.ToLower(def)
	if !strings.Contains(norm, "(internal_date desc, id desc)") {
		t.Fatalf("idx_messages_date_id must be (internal_date DESC, id DESC): %s", def)
	}
}
