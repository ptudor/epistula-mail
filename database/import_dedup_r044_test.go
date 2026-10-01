package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// TestImportDedupConcurrent is the R-044 regression: two goroutines importing
// the same (folder, raw_sha256) into one folder must not both insert. The
// in-transaction dedup recheck (DedupOnRawSHA), gated by the per-blob advisory
// lock, serializes the two ingests so the loser sees the winner's committed row
// and returns ErrDuplicate. The folder ends with exactly one message row.
func TestImportDedupConcurrent(t *testing.T) {
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
	const tenant blob.Tenant = "dedup-race"
	blobDate := time.Date(2003, 6, 15, 10, 0, 0, 0, time.UTC)
	bucket := blob.BucketFromTime(blobDate)
	sha, _ := writeTestBlob(t, store, tenant, bucket, raw)
	mboxID := gcMustMailbox(t, ctx, db, string(tenant))

	mkParams := func() storage.IngestParams {
		return storage.IngestParams{
			MailboxID:     mboxID,
			MailboxName:   string(tenant),
			FolderName:    "INBOX",
			EnvelopeTo:    "rcpt@dedup.invalid",
			RawSHA256Hex:  sha,
			RawSize:       int64(len(raw)),
			RawBlobDate:   blobDate,
			Message:       msg,
			InternalDate:  &blobDate,
			DedupOnRawSHA: true,
			EnsureBlobs:   ensureBlobsFunc(store, tenant, bucket, sha, raw, nil, nil),
		}
	}

	const workers = 2
	var wg sync.WaitGroup
	errs := make([]error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = db.Ingest(ctx, mkParams())
		}(i)
	}
	close(start)
	wg.Wait()

	oks, dups := 0, 0
	for _, e := range errs {
		switch {
		case e == nil:
			oks++
		case errors.Is(e, storage.ErrDuplicate):
			dups++
		default:
			t.Fatalf("unexpected ingest error: %v", e)
		}
	}
	if oks != 1 || dups != 1 {
		t.Fatalf("want 1 imported + 1 duplicate, got %d imported, %d duplicate", oks, dups)
	}

	var n int
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM messages m
		   JOIN folders f ON f.id = m.folder_id
		  WHERE f.mailbox_id = $1 AND m.raw_sha256 = decode($2, 'hex')`,
		mboxID, sha,
	).Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if n != 1 {
		t.Fatalf("folder holds %d rows for the shared sha, want exactly 1 (concurrent import duplicated)", n)
	}
}
