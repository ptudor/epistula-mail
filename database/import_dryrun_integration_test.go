package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/pgtest"
)

// dryRunMsg returns a distinct, parseable RFC 5322 message keyed by tag, so
// each produces a unique sha256 (and thus a distinct messages row).
func dryRunMsg(tag string) string {
	return "From: sender@dryrun.invalid\r\n" +
		"To: rcpt@dryrun.invalid\r\n" +
		"Subject: dry-run fixture " + tag + "\r\n" +
		"Date: Tue, 01 Jul 2003 10:00:00 +0000\r\n" +
		"\r\n" +
		"body " + tag + "\r\n"
}

func writeMaildirMessage(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, "cur")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir cur: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func readCheckpointKey(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read checkpoint %s: %v", path, err)
	}
	var c checkpoint
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("parse checkpoint: %v", err)
	}
	return c.LastKey
}

// TestImportDryRunCreatesNoFolder is the R-023 verification (folder side): a
// pure dry-run against an empty mailbox must not create a folder row, and must
// not write a checkpoint file.
func TestImportDryRunCreatesNoFolder(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	mboxID := gcMustMailbox(t, ctx, db, "dryrunbox")
	root := t.TempDir()
	writeMaildirMessage(t, root, "0001", dryRunMsg("one"))
	writeMaildirMessage(t, root, "0002", dryRunMsg("two"))

	cfg := writeAdminConfig(t, dsn)
	if code := runImport([]string{"-config", cfg, "-maildir", root, "-mailbox", "dryrunbox", "-dry-run"}); code != EX_OK {
		t.Fatalf("dry-run import exit=%d, want EX_OK", code)
	}

	var nFolders int
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM folders WHERE mailbox_id = $1`, mboxID,
	).Scan(&nFolders); err != nil {
		t.Fatalf("count folders: %v", err)
	}
	if nFolders != 0 {
		t.Fatalf("dry-run created %d folder row(s) against an empty mailbox, want 0", nFolders)
	}

	if _, err := os.Stat(filepath.Join(root, ".epistula-database-import.ckpt")); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote a checkpoint file (stat err=%v), want none", err)
	}
}

// TestImportVerifyMissingFolderExitsWithoutCreating is the R-023 verification
// (import-verify side): verifying a folder that doesn't exist must exit EX_USAGE
// and must NOT create the folder (which would then "verify" empty).
func TestImportVerifyMissingFolderExitsWithoutCreating(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	mboxID := gcMustMailbox(t, ctx, db, "verifybox")
	root := t.TempDir()
	writeMaildirMessage(t, root, "0001", dryRunMsg("one"))

	cfg := writeAdminConfig(t, dsn)
	code := runImportVerify([]string{"-config", cfg, "-maildir", root, "-mailbox", "verifybox", "-folder", "Nonexistent"})
	if code != EX_USAGE {
		t.Fatalf("import-verify on a missing folder exit=%d, want EX_USAGE(%d)", code, EX_USAGE)
	}

	var n int
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM folders WHERE mailbox_id = $1 AND name = 'Nonexistent'`, mboxID,
	).Scan(&n); err != nil {
		t.Fatalf("count folders: %v", err)
	}
	if n != 0 {
		t.Fatalf("import-verify created the missing folder (%d row(s))", n)
	}
}

// TestImportDryRunDoesNotClobberCheckpoint is the R-023 verification (checkpoint
// side): after a partial real import, a dry-run over the whole tree must NOT
// overwrite the resume checkpoint, so a later -resume imports the remainder
// rather than silently skipping it as done.
func TestImportDryRunDoesNotClobberCheckpoint(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	mboxID := gcMustMailbox(t, ctx, db, "resumebox")
	root := t.TempDir()
	cfg := writeAdminConfig(t, dsn)
	ckpt := filepath.Join(root, ".epistula-database-import.ckpt")

	// Step 1: a partial real import — only A and B are on disk yet.
	writeMaildirMessage(t, root, "0001", dryRunMsg("A"))
	writeMaildirMessage(t, root, "0002", dryRunMsg("B"))
	if code := runImport([]string{"-config", cfg, "-maildir", root, "-mailbox", "resumebox"}); code != EX_OK {
		t.Fatalf("real import #1 exit=%d", code)
	}
	before := readCheckpointKey(t, ckpt)
	if before == "" {
		t.Fatal("expected a checkpoint LastKey after the first real import")
	}

	// Step 2: the rest of the mail arrives; an operator dry-runs the whole tree
	// to inspect before resuming. This must not touch the checkpoint.
	writeMaildirMessage(t, root, "0003", dryRunMsg("C"))
	writeMaildirMessage(t, root, "0004", dryRunMsg("D"))
	if code := runImport([]string{"-config", cfg, "-maildir", root, "-mailbox", "resumebox", "-resume", "-dry-run"}); code != EX_OK {
		t.Fatalf("dry-run exit=%d", code)
	}
	if after := readCheckpointKey(t, ckpt); after != before {
		t.Fatalf("dry-run clobbered the checkpoint: before=%q after=%q", before, after)
	}

	// Step 3: the real resume must import C and D (4 messages total). With the
	// bug, the clobbered checkpoint would make this a no-op (only 2 rows).
	if code := runImport([]string{"-config", cfg, "-maildir", root, "-mailbox", "resumebox", "-resume"}); code != EX_OK {
		t.Fatalf("real resume exit=%d", code)
	}
	var n int
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM messages m JOIN folders f ON f.id = m.folder_id WHERE f.mailbox_id = $1`, mboxID,
	).Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if n != 4 {
		t.Fatalf("after resume the folder has %d messages, want 4 (dry-run must not have advanced the checkpoint past C/D)", n)
	}
}
