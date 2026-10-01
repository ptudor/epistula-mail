package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ptudor/epistula-mail/database/maintenance"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

func TestOfflineRenameBarrierAndRecovery(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx := context.Background()
	id := gcMustMailbox(t, ctx, db, "oldbox")
	root, source := t.TempDir(), t.TempDir()
	cfg := writeRenameConfig(t, dsn, root)
	writeMaildirMessage(t, source, "0001", dryRunMsg("rename"))
	if code := runImport([]string{"-config", cfg, "-maildir", source, "-mailbox", "oldbox"}); code != EX_OK {
		t.Fatal(code)
	}
	snapshot := func() string {
		var s string
		if err := db.Pool().QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY id)::text FROM messages m`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()
	lease, err := maintenance.Acquire(ctx, root, db.Pool(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if code := adminMailboxMaintenance([]string{"-config", cfg, "-name", "oldbox", "-on"}); code != EX_TEMPFAIL {
		t.Fatal("in-flight blob operation not drained", code)
	}
	lease.Close()
	mustQuiesce(t, cfg, "oldbox")
	if _, err := maintenance.Acquire(ctx, root, db.Pool(), false, false); err == nil {
		t.Fatal("new blob reader admitted")
	}
	if code := runImport([]string{"-config", cfg, "-maildir", source, "-mailbox", "oldbox"}); code != EX_TEMPFAIL {
		t.Fatal("new import admitted", code)
	}
	if code := runGC([]string{"-config", cfg, "-phase", "mark", "-manifest", filepath.Join(t.TempDir(), "gc")}); code != EX_TEMPFAIL {
		t.Fatal("GC admitted during move", code)
	}
	if code := runReparseBodystructure([]string{"-config", cfg, "-manifest", filepath.Join(t.TempDir(), "reparse")}); code != EX_TEMPFAIL {
		t.Fatal("reparse admitted during move", code)
	}
	oldPath, newPath := filepath.Join(root, "oldbox"), filepath.Join(root, "newbox")
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatal(err)
	}
	if code := adminMailboxMaintenance([]string{"-config", cfg, "-name", "oldbox", "-off"}); code != EX_TEMPFAIL {
		t.Fatal("released wrong tree", code)
	}
	var rawPath string
	if err := filepath.WalkDir(newPath, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if filepath.Ext(p) == ".eml" {
			rawPath = p
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(rawPath)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), raw...)
	corrupt[len(corrupt)-1] ^= 1
	if err := os.WriteFile(rawPath, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	rename := []string{"-config", cfg, "-from", "oldbox", "-to", "newbox", "-yes"}
	if code := adminMailboxRename(rename); code == EX_OK {
		t.Fatal("equal-size corrupt destination accepted")
	}
	if mailboxName(t, ctx, db, id) != "oldbox" {
		t.Fatal("failed rename changed identity")
	}
	if err := os.WriteFile(rawPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if code := adminMailboxRename(rename); code != EX_OK {
		t.Fatal("retry after restoration", code)
	}
	if mailboxName(t, ctx, db, id) != "newbox" || snapshot() != before {
		t.Fatal("rename altered message identities/content/flags/dates")
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatal("old tenant recreated")
	}
	lease, err = maintenance.Acquire(ctx, root, db.Pool(), false, false)
	if err != nil {
		t.Fatal("normal service not released", err)
	}
	lease.Close()
}
