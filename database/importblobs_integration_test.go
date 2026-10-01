package main

import (
	"context"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestImportBlobsRecoversMessages is the disaster-recovery scenario: a blob
// tree survives, Postgres is empty (fresh schema). import-blobs must rebuild
// the rows with the bucket-derived dates, attachments included, idempotently.
func TestImportBlobsRecoversMessages(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}

	// Two messages in different historical buckets, one with an attachment.
	msg1999 := "From: old@recovery.invalid\r\n" +
		"Subject: from the nineties\r\n" +
		"Date: Fri, 31 Dec 1999 23:59:00 +0000\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"party like it is\r\n"
	msg2022 := "From: newer@recovery.invalid\r\n" +
		"Subject: with attachment\r\n" +
		"Date: Mon, 31 Oct 2022 12:00:00 +0000\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n" +
		"\r\n" +
		"--B\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"see attachment\r\n" +
		"--B\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=data.bin\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"aGVsbG8gd29ybGQ=\r\n" +
		"--B--\r\n"

	const tenant blob.Tenant = "recovered"
	type seeded struct {
		bucket blob.Bucket
		sha    string
		path   string
	}
	var blobs []seeded
	for _, m := range []struct {
		bucket blob.Bucket
		raw    string
	}{
		{"1999/12/31", msg1999},
		{"2022/10/31", msg2022},
	} {
		sha, path := writeTestBlob(t, store, tenant, m.bucket, []byte(m.raw))
		blobs = append(blobs, seeded{m.bucket, sha, path})
	}

	mboxID := gcMustMailbox(t, ctx, db, string(tenant))
	folderID, err := db.LookupOrCreateFolder(ctx, mboxID, "INBOX")
	if err != nil {
		t.Fatalf("folder: %v", err)
	}

	parser := gcTestParser()
	runAll := func() (ok, dup, fail int) {
		err := store.Walk(blob.KindRaw, func(kind blob.Kind, walkTenant blob.Tenant, bucket blob.Bucket, shaHex, path string, info os.FileInfo) error {
			rc, _, err := importBlobOne(ctx, db, store, parser, importBlobParams{
				Tenant:     walkTenant,
				Bucket:     bucket,
				SHAHex:     shaHex,
				Path:       path,
				MailboxID:  mboxID,
				FolderID:   folderID,
				FolderName: "INBOX",
			})
			switch rc {
			case importOK:
				ok++
			case importDuplicate:
				dup++
			case importParseFailed:
				fail++
				t.Logf("parse failed: %v", err)
			default:
				t.Fatalf("importBlobOne(%s): %v", path, err)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk: %v", err)
		}
		return
	}

	ok, dup, fail := runAll()
	if ok != 2 || dup != 0 || fail != 0 {
		t.Fatalf("first run: ok=%d dup=%d fail=%d, want 2/0/0", ok, dup, fail)
	}

	// Idempotency: a second pass dedups everything.
	ok, dup, fail = runAll()
	if ok != 0 || dup != 2 || fail != 0 {
		t.Fatalf("second run: ok=%d dup=%d fail=%d, want 0/2/0", ok, dup, fail)
	}

	// The 1999 message keeps its Date as INTERNALDATE and its bucket as
	// raw_blob_date; the attachment row from the 2022 message exists with
	// its blob co-located in the 2022 bucket.
	var internal, blobDate time.Time
	if err := db.Pool().QueryRow(ctx,
		`SELECT internal_date, raw_blob_date FROM messages WHERE subject = 'from the nineties'`,
	).Scan(&internal, &blobDate); err != nil {
		t.Fatalf("select 1999 message: %v", err)
	}
	if internal.UTC().Year() != 1999 {
		t.Errorf("1999 message INTERNALDATE = %v, want year 1999", internal)
	}
	if got := blobDate.Format("2006/01/02"); got != "1999/12/31" {
		t.Errorf("raw_blob_date = %s, want 1999/12/31", got)
	}

	var attSHA []byte
	var attName string
	if err := db.Pool().QueryRow(ctx,
		`SELECT a.sha256, a.filename
		   FROM attachments a
		   JOIN messages m ON m.id = a.message_id
		  WHERE m.subject = 'with attachment'`,
	).Scan(&attSHA, &attName); err != nil {
		t.Fatalf("select attachment: %v", err)
	}
	if attName != "data.bin" {
		t.Errorf("attachment filename = %q, want data.bin", attName)
	}
	if exists, err := store.Exists(blob.KindAttachment, tenant, "2022/10/31", hex.EncodeToString(attSHA)); err != nil || !exists {
		t.Errorf("attachment blob missing from 2022/10/31 bucket (exists=%v err=%v)", exists, err)
	}

	// Corruption guard: a blob whose bytes don't match its filename hash
	// must be refused, not ingested under the wrong identity.
	if err := os.WriteFile(blobs[0].path, []byte(strings.Replace(msg1999, "party", "PARTY", 1)), 0o640); err != nil {
		t.Fatalf("corrupt blob: %v", err)
	}
	rc, _, err := importBlobOne(ctx, db, store, parser, importBlobParams{
		Tenant: tenant, Bucket: blobs[0].bucket, SHAHex: blobs[0].sha, Path: blobs[0].path,
		MailboxID: mboxID, FolderID: folderID, FolderName: "INBOX",
	})
	if rc != importParseFailed || err == nil || !strings.Contains(err.Error(), "sha mismatch") {
		t.Fatalf("corrupted blob: rc=%d err=%v, want importParseFailed with sha mismatch", rc, err)
	}
}
