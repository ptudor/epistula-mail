package main

import (
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestCheckpointRefusesAnotherJob is the RA6X-031 regression.
//
// A checkpoint recorded only LastKey, Count and StartedAt, and resume compared
// each key against that unqualified marker. Pointing a second import at the
// same checkpoint path SILENTLY SKIPPED every key the first job had passed,
// and the incomplete result looked exactly like a successful resume.
func TestCheckpointRefusesAnotherJob(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "import.ckpt")

	jobA := jobIdentity{
		Kind: "maildir-import", Source: "/srv/mail/alice/Maildir",
		Destination: "db.invalid/mail", Mailbox: "alice", Folder: "INBOX",
	}
	state, err := loadCheckpoint(path, false, jobA, false)
	if err != nil {
		t.Fatalf("fresh checkpoint: %v", err)
	}
	state.LastKey = "cur/1234.M1"
	state.Count = 5000
	if err := state.save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	// The same job resumes.
	resumed, err := loadCheckpoint(path, true, jobA, false)
	if err != nil {
		t.Fatalf("resuming the same job failed: %v", err)
	}
	if resumed.resumeKey() != "cur/1234.M1" || resumed.Count != 5000 {
		t.Fatalf("resume lost state: %+v", resumed)
	}

	// Every field that changes WHICH items are processed must refuse.
	for name, job := range map[string]jobIdentity{
		"different source":     {Kind: jobA.Kind, Source: "/srv/mail/bob/Maildir", Destination: jobA.Destination, Mailbox: jobA.Mailbox, Folder: jobA.Folder},
		"different database":   {Kind: jobA.Kind, Source: jobA.Source, Destination: "other.invalid/mail", Mailbox: jobA.Mailbox, Folder: jobA.Folder},
		"different mailbox":    {Kind: jobA.Kind, Source: jobA.Source, Destination: jobA.Destination, Mailbox: "bob", Folder: jobA.Folder},
		"different folder":     {Kind: jobA.Kind, Source: jobA.Source, Destination: jobA.Destination, Mailbox: jobA.Mailbox, Folder: "Archive"},
		"different importer":   {Kind: "blob-import", Source: jobA.Source, Destination: jobA.Destination, Mailbox: jobA.Mailbox, Folder: jobA.Folder},
		"an additional filter": {Kind: jobA.Kind, Source: jobA.Source, Destination: jobA.Destination, Mailbox: jobA.Mailbox, Folder: jobA.Folder, Filters: []string{"mailbox=alice"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadCheckpoint(path, true, job, false); err == nil {
				t.Fatal("resuming a different job was allowed; it would skip unprocessed items")
			} else if !strings.Contains(err.Error(), "different job") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestLegacyCheckpointNeedsExplicitConsent pins the migration path: a marker
// written before identity existed cannot be attributed, so resuming from it is
// a guess the operator has to make explicitly.
func TestLegacyCheckpointNeedsExplicitConsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.ckpt")
	legacy := map[string]any{"last_key": "cur/999.M1", "count": 10, "started_at": time.Now()}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	job := jobIdentity{Kind: "maildir-import", Source: "/srv/m", Destination: "d/x", Mailbox: "a", Folder: "INBOX"}
	if _, err := loadCheckpoint(path, true, job, false); err == nil {
		t.Fatal("a legacy checkpoint was resumed without consent")
	}
	resumed, err := loadCheckpoint(path, true, job, true)
	if err != nil {
		t.Fatalf("legacy checkpoint with consent: %v", err)
	}
	if resumed.resumeKey() != "cur/999.M1" {
		t.Fatalf("legacy state lost: %+v", resumed)
	}
}

// TestCheckpointFileHoldsNoCredential pins that the identity written to disk
// carries no DSN password.
func TestCheckpointFileHoldsNoCredential(t *testing.T) {
	for _, dsn := range []string{
		"postgres://app:sup3rs3cret@db.invalid:5432/mail?sslmode=verify-full",
		"host=db.invalid user=app password='sup3rs3cret' dbname=mail",
	} {
		got := redactedDSN(dsn)
		if strings.Contains(got, "sup3rs3cret") {
			t.Fatalf("redactedDSN(%q) = %q; it leaks the password", dsn, got)
		}
		if !strings.Contains(got, "db.invalid") || !strings.Contains(got, "mail") {
			t.Errorf("redactedDSN(%q) = %q; it should still identify the deployment", dsn, got)
		}
	}
}

// TestPartialFailureIsReportedAndRetryable is the RA6X-032 regression.
//
// Import loops incremented a counter, advanced the durable checkpoint past the
// failed file, and returned EX_OK on a completed walk — so a later resume never
// retried those files even after the parser was fixed, and automation could not
// tell a complete recovery from a partial one by exit status.
func TestPartialFailureIsReportedAndRetryable(t *testing.T) {
	dir := t.TempDir()
	ckpt := filepath.Join(dir, "import.ckpt")
	job := jobIdentity{Kind: "maildir-import", Source: dir, Destination: "d/x", Mailbox: "a", Folder: "INBOX"}

	journal, err := openRecovery(ckpt, job, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	for _, key := range []string{"cur/bad-1.M1", "cur/bad-2.M1"} {
		if err := journal.fail(key, "parse_failed", os.ErrInvalid); err != nil {
			t.Fatal(err)
		}
	}
	if code := recoveryOutcome(journal, true); code == EX_OK || IsPermanentFailure(code) {
		t.Fatalf("partial status %d", code)
	}
	data, err := os.ReadFile(manifestPathFor(ckpt))
	if err != nil {
		t.Fatal(err)
	}
	var report struct{ Items []failedItem }
	if err := json.Unmarshal(data, &report); err != nil || len(report.Items) != 2 {
		t.Fatalf("report %s: %v", data, err)
	}
	for _, item := range report.Items {
		if err := journal.resolved(item.Key); err != nil {
			t.Fatal(err)
		}
	}
	if code := recoveryOutcome(journal, true); code != EX_OK {
		t.Fatal(code)
	}
	if _, err := os.Stat(manifestPathFor(ckpt)); !os.IsNotExist(err) {
		t.Fatal("stale failure report remains")
	}
	if code := recoveryOutcome(journal, false); code == EX_OK {
		t.Fatal("checkpoint failure reported success")
	}

}

// TestReadBoundedRefusesOversizedAndSpecialFiles is the RA6X-034 regression.
//
// Every recovery entry point called os.ReadFile, allocating the whole file
// before the parser could enforce MaxMessageBytes — so a corrupt or growing
// source could exhaust memory during the very pass meant to recover from a
// problem, and a FIFO under the source tree blocked it indefinitely.
func TestReadBoundedRefusesOversizedAndSpecialFiles(t *testing.T) {
	dir := t.TempDir()

	// Exactly at the limit is accepted; one byte over is not.
	atLimit := filepath.Join(dir, "at-limit")
	if err := os.WriteFile(atLimit, make([]byte, 100), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if data, err := readBounded(atLimit, 100); err != nil {
		t.Errorf("a file exactly at the limit was refused: %v", err)
	} else if len(data) != 100 {
		t.Errorf("read %d bytes, want 100", len(data))
	}

	over := filepath.Join(dir, "over-limit")
	if err := os.WriteFile(over, make([]byte, 101), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := readBounded(over, 100); err == nil {
		t.Error("an oversized file was read")
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("unexpected error: %v", err)
	}

	// A sparse file far larger than the limit must be refused without being
	// allocated.
	sparse := filepath.Join(dir, "sparse")
	f, err := os.Create(sparse)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.Seek(1<<30, 0); err != nil {
		t.Fatalf("seek: %v", err)
	}
	if _, err := f.Write([]byte{0}); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()
	if _, err := readBounded(sparse, 1024); err == nil {
		t.Error("a 1 GiB sparse file was read under a 1 KiB limit")
	}

	// A FIFO would block forever; it must be refused as not a regular file.
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readBounded(fifo, 1024)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO was accepted as a message source")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readBounded blocked on a FIFO")
	}
}

// TestRejectImportRefusesControlCharacters is the RA6X-061 regression.
//
// A quoted CSV field may contain an embedded newline, and TrimSpace removes
// only outer whitespace: regexp.QuoteMeta plus slash escaping leave an interior
// newline intact, so a pattern containing one SPLIT the generated regexp across
// two Postfix policy lines — the first matching something else, the second a
// syntax error Postfix logs and ignores. A rule the legacy system enforced
// silently stops being enforced.
func TestRejectImportRefusesControlCharacters(t *testing.T) {
	for name, csvText := range map[string]string{
		"newline in the pattern": "domain,scope,reject_string\nexample.invalid,sender,\"first\nsecond\"\n",
		"CR in the pattern":      "domain,scope,reject_string\nexample.invalid,sender,\"first\rsecond\"\n",
		"tab in the pattern":     "domain,scope,reject_string\nexample.invalid,sender,\"first\tsecond\"\n",
		"newline in the domain":  "domain,scope,reject_string\n\"ex\nample.invalid\",sender,spam\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseRejectCSV(csv.NewReader(strings.NewReader(csvText)))
			if err == nil {
				t.Fatal("a control-bearing field was accepted into policy generation")
			}
			if !strings.Contains(err.Error(), "control character") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}

	// A normal rule still parses, including regexp metacharacters and slashes,
	// which must be escaped rather than refused.
	ok := "domain,scope,reject_string\nexample.invalid,both,\"a/b.c*d\"\n"
	rules, err := parseRejectCSV(csv.NewReader(strings.NewReader(ok)))
	if err != nil {
		t.Fatalf("a valid rule was refused: %v", err)
	}
	if len(rules) != 1 || rules[0].Substring != "a/b.c*d" {
		t.Fatalf("parsed %+v", rules)
	}
}

// TestCommentSanitizesEveryField pins that the provenance comment cannot carry
// an injected line from any CSV column, not just the description.
func TestCommentSanitizesEveryField(t *testing.T) {
	c := commentFor(rejectRule{
		Domain:      "ex\nample.invalid",
		Scope:       "sender\nX",
		Description: "note\nline",
	})
	if strings.ContainsAny(c, "\r\n") {
		t.Fatalf("comment spans multiple lines: %q", c)
	}
}
