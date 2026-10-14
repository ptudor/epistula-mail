package main

import (
	"context"
	"testing"

	"github.com/ptudor/epistula-mail/database/pgtest"
)

func TestImportCanonicalFolderAndValidation(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx := context.Background()
	id := gcMustMailbox(t, ctx, db, "foldercase")
	root := t.TempDir()
	writeMaildirMessage(t, root, "0001", dryRunMsg("foldercase"))
	cfg := writeAdminConfig(t, dsn)
	args := []string{"-config", cfg, "-maildir", root, "-mailbox", "foldercase", "-folder"}
	if code := runImport(append(args, "Sent/")); code != EX_USAGE {
		t.Fatalf("bad folder: %d", code)
	}
	var n int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM folders WHERE mailbox_id=$1`, id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("folders=%d err=%v", n, err)
	}
	if code := runImportBlobs([]string{"-config", cfg, "-folder", "a//b"}); code != EX_USAGE {
		t.Fatalf("bad blob-import folder: %d", code)
	}
	if code := runImport(append(args, "inbox")); code != EX_OK {
		t.Fatalf("inbox import: %d", code)
	}
	var name string
	if err := db.Pool().QueryRow(ctx, `SELECT f.name FROM folders f JOIN messages m ON m.folder_id=f.id WHERE f.mailbox_id=$1`, id).Scan(&name); err != nil || name != "INBOX" {
		t.Fatalf("folder=%q err=%v", name, err)
	}
	if got := canonicalImportFolderName("inbox/Child"); got != "INBOX/Child" {
		t.Fatal(got)
	}
}
