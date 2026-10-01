package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/maildir"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

func TestDuplicateImportsRepairPersistedReferences(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := context.Background()
	const tenant blob.Tenant = "duplicate-import"
	mailboxID := gcMustMailbox(t, ctx, db, string(tenant))
	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	raw := []byte(duplicateAttachmentMessage)
	source := filepath.Join(t.TempDir(), "message")
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	parser := gcTestParser()
	p := importOneParams{Entry: maildir.Entry{Path: source, BaseName: "message", ModTime: time.Date(2001, 2, 3, 0, 0, 0, 0, time.UTC)}, MailboxID: mailboxID, Tenant: tenant, FolderName: "INBOX"}
	rc, folderID, err := importOne(ctx, db, store, parser, p)
	if rc != importOK || err != nil {
		t.Fatalf("seed: %v %v", rc, err)
	}
	p.FolderID = folderID
	var rawSHA, attSHA, bucket string
	var attSize int64
	if err := db.Pool().QueryRow(ctx, `SELECT encode(m.raw_sha256,'hex'), encode(a.sha256,'hex'), to_char(m.raw_blob_date,'YYYY/MM/DD'),a.size_bytes FROM messages m JOIN attachments a ON a.message_id=m.id`).Scan(&rawSHA, &attSHA, &bucket, &attSize); err != nil {
		t.Fatal(err)
	}
	// Historical numbering must never be rewritten just to reconstruct content.
	if _, err := db.Pool().Exec(ctx, `UPDATE attachments SET part_number='99'`); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		t.Helper()
		var value string
		err := db.Pool().QueryRow(ctx, `SELECT jsonb_build_array((SELECT jsonb_agg(m) FROM messages m),(SELECT jsonb_agg(a) FROM attachments a),(SELECT jsonb_agg(f) FROM folders f),(SELECT jsonb_agg(b) FROM mailboxes b))::text`).Scan(&value)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := snapshot()
	rawPath, err := store.PathFor(blob.KindRaw, tenant, blob.Bucket(bucket), rawSHA)
	if err != nil {
		t.Fatal(err)
	}
	attPath, err := store.PathFor(blob.KindAttachment, tenant, blob.Bucket(bucket), attSHA)
	if err != nil {
		t.Fatal(err)
	}
	bp := importBlobParams{Tenant: tenant, Bucket: blob.Bucket(bucket), SHAHex: rawSHA, Path: rawPath, MailboxID: mailboxID, FolderID: folderID, FolderName: "INBOX"}
	for _, method := range []string{"maildir", "raw-tree", "concurrent-first-ingest"} {
		for _, damage := range []string{"missing", "truncated", "same-size"} {
			t.Run(method+"/"+damage, func(t *testing.T) {
				switch damage {
				case "missing":
					err = os.Remove(attPath)
				case "truncated":
					err = os.Truncate(attPath, 1)
				case "same-size":
					err = os.WriteFile(attPath, bytes.Repeat([]byte("x"), int(attSize)), 0600)
				}
				if err != nil {
					t.Fatal(err)
				}
				// Dry run must not repair a missing/corrupt stored attachment.
				dry := p
				dry.DryRun = true
				if rc, _, err := importOne(ctx, db, store, parser, dry); rc != importDryRunDuplicate || err != nil {
					t.Fatalf("dry: %v %v", rc, err)
				}
				if ok, err := store.ContentMatches(blob.KindAttachment, tenant, blob.Bucket(bucket), attSHA, attSize); ok || err != nil {
					t.Fatalf("dry repaired: %v %v", ok, err)
				}
				var rc importResult
				var id int64
				var err error
				switch method {
				case "maildir":
					rc, id, err = importOne(ctx, db, store, parser, p)
				case "raw-tree":
					rc, id, err = importBlobOne(ctx, db, store, parser, bp)
				default:
					lazy := p
					lazy.FolderID = 0
					rc, id, err = importOne(ctx, db, store, parser, lazy)
				}
				if rc != importDuplicate || id != folderID || err != nil {
					t.Fatalf("duplicate: %v %d %v", rc, id, err)
				}
				if ok, err := store.ContentMatches(blob.KindAttachment, tenant, blob.Bucket(bucket), attSHA, attSize); !ok || err != nil {
					t.Fatalf("not repaired: %v %v", ok, err)
				}
				if after := snapshot(); after != before {
					t.Fatal("duplicate changed message, attachment, folder or quota metadata")
				}
			})
		}
	}
	for _, bad := range [][]byte{nil, []byte("short"), bytes.Repeat([]byte("x"), len(raw))} {
		if bad == nil {
			err = os.Remove(rawPath)
		} else {
			err = os.WriteFile(rawPath, bad, 0600)
		}
		if err != nil {
			t.Fatal(err)
		}
		if rc, _, err := importOne(ctx, db, store, parser, p); rc != importDuplicate || err != nil {
			t.Fatalf("raw repair %v %v", rc, err)
		}
		if got, err := os.ReadFile(rawPath); err != nil || !bytes.Equal(got, raw) {
			t.Fatal("raw not repaired", err)
		}
	}
	// A checkpoint follows its durable folder ID even after a name changes.
	if _, err := db.Pool().Exec(ctx, `UPDATE folders SET name='Archive' WHERE id=$1`, folderID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.LookupOrCreateFolder(ctx, mailboxID, "INBOX"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(attPath); err != nil {
		t.Fatal(err)
	}
	if rc, id, err := importOne(ctx, db, store, parser, p); rc != importDuplicate || id != folderID || err != nil {
		t.Fatalf("durable folder: %v %d %v", rc, id, err)
	}
}

func TestDuplicateImportFailureIsDurablyRetryable(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx := context.Background()
	gcMustMailbox(t, ctx, db, "recovery")
	root, storageRoot := t.TempDir(), t.TempDir()
	cfg := writeRenameConfig(t, dsn, storageRoot)
	writeMaildirMessage(t, root, "0001", duplicateAttachmentMessage)
	checkpoint := filepath.Join(root, "checkpoint")
	args := []string{"-config", cfg, "-maildir", root, "-mailbox", "recovery", "-checkpoint-file", checkpoint}
	if code := runImport(args); code != EX_OK {
		t.Fatal("seed", code)
	}
	var sha string
	if err := db.Pool().QueryRow(ctx, `SELECT encode(sha256,'hex') FROM attachments`).Scan(&sha); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `UPDATE attachments SET sha256=decode($1,'hex')`, strings.Repeat("ab", 32)); err != nil {
		t.Fatal(err)
	}
	if code := runImport(args); code != EX_TEMPFAIL {
		t.Fatal("unreconstructable duplicate acknowledged", code)
	}
	if code := runImport(append(args, "-resume")); code != EX_TEMPFAIL {
		t.Fatal("resume hid duplicate failure", code)
	}
	report, err := os.ReadFile(manifestPathFor(checkpoint))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(report, []byte("cannot reconstruct stored")) {
		t.Fatalf("missing integrity failure: %s", report)
	}
	if _, err := db.Pool().Exec(ctx, `UPDATE attachments SET sha256=decode($1,'hex')`, sha); err != nil {
		t.Fatal(err)
	}
	if code := runImport(append(args, "-retry-failures")); code != EX_OK {
		t.Fatal("retry", code)
	}
	if _, err := os.Stat(manifestPathFor(checkpoint)); !os.IsNotExist(err) {
		t.Fatal("stale failure", err)
	}
	var count int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM messages`).Scan(&count); err != nil || count != 1 {
		t.Fatal("retry changed message count", count, err)
	}
}
