package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

func TestGCStagingDenialIsPartialFailure(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := context.Background()
	gcMustMailbox(t, ctx, db, "staging")
	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(store.Root(), "staging", "tmp")
	if err := os.MkdirAll(tmp, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tmp, "blob-stale")
	if err := os.WriteFile(path, []byte("unfinished"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(tmp, 0700)
	j, err := openRecovery(filepath.Join(t.TempDir(), "gc"), jobIdentity{Kind: "gc-staging"}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if code := gcMark(ctx, db, store, 24*time.Hour, j); code != EX_TEMPFAIL {
		t.Fatal("unlink denial", code)
	}
	if ok, err := j.contains(path); !ok || err != nil {
		t.Fatal("missing failed staging path", err)
	}
	if err := os.Chmod(tmp, 0700); err != nil {
		t.Fatal(err)
	}
	if code := gcMark(ctx, db, store, 24*time.Hour, j); code != EX_OK {
		t.Fatal("cleanup retry", code)
	}
	if j.count != 0 {
		t.Fatal("stale staging failure", j.count)
	}
}

func TestRecoveryQueueSurvivesUnpublishedReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint")
	job := jobIdentity{Kind: "maildir-import", Source: "archive"}
	j, err := openRecovery(path, job, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openRecovery(path, job, false, false); err == nil {
		t.Fatal("concurrent writer accepted")
	}
	if err := j.fail("cur/bad", "parse_failed", os.ErrInvalid); err != nil {
		t.Fatal(err)
	}
	state, _ := loadCheckpoint(path, false, job, false)
	state.LastKey = "cur/later"
	if err := state.save(path); err != nil {
		t.Fatal(err)
	}
	// No finish/report publication: exactly the crash window that lost failures.
	j.Close()
	if _, err := os.Stat(manifestPathFor(path)); !os.IsNotExist(err) {
		t.Fatal("unexpected report")
	}
	j, err = openRecovery(path, job, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if j.count != 1 {
		t.Fatal(j.count)
	}
	if code := recoveryOutcome(j, true); code != EX_TEMPFAIL {
		t.Fatal("missing retry source reported success", code)
	}
	if err := j.resolved("cur/bad"); err != nil {
		t.Fatal(err)
	}
	if code := recoveryOutcome(j, true); code != EX_OK {
		t.Fatal(code)
	}
	j.Close()
	job.Source = "another archive"
	if _, err := openRecovery(path, job, false, true); err == nil {
		t.Fatal("foreign failure queue accepted")
	}
	if _, err := openRecovery(path+"absent", job, false, true); err == nil {
		t.Fatal("missing retry queue accepted")
	}
}

func TestRecoveryQueueRetainsMoreThanTenThousand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint")
	job := jobIdentity{Kind: "test"}
	j, err := openRecovery(path, job, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	const count = 10017
	for i := 0; i < count; i++ {
		if err := j.fail(fmt.Sprintf("cur/%08d", i), "read_failed", os.ErrPermission); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	if growth := int64(after.HeapAlloc) - int64(before.HeapAlloc); growth > 4<<20 {
		t.Fatalf("queue retained %d bytes", growth)
	}
	if n, err := j.finish(); err != nil || n != count {
		t.Fatalf("%d %v", n, err)
	}
	f, err := os.Open(manifestPathFor(path))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var report struct{ Items []failedItem }
	if err := json.NewDecoder(f).Decode(&report); err != nil {
		t.Fatal(err)
	}
	if len(report.Items) != count {
		t.Fatal("lost failures", len(report.Items))
	}
	if ok, err := j.contains("cur/00010016"); err != nil || !ok {
		t.Fatal("last item not retryable", err)
	}
}

func TestImportFailureRetryAndReparseCLI(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx := context.Background()
	gcMustMailbox(t, ctx, db, "recovery")
	root, storageRoot := t.TempDir(), t.TempDir()
	cfg := writeRenameConfig(t, dsn, storageRoot)
	config, err := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.WriteString("\n[limits]\nmax_message_bytes=1024\n"); err != nil {
		t.Fatal(err)
	}
	config.Close()
	writeMaildirMessage(t, root, "0001", dryRunMsg("one"))
	writeMaildirMessage(t, root, "0002", strings.Repeat("x", 1025))
	writeMaildirMessage(t, root, "0003", dryRunMsg("three"))
	ckpt := filepath.Join(root, "checkpoint")
	args := []string{"-config", cfg, "-maildir", root, "-mailbox", "recovery", "-checkpoint-file", ckpt}
	if code := runImport(args); code != EX_TEMPFAIL {
		t.Fatal("partial import", code)
	}
	count := func() int {
		var n int
		if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM messages`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count() != 2 {
		t.Fatal("good messages not imported")
	}
	if code := runImport(append(args, "-resume")); code != EX_TEMPFAIL {
		t.Fatal("resume hid failure", code)
	}
	if err := os.Remove(filepath.Join(root, "cur", "0002")); err != nil {
		t.Fatal(err)
	}
	if code := runImport(append(args, "-retry-failures")); code != EX_TEMPFAIL {
		t.Fatal("missing retry source", code)
	}
	writeMaildirMessage(t, root, "0002", dryRunMsg("repaired"))
	if code := runImport(append(args, "-retry-failures")); code != EX_OK {
		t.Fatal("retry", code)
	}
	if count() != 3 {
		t.Fatal("retry duplicated good messages")
	}
	if _, err := os.Stat(manifestPathFor(ckpt)); !os.IsNotExist(err) {
		t.Fatal("resolved report not cleared")
	}
	if code := runImport(append(args, "-retry-failures")); code != EX_OK || count() != 3 {
		t.Fatal("clean retry", code)
	}

	var id int64
	var sha, bucket string
	if err := db.Pool().QueryRow(ctx, `SELECT id,encode(raw_sha256,'hex'),to_char(raw_blob_date,'YYYY/MM/DD') FROM messages ORDER BY id LIMIT 1`).Scan(&id, &sha, &bucket); err != nil {
		t.Fatal(err)
	}
	store := blob.NewStore(storageRoot)
	path, err := store.PathFor(blob.KindRaw, "recovery", blob.Bucket(bucket), sha)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", len(raw))), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(t.TempDir(), "reparse")
	rargs := []string{"-config", cfg, "-manifest", manifest}
	if code := runReparseBodystructure(rargs); code != EX_TEMPFAIL {
		t.Fatal("corrupt raw reparse", code)
	}
	job := jobIdentity{Kind: "reparse-bodystructure", Source: canonicalSource(storageRoot), Destination: redactedDSN(dsn)}
	if err := bindJob(ctx, db, &job); err != nil {
		t.Fatal(err)
	}
	j, err := openRecovery(manifest, job, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := j.contains(strconv.FormatInt(id, 10)); !ok || err != nil {
		t.Fatal("missing failed ID", err)
	}
	j.Close()
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if code := runReparseBodystructure(append(rargs, "-retry-failures")); code != EX_OK {
		t.Fatal("reparse retry", code)
	}
	if count() != 3 {
		t.Fatal("reparse changed message identities")
	}
}
