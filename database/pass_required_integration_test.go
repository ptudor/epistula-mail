package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// TestIngestMarksAnnotationPassRequired covers the annotation pipeline's work
// queue from the writer's side (migration 022): a stored message is marked in
// the transaction that stores it, a duplicate that stores nothing adds no
// marker, a delivery rolled back for quota leaves none behind, and deleting the
// message removes its marker.
func TestIngestMarksAnnotationPassRequired(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var mailboxID int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash, quota_bytes) VALUES ('queuebox', 'x', 80) RETURNING id`,
	).Scan(&mailboxID); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}
	markers := func() int64 {
		t.Helper()
		var n int64
		if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM annotation_pass_required`).Scan(&n); err != nil {
			t.Fatalf("count markers: %v", err)
		}
		return n
	}
	ingest := func(body string) (storage.IngestResult, error) {
		t.Helper()
		raw := []byte("From: a@q.invalid\r\nSubject: " + body + "\r\n\r\n" + body + "\r\n")
		msg, err := gcTestParser().Parse(raw)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		return db.Ingest(ctx, storage.IngestParams{
			MailboxID:     mailboxID,
			MailboxName:   "queuebox",
			FolderName:    "INBOX",
			EnvelopeFrom:  "a@q.invalid",
			EnvelopeTo:    "queuebox@q.invalid",
			RawSHA256Hex:  msg.SHA256Hex,
			RawSize:       int64(len(raw)),
			RawBlobDate:   time.Now().UTC(),
			Message:       msg,
			DedupOnRawSHA: true,
		})
	}

	res, err := ingest("aa")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	var marked int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT message_id FROM annotation_pass_required`).Scan(&marked); err != nil || marked != res.MessageID {
		t.Fatalf("marker = %d (%v); want the stored message %d", marked, err, res.MessageID)
	}

	if _, err := ingest("aa"); !errors.Is(err, storage.ErrDuplicate) {
		t.Fatalf("second ingest of the same content = %v; want ErrDuplicate", err)
	}
	if n := markers(); n != 1 {
		t.Fatalf("%d markers after a duplicate; want 1", n)
	}

	if _, err := ingest("this body is deliberately long enough to exceed the tiny quota"); !errors.Is(err, storage.ErrOverQuota) {
		t.Fatalf("over-quota ingest = %v; want ErrOverQuota", err)
	}
	if n := markers(); n != 1 {
		t.Fatalf("%d markers after a rolled-back delivery; want 1", n)
	}

	if _, err := db.Pool().Exec(ctx, `DELETE FROM messages WHERE id = $1`, res.MessageID); err != nil {
		t.Fatalf("delete message: %v", err)
	}
	if n := markers(); n != 0 {
		t.Fatalf("%d markers after the message was deleted; want 0", n)
	}
}
