package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ptudor/epistula-mail/database/pgtest"
)

func TestMaintenanceManifestSurvivesMailboxChanges(t *testing.T) {
	db, dsn := pgtest.Open(t)
	root := t.TempDir()
	cfg := writeRenameConfig(t, dsn, root)
	gcMustMailbox(t, context.Background(), db, "a")
	gcManifest, reparseManifest := filepath.Join(root, "gc"), filepath.Join(root, "reparse")
	for i := 0; i < 2; i++ {
		if code := runGC([]string{"-config", cfg, "-phase", "mark", "-manifest", gcManifest}); code != EX_OK {
			t.Fatalf("GC pass %d: %d", i, code)
		}
		if code := runReparseBodystructure([]string{"-config", cfg, "-manifest", reparseManifest}); code != EX_OK {
			t.Fatalf("reparse pass %d: %d", i, code)
		}
		if i == 0 {
			gcMustMailbox(t, context.Background(), db, "b")
		}
	}
}

func TestDefaultMaintenanceManifestIgnoresWorkingDirectory(t *testing.T) {
	_, dsn := pgtest.Open(t)
	root := t.TempDir()
	cfg := writeRenameConfig(t, dsn, root)
	t.Chdir("/")
	if code := runGC([]string{"-config", cfg, "-phase", "mark"}); code != EX_OK {
		t.Fatalf("GC from /: %d", code)
	}
	if code := runReparseBodystructure([]string{"-config", cfg}); code != EX_OK {
		t.Fatalf("reparse from /: %d", code)
	}
	for _, prefix := range []string{".epistula-database-gc", ".epistula-database-reparse"} {
		if _, err := os.Stat(filepath.Join(root, prefix+".failures.d", "job.json")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecoveryMissingIdentityDryRun(t *testing.T) {
	for _, records := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "records"}[records], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "x")
			job := jobIdentity{Kind: "gc-staging"}
			j, err := openRecovery(path, job, false, false)
			if err != nil {
				t.Fatal(err)
			}
			if records {
				if err := j.fail("bad", "unreadable", os.ErrPermission); err != nil {
					t.Fatal(err)
				}
			}
			j.Close()
			identity := filepath.Join(path+".failures.d", "job.json")
			if err := os.Remove(identity); err != nil {
				t.Fatal(err)
			}
			j, err = openRecovery(path, job, true, false)
			if records {
				if err == nil || !strings.Contains(err.Error(), path+".failures.d") || !strings.Contains(err.Error(), "rerun the original job") {
					t.Fatalf("missing recovery guidance: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				j.Close()
			}
			if _, err := os.Stat(identity); !os.IsNotExist(err) {
				t.Fatalf("dry run wrote identity: %v", err)
			}
		})
	}
}

func TestImportRetryAcceptsPreviouslyAbsentFolder(t *testing.T) {
	db, dsn := pgtest.Open(t)
	mailboxID := gcMustMailbox(t, context.Background(), db, "retry")
	cfg := writeRenameConfig(t, dsn, t.TempDir())
	source := t.TempDir()
	writeMaildirMessage(t, source, "one", "invalid top-level header\r\n\r\nbody\r\n")
	args := []string{"-config", cfg, "-maildir", source, "-mailbox", "retry"}
	if code := runImport(args); code != EX_TEMPFAIL {
		t.Fatalf("failed initial import: %d", code)
	}
	if _, err := db.LookupOrCreateFolder(context.Background(), mailboxID, "INBOX"); err != nil {
		t.Fatal(err)
	}
	writeMaildirMessage(t, source, "one", dryRunMsg("repaired"))
	if code := runImport(append(args, "-retry-failures")); code != EX_OK {
		t.Fatalf("retry after external folder creation: %d", code)
	}
	if code := runImport(append(args, "-resume")); code != EX_OK {
		t.Fatalf("resume after external folder creation: %d", code)
	}
}

func TestUnfilteredBlobImportRetainsRecordedTenantBindings(t *testing.T) {
	old := jobIdentity{Kind: "blob-import", Folder: "INBOX", Targets: map[string]jobTarget{"a": {MailboxID: 1, FolderID: 2}}}
	now := jobIdentity{Kind: "blob-import", Folder: "INBOX", Targets: map[string]jobTarget{"a": {MailboxID: 3, FolderID: 4}, "b": {MailboxID: 5}}}
	path := filepath.Join(t.TempDir(), "checkpoint")
	state, _ := loadCheckpoint(path, false, old, false)
	// Also accept the checksum emitted by the previous full-catalog format.
	state.Fingerprint = old.fullFingerprint()
	if err := state.save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadCheckpoint(path, true, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Job.Targets["a"] != old.Targets["a"] || loaded.Job.Targets["b"] != now.Targets["b"] {
		t.Fatalf("lost saved identity or new tenant: %+v", loaded.Job.Targets)
	}
}
