package main

import (
	"context"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/ingest"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// TestIngestNULHeaderKeyRoundTrip is the R-053 end-to-end check: a message with
// a NUL in a header name parses, ingests, and the stored headers JSONB carries
// the sanitized key — where before the fix the INSERT would fail as EX_SOFTWARE
// on the JSONB NUL escape.
func TestIngestNULHeaderKeyRoundTrip(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}

	raw := []byte("From: a@x.invalid\r\nX-Bad\x00Key: v\r\nSubject: nul-key\r\n\r\nbody\r\n")
	msg, err := ingest.New(ingest.DefaultLimits()).Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	const tenant blob.Tenant = "nulkey"
	blobDate := time.Date(2003, 6, 15, 10, 0, 0, 0, time.UTC)
	bucket := blob.BucketFromTime(blobDate)
	sha, _ := writeTestBlob(t, store, tenant, bucket, raw)
	mboxID := gcMustMailbox(t, ctx, db, string(tenant))

	res, err := db.Ingest(ctx, storage.IngestParams{
		MailboxID:    mboxID,
		MailboxName:  string(tenant),
		FolderName:   "INBOX",
		EnvelopeTo:   "rcpt@nul.invalid",
		RawSHA256Hex: sha,
		RawSize:      int64(len(raw)),
		RawBlobDate:  blobDate,
		Message:      msg,
		InternalDate: &blobDate,
		EnsureBlobs:  ensureBlobsFunc(store, tenant, bucket, sha, raw, nil, nil),
	})
	if err != nil {
		t.Fatalf("Ingest of a NUL-header-key message must succeed, got: %v", err)
	}

	var hasKey bool
	if err := db.Pool().QueryRow(ctx,
		`SELECT jsonb_exists(headers, 'X-BadKey') FROM messages WHERE folder_id = $1 AND uid = $2`,
		res.FolderID, res.UID,
	).Scan(&hasKey); err != nil {
		t.Fatalf("query stored headers: %v", err)
	}
	if !hasKey {
		t.Error("stored headers JSONB is missing the sanitized key X-BadKey")
	}
}
