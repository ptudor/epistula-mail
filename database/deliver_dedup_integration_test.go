package main

import (
	"context"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// TestDeliveryIsDuplicate locks down the Postfix-retry idempotency guard:
// a message already committed to (mailbox, folder) is reported as a
// duplicate; a different message or an absent folder is not.
func TestDeliveryIsDuplicate(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var mailboxID int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('dedup', 'x') RETURNING id`,
	).Scan(&mailboxID); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}

	raw := []byte("From: a@dedup.invalid\r\nSubject: once\r\n\r\nbody\r\n")
	msg, err := gcTestParser().Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}
	blobDate := time.Now().UTC()
	w, err := store.NewWriter(blob.KindRaw, "dedup", blob.BucketFromTime(blobDate))
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

	// Before any delivery: the folder doesn't even exist yet.
	dup, _, err := deliveryIsDuplicate(ctx, db, mailboxID, "INBOX", sha)
	if err != nil {
		t.Fatalf("deliveryIsDuplicate (no folder): %v", err)
	}
	if dup {
		t.Error("no folder yet — must not report duplicate")
	}

	if _, err := db.Ingest(ctx, storage.IngestParams{
		MailboxID:    mailboxID,
		MailboxName:  "dedup",
		FolderName:   "INBOX",
		EnvelopeFrom: "a@dedup.invalid",
		EnvelopeTo:   "dedup@dedup.invalid",
		RawSHA256Hex: sha,
		RawSize:      int64(len(raw)),
		RawBlobDate:  blobDate,
		Message:      msg,
	}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// Redelivery of the same bytes to the same folder is the duplicate.
	dup, _, err = deliveryIsDuplicate(ctx, db, mailboxID, "INBOX", sha)
	if err != nil {
		t.Fatalf("deliveryIsDuplicate (committed): %v", err)
	}
	if !dup {
		t.Error("committed message must be reported as duplicate on redelivery")
	}

	// Same bytes, different folder: not a duplicate.
	dup, _, err = deliveryIsDuplicate(ctx, db, mailboxID, "Archive", sha)
	if err != nil {
		t.Fatalf("deliveryIsDuplicate (other folder): %v", err)
	}
	if dup {
		t.Error("different folder must not be reported as duplicate")
	}
}
