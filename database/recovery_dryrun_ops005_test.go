package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// OPS-005: a dry run of a recovery pass reports what the same real run would
// leave unresolved. It used to start from the records on disk, add every
// failure again whether or not it was already recorded, and never subtract
// the records the pass would clear. A -retry-failures -dry-run whose 8 items
// would all import therefore printed "8 unresolved item(s)".

// captureStderr runs f with os.Stderr redirected to a pipe and returns f's
// result and everything written to stderr meanwhile.
func captureStderr(t *testing.T, f func() int) (int, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	code := f()
	os.Stderr = saved
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out := <-done
	r.Close()
	return code, out
}

// queueSnapshot lists a failure queue's files with their contents, to prove a
// dry run left it exactly as it was.
func queueSnapshot(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s %s\n", e.Name(), data)
	}
	return b.String()
}

func journalKeys(t *testing.T, j *recoveryJournal) []string {
	t.Helper()
	var keys []string
	if err := j.each(func(item failedItem) error { keys = append(keys, item.Key); return nil }); err != nil {
		t.Fatal(err)
	}
	sort.Strings(keys)
	return keys
}

// TestDryRunJournalAnswersAsTheRealRun applies the same fail/resolved sequence
// to a dry journal and to a real journal over a copy of the same queue. After
// every step the two must agree on the count and on every key, and the dry
// journal's queue must be untouched at the end.
func TestDryRunJournalAnswersAsTheRealRun(t *testing.T) {
	job := jobIdentity{Kind: "maildir-import", Source: "archive"}
	seed := func(path string) {
		j, err := openRecovery(path, job, false, false)
		if err != nil {
			t.Fatal(err)
		}
		defer j.Close()
		for _, k := range []string{"cur/0", "cur/1", "cur/2", "cur/3"} {
			if err := j.fail(k, "parse_failed", os.ErrInvalid); err != nil {
				t.Fatal(err)
			}
		}
	}
	dryPath := filepath.Join(t.TempDir(), "checkpoint")
	realPath := filepath.Join(t.TempDir(), "checkpoint")
	seed(dryPath)
	seed(realPath)
	before := queueSnapshot(t, dryPath+".failures.d")

	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprintf("retry=%v", retry), func(t *testing.T) {
			realCopy := filepath.Join(t.TempDir(), "checkpoint")
			if err := os.CopyFS(realCopy+".failures.d", os.DirFS(realPath+".failures.d")); err != nil {
				t.Fatal(err)
			}
			dry, err := openRecovery(dryPath, job, true, retry)
			if err != nil {
				t.Fatal(err)
			}
			defer dry.Close()
			real, err := openRecovery(realCopy, job, false, retry)
			if err != nil {
				t.Fatal(err)
			}
			defer real.Close()

			keys := []string{"cur/0", "cur/1", "cur/2", "cur/3", "cur/4", "cur/5"}
			steps := []struct {
				op, key string
			}{
				{"resolved", "cur/0"}, // a record the pass clears
				{"resolved", "cur/0"}, // cleared once only
				{"fail", "cur/1"},     // still failing: already recorded
				{"fail", "cur/4"},     // a new failure
				{"fail", "cur/4"},     // recorded once only
				{"resolved", "cur/4"}, // a new failure cleared again
				{"fail", "cur/0"},     // a cleared record failing again
				{"resolved", "cur/5"}, // never recorded
				{"fail", "cur/5"},
				{"resolved", "cur/2"},
			}
			for i, s := range steps {
				for _, j := range []*recoveryJournal{dry, real} {
					var err error
					if s.op == "fail" {
						err = j.fail(s.key, "parse_failed", os.ErrInvalid)
					} else {
						err = j.resolved(s.key)
					}
					if err != nil {
						t.Fatalf("step %d %s %s (dry=%v): %v", i, s.op, s.key, j.dry, err)
					}
				}
				if dry.count != real.count {
					t.Fatalf("after step %d (%s %s): dry count %d, real count %d", i, s.op, s.key, dry.count, real.count)
				}
				for _, k := range keys {
					d, err := dry.contains(k)
					if err != nil {
						t.Fatal(err)
					}
					r, err := real.contains(k)
					if err != nil {
						t.Fatal(err)
					}
					if d != r {
						t.Fatalf("after step %d (%s %s): contains(%s) dry %v, real %v", i, s.op, s.key, k, d, r)
					}
				}
			}
			d, r := strings.Join(journalKeys(t, dry), " "), strings.Join(journalKeys(t, real), " ")
			if d != r || r != "cur/0 cur/1 cur/3 cur/5" {
				t.Errorf("each: dry visits %q, the real queue holds %q; want cur/0 cur/1 cur/3 cur/5", d, r)
			}
			dn, err := dry.finish()
			if err != nil {
				t.Fatal(err)
			}
			rn, err := real.finish()
			if err != nil || dn != rn || dn != 4 {
				t.Errorf("finish: dry %d, real %d (%v); want 4 unresolved", dn, rn, err)
			}
		})
	}
	if after := queueSnapshot(t, dryPath+".failures.d"); after != before {
		t.Errorf("a dry run changed its queue:\nbefore %s\nafter  %s", before, after)
	}

	// With no queue on disk a dry run counts the failures it finds and
	// creates nothing.
	fresh := filepath.Join(t.TempDir(), "checkpoint")
	dry, err := openRecovery(fresh, job, true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer dry.Close()
	for _, k := range []string{"cur/a", "cur/b", "cur/a"} {
		if err := dry.fail(k, "parse_failed", os.ErrInvalid); err != nil {
			t.Fatal(err)
		}
	}
	if err := dry.resolved("cur/b"); err != nil {
		t.Fatal(err)
	}
	if n, err := dry.finish(); err != nil || n != 1 {
		t.Errorf("dry run without a queue: %d unresolved (%v), want 1", n, err)
	}
	if _, err := os.Stat(fresh + ".failures.d"); !os.IsNotExist(err) {
		t.Errorf("a dry run created a queue: %v", err)
	}
}

// TestImportRetryDryRunForecastsTheRetry is the OPS-005 regression: a first pass
// leaves 8 items in the queue, a -retry-failures -dry-run under limits that
// read all 8 must not call any of them unresolved, and in general the dry run's
// forecast is exactly what the real retry then leaves.
func TestImportRetryDryRunForecastsTheRetry(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	gcMustMailbox(t, ctx, db, "forecast")

	root, storageRoot := t.TempDir(), t.TempDir()
	message := func(n int) string {
		return fmt.Sprintf("From: a@ops005.invalid\r\nSubject: item %d\r\n%s\r\nbody %d\r\n", n, pad, n)
	}
	for n := 1; n <= 8; n++ {
		writeMaildirMessage(t, root, fmt.Sprintf("%04d", n), message(n))
	}
	strict := writeRenameConfig(t, dsn, storageRoot)
	f, err := os.OpenFile(strict, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n[limits]\nmax_message_bytes=256\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	cfg := writeRenameConfig(t, dsn, storageRoot)

	ckpt := filepath.Join(root, "forecast.ckpt")
	args := func(config string, extra ...string) []string {
		return append([]string{"-config", config, "-maildir", root, "-mailbox", "forecast", "-checkpoint-file", ckpt}, extra...)
	}
	count := func() int {
		var n int
		if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM messages`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	queue := ckpt + ".failures.d"
	records := func() int {
		entries, err := os.ReadDir(queue)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") && e.Name() != "job.json" {
				n++
			}
		}
		return n
	}

	if code, _ := captureStderr(t, func() int { return runImport(args(strict)) }); code != EX_TEMPFAIL || records() != 8 || count() != 0 {
		t.Fatalf("pass 1: exit %d, %d record(s), %d message(s); want EX_TEMPFAIL, 8, 0", code, records(), count())
	}
	before := queueSnapshot(t, queue)

	// All 8 would import: nothing would remain unresolved.
	code, stderr := captureStderr(t, func() int { return runImport(args(cfg, "-retry-failures", "-dry-run")) })
	if code != EX_OK || strings.Contains(stderr, "unresolved") {
		t.Errorf("dry-run retry of 8 importable items: exit %d, stderr %q; want EX_OK and no unresolved items", code, stderr)
	}
	if count() != 0 || queueSnapshot(t, queue) != before {
		t.Fatal("the dry run wrote a message or changed the queue")
	}

	// Now one item is unreadable and one source is gone, so the pass does not
	// reach it. The forecast is 2, and the real retry leaves exactly 2.
	writeMaildirMessage(t, root, "0007", "not a message, no header field and no empty line "+strings.Repeat("g", 300))
	if err := os.Remove(filepath.Join(root, "cur", "0008")); err != nil {
		t.Fatal(err)
	}
	code, stderr = captureStderr(t, func() int { return runImport(args(cfg, "-retry-failures", "-dry-run")) })
	if code != EX_TEMPFAIL || !strings.Contains(stderr, "dry run: 2 item(s) would remain unresolved in "+queue) {
		t.Errorf("dry-run retry: exit %d, stderr %q; want EX_TEMPFAIL and 2 would remain", code, stderr)
	}
	if count() != 0 || queueSnapshot(t, queue) != before {
		t.Fatal("the dry run wrote a message or changed the queue")
	}
	code, stderr = captureStderr(t, func() int { return runImport(args(cfg, "-retry-failures")) })
	if code != EX_TEMPFAIL || !strings.Contains(stderr, "2 unresolved item(s) in "+queue) || records() != 2 || count() != 6 {
		t.Errorf("real retry: exit %d, stderr %q, %d record(s), %d message(s); want EX_TEMPFAIL, 2, 2, 6",
			code, stderr, records(), count())
	}
}

// TestReparseRetryDryRunForecastsTheRetry covers reparse-bodystructure
// -manifest. Of four recorded failures, one row now reparses unchanged, one
// would be rewritten, one was expunged, and one blob is still corrupt. The
// dry run used to leave all four unresolved: it did not clear a row it would
// rewrite, and it skipped the reconcile that clears expunged rows.
func TestReparseRetryDryRunForecastsTheRetry(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	gcMustMailbox(t, ctx, db, "reforecast")

	root, storageRoot := t.TempDir(), t.TempDir()
	for n := 1; n <= 4; n++ {
		writeMaildirMessage(t, root, fmt.Sprintf("%04d", n), dryRunMsg(strconv.Itoa(n)))
	}
	cfg := writeRenameConfig(t, dsn, storageRoot)
	if code := runImport([]string{"-config", cfg, "-maildir", root, "-mailbox", "reforecast"}); code != EX_OK {
		t.Fatalf("import exit %d", code)
	}

	type stored struct {
		id   int64
		path string
		raw  []byte
	}
	var msgs []stored
	rows, err := db.Pool().Query(ctx, `SELECT id, encode(raw_sha256,'hex'), to_char(raw_blob_date,'YYYY/MM/DD') FROM messages ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	store := blob.NewStore(storageRoot)
	for rows.Next() {
		var m stored
		var sha, bucket string
		if err := rows.Scan(&m.id, &sha, &bucket); err != nil {
			t.Fatal(err)
		}
		if m.path, err = store.PathFor(blob.KindRaw, "reforecast", blob.Bucket(bucket), sha); err != nil {
			t.Fatal(err)
		}
		msgs = append(msgs, m)
	}
	rows.Close()
	if len(msgs) != 4 {
		t.Fatalf("%d messages imported, want 4", len(msgs))
	}
	// Corrupt every raw blob, keeping its size, so the first pass records
	// all four.
	for i := range msgs {
		if msgs[i].raw, err = os.ReadFile(msgs[i].path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(msgs[i].path, []byte(strings.Repeat("x", len(msgs[i].raw))), 0600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := filepath.Join(t.TempDir(), "reparse")
	rargs := []string{"-config", cfg, "-manifest", manifest}
	if code, _ := captureStderr(t, func() int { return runReparseBodystructure(rargs) }); code != EX_TEMPFAIL {
		t.Fatalf("reparse over corrupt blobs: exit %d", code)
	}

	// The first reparses unchanged, the second would be rewritten, the third
	// is expunged, the fourth stays corrupt.
	for _, m := range msgs[:3] {
		if err := os.WriteFile(m.path, m.raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Pool().Exec(ctx, `UPDATE messages SET text_body = 'stale' WHERE id = $1`, msgs[1].id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `DELETE FROM messages WHERE id = $1`, msgs[2].id); err != nil {
		t.Fatal(err)
	}
	queue := manifest + ".failures.d"
	before := queueSnapshot(t, queue)

	code, stderr := captureStderr(t, func() int { return runReparseBodystructure(append(rargs, "-retry-failures", "-dry-run")) })
	if code != EX_TEMPFAIL || !strings.Contains(stderr, "dry run: 1 item(s) would remain unresolved in "+queue) {
		t.Errorf("dry-run retry: exit %d, stderr %q; want EX_TEMPFAIL and 1 would remain", code, stderr)
	}
	if queueSnapshot(t, queue) != before {
		t.Fatal("the dry run changed the queue")
	}
	var stale string
	if err := db.Pool().QueryRow(ctx, `SELECT text_body FROM messages WHERE id = $1`, msgs[1].id).Scan(&stale); err != nil || stale != "stale" {
		t.Fatalf("the dry run rewrote a row: %q, %v", stale, err)
	}

	code, stderr = captureStderr(t, func() int { return runReparseBodystructure(append(rargs, "-retry-failures")) })
	if code != EX_TEMPFAIL || !strings.Contains(stderr, "1 unresolved item(s) in "+queue) {
		t.Errorf("real retry: exit %d, stderr %q; want EX_TEMPFAIL and 1 unresolved", code, stderr)
	}
	job := jobIdentity{Kind: "reparse-bodystructure", Source: canonicalSource(storageRoot), Destination: redactedDSN(dsn)}
	if err := bindJob(ctx, db, &job); err != nil {
		t.Fatal(err)
	}
	j, err := openRecovery(manifest, job, true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if keys := journalKeys(t, j); len(keys) != 1 || keys[0] != strconv.FormatInt(msgs[3].id, 10) {
		t.Errorf("queue after the retry holds %v, want only message %d", keys, msgs[3].id)
	}
}
