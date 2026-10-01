package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/storage"
)

const duplicateAttachmentMessage = "From: sender@ra6x008.invalid\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nbody\r\n--b\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=report.bin\r\n\r\nattachment bytes\r\n--b--\r\n"

func TestRemainingDuplicateRepairsAllReferences(t *testing.T) {
	cfg, _, teardown := deliverFixture(t)
	defer teardown()
	ctx := context.Background()
	db, err := storage.Open(ctx, storage.Config{DSN: cfg.Postgres.DSN})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	deliverAgain := func(want int) {
		t.Helper()
		if got := deliverBytes(t, cfg, "ra6x008@ra6x008.invalid", "sender@ra6x008.invalid", duplicateAttachmentMessage); got != want {
			t.Fatalf("exit=%d want=%d", got, want)
		}
	}
	deliverAgain(EX_OK)
	var messageID, uid, size, used int64
	var tenant, sha string
	var date time.Time
	if err := db.Pool().QueryRow(ctx, `SELECT m.id,m.uid,b.name,a.size_bytes,encode(a.sha256,'hex'),a.blob_date,b.used_bytes FROM attachments a JOIN messages m ON m.id=a.message_id JOIN folders f ON f.id=m.folder_id JOIN mailboxes b ON b.id=f.mailbox_id`).Scan(&messageID, &uid, &tenant, &size, &sha, &date, &used); err != nil {
		t.Fatal(err)
	}
	store := blob.NewStore(cfg.Storage.Root)
	path, err := store.PathFor(blob.KindAttachment, blob.Tenant(tenant), blob.BucketFromTime(date), sha)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, damage := range []string{"missing", "truncated", "same-size", "healthy"} {
		t.Run(damage, func(t *testing.T) {
			switch damage {
			case "missing":
				err = os.Remove(path)
			case "truncated":
				err = os.Truncate(path, 1)
			case "same-size":
				err = os.WriteFile(path, bytes.Repeat([]byte("x"), int(size)), 0640)
			}
			if err != nil {
				t.Fatal(err)
			}
			deliverAgain(EX_OK)
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatal("attachment was not restored")
			}
		})
	}
	// A historical attachment that cannot be reconstructed must defer, not
	// acknowledge only the raw blob. No message or quota changes are allowed.
	if _, err := db.Pool().Exec(ctx, `UPDATE attachments SET sha256=decode($1,'hex') WHERE message_id=$2`, strings.Repeat("ab", 32), messageID); err != nil {
		t.Fatal(err)
	}
	deliverAgain(EX_TEMPFAIL)
	var count, afterUID, afterUsed int64
	if err := db.Pool().QueryRow(ctx, `SELECT count(m.id),min(m.uid),min(b.used_bytes) FROM messages m JOIN folders f ON f.id=m.folder_id JOIN mailboxes b ON b.id=f.mailbox_id WHERE m.id=$1`, messageID).Scan(&count, &afterUID, &afterUsed); err != nil {
		t.Fatal(err)
	}
	if count != 1 || uid != afterUID || used != afterUsed {
		t.Fatalf("duplicate changed rows/identity/quota: %d/%d/%d", count, afterUID, afterUsed)
	}
	if n := countMessageRows(t, ctx, cfg.Postgres.DSN); n != 1 {
		t.Fatalf("duplicate inserted rows: %d", n)
	}
}

func TestRemainingDuplicateRepairSerializesWithGC(t *testing.T) {
	cfg, _, teardown := deliverFixture(t)
	defer teardown()
	if code := deliverBytes(t, cfg, "ra6x008@ra6x008.invalid", "sender@ra6x008.invalid", duplicateAttachmentMessage); code != EX_OK {
		t.Fatal(code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := storage.Open(ctx, storage.Config{DSN: cfg.Postgres.DSN})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var mailboxID int64
	var tenant, sha string
	var date time.Time
	if err := db.Pool().QueryRow(ctx, `SELECT b.id,b.name,encode(a.sha256,'hex'),a.blob_date FROM attachments a JOIN messages m ON m.id=a.message_id JOIN folders f ON f.id=m.folder_id JOIN mailboxes b ON b.id=f.mailbox_id`).Scan(&mailboxID, &tenant, &sha, &date); err != nil {
		t.Fatal(err)
	}
	bucket := blob.BucketFromTime(date)
	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, storage.BlobAdvisoryLockKey(tenant, string(blob.KindAttachment), string(bucket), sha)); err != nil {
		t.Fatal(err)
	}
	msg, err := gcTestParser().Parse([]byte(duplicateAttachmentMessage))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	a := &deliveryAcceptance{}
	go func() {
		ok, err := repairDuplicateDelivery(ctx, db, cfg, mailboxID, tenant, msg, []byte(duplicateAttachmentMessage), a)
		if err == nil && !ok {
			err = context.Canceled
		}
		done <- err
	}()
	for {
		var waiting bool
		if err := db.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database()))`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("repair bypassed GC lock: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond * 5):
		}
	}
	if a.isCommitted() {
		t.Fatal("accepted before GC exclusion")
	}
	store := blob.NewStore(cfg.Storage.Root)
	path, err := store.PathFor(blob.KindAttachment, blob.Tenant(tenant), bucket, sha)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !a.isCommitted() {
		t.Fatal("repair did not record acceptance")
	}
	if ok, err := store.ContentMatches(blob.KindAttachment, blob.Tenant(tenant), bucket, sha, -1); err != nil || !ok {
		t.Fatalf("repair after GC=%v/%v", ok, err)
	}
}
