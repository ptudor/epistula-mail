package main

import (
	"context"
	"fmt"
	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/ingest"
	"github.com/ptudor/epistula-mail/database/maildir"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const integrityMail = "From: a@x\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nbody\r\n--b\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=x.bin\r\n\r\ndata\r\n--b--\r\n"

func TestImportVerifyReportsAttachmentsAndOversizedSource(t *testing.T) {
	db, dsn := pgtest.Open(t)
	gcMustMailbox(t, context.Background(), db, "alice")
	root, source := t.TempDir(), t.TempDir()
	store := blob.NewStore(root)
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	cfg := writeTimedAdminConfig(t, dsn, root, "10s")
	f, err := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(f, "[limits]\nmax_message_bytes=1024\n")
	f.Close()
	writeMaildirMessage(t, source, "0001", integrityMail)
	if code := runImport([]string{"-config", cfg, "-maildir", source, "-mailbox", "alice"}); code != EX_OK {
		t.Fatal(code)
	}
	args := []string{"-config", cfg, "-maildir", source, "-mailbox", "alice"}
	if code := runImportVerify(args); code != EX_OK {
		t.Fatal(code)
	}
	if err := store.Walk(blob.KindAttachment, func(_ blob.Kind, _ blob.Tenant, _ blob.Bucket, _ string, path string, _ os.FileInfo) error {
		return os.Remove(path)
	}); err != nil {
		t.Fatal(err)
	}
	if code := runImportVerify(args); code != EX_DATAERR {
		t.Fatalf("missing attachment: %d", code)
	}
	writeMaildirMessage(t, source, "0000-too-large", strings.Repeat("x", 2048))
	if code := runImportVerify(args); code != EX_DATAERR {
		t.Fatalf("oversize source: %d", code)
	}
}
func TestImportBlobsReportsMisplacedAndUnknown(t *testing.T) {
	db, dsn := pgtest.Open(t)
	gcMustMailbox(t, context.Background(), db, "alice")
	root := t.TempDir()
	store := blob.NewStore(root)
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	_, canonical := writeTestBlob(t, store, "alice", "2003/06/15", []byte(integrityMail))
	misplaced := filepath.Join(root, "alice", "raw", "2003", "06", "15", "00", "00", filepath.Base(canonical))
	if misplaced == canonical {
		t.Fatal("fixture hash has unexpected shard")
	}
	if err := os.MkdirAll(filepath.Dir(misplaced), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(misplaced, []byte(integrityMail), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "Unknown"), 0750); err != nil {
		t.Fatal(err)
	}
	cfg := writeTimedAdminConfig(t, dsn, root, "10s")
	if code := runImportBlobs([]string{"-config", cfg}); code != EX_TEMPFAIL {
		t.Fatal(code)
	}
	var n int
	if err := db.Pool().QueryRow(context.Background(), `SELECT count(*) FROM messages`).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if _, err := os.Stat(misplaced); err != nil {
		t.Fatal(err)
	}
	var records string
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".json") {
			b, _ := os.ReadFile(path)
			records += string(b)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(records, "misplaced_blob") || !strings.Contains(records, "unknown_subtree") {
		t.Fatal(records)
	}
}
func TestReparseQueuesChangedProjectionOnly(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx := context.Background()
	gcMustMailbox(t, ctx, db, "alice")
	root, source := t.TempDir(), t.TempDir()
	store := blob.NewStore(root)
	store.Init()
	cfg := writeTimedAdminConfig(t, dsn, root, "10s")
	writeMaildirMessage(t, source, "one", integrityMail)
	if code := runImport([]string{"-config", cfg, "-maildir", source, "-mailbox", "alice"}); code != EX_OK {
		t.Fatal(code)
	}
	if _, err := db.Pool().Exec(ctx, `DELETE FROM annotation_pass_required; UPDATE messages SET text_body='stale'`); err != nil {
		t.Fatal(err)
	}
	for i, want := range []int{1, 0} {
		if _, err := reparseAll(ctx, db, store, ingest.New(ingest.DefaultLimits()), reparseParams{BatchSize: 10}); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM annotation_pass_required`).Scan(&n); err != nil || n != want {
			t.Fatalf("pass%d got%d want%d err%v", i, n, want, err)
		}
		if _, err := db.Pool().Exec(ctx, `DELETE FROM annotation_pass_required`); err != nil {
			t.Fatal(err)
		}
	}
}
func TestMaintenanceWalkClosedPoolIsTemporary(t *testing.T) {
	db, _ := pgtest.Open(t)
	source := t.TempDir()
	writeMaildirMessage(t, source, "one", integrityMail)
	err := maildir.Walk(source, func(maildir.Entry) error {
		db.Pool().Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := db.Pool().Exec(ctx, "SELECT 1")
		return fmt.Errorf("walk query: %w", err)
	})
	if err == nil || maintenanceWalkExit(err) != EX_TEMPFAIL {
		t.Fatal(err)
	}
	if maintenanceWalkExit(&os.PathError{Op: "read", Path: "missing", Err: os.ErrNotExist}) != EX_OSERR {
		t.Fatal("filesystem classification")
	}
}
