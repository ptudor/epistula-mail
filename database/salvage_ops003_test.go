package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/ingest"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// OPS-003 fixtures: synthetic messages the parser used to refuse. pad makes
// each one longer than the 256-byte limit the first import pass runs under.
var (
	pad = "X-Pad: " + strings.Repeat("p", 300) + "\r\n"

	truncatedFixture = "From: a@ops003.invalid\r\nSubject: truncated\r\n" + pad +
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nfirst part\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nsecond part, cut off"
	headerOnlyFixture = "From: a@ops003.invalid\nSubject: header only\n" + strings.ReplaceAll(pad, "\r\n", "\n")
	footerFixture     = "From: a@ops003.invalid\r\nSubject: footer\r\n" + pad +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString([]byte("list message text")) + "\r\n-- \r\nList footer\r\n"
	// No colon anywhere: net/mail reads a line with one as a header field.
	garbageFixture = "not a message, no header field and no empty line, only text " + strings.Repeat("g", 300)
)

// TestImportRetryFailuresSalvagesOnce walks the operator's retry procedure for
// OPS-003. A first pass records items it cannot import in the failure
// manifest, as the pre-OPS-003 parser did for malformed archive messages. A
// -retry-failures pass then imports exactly those items once each, clears
// their records, and leaves only what still cannot be read. Repeating the
// retry or the whole import adds nothing.
func TestImportRetryFailuresSalvagesOnce(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	gcMustMailbox(t, ctx, db, "archive")

	root, storageRoot := t.TempDir(), t.TempDir()
	writeMaildirMessage(t, root, "0001", dryRunMsg("clean"))
	writeMaildirMessage(t, root, "0002", truncatedFixture)
	writeMaildirMessage(t, root, "0003", headerOnlyFixture)
	writeMaildirMessage(t, root, "0004", footerFixture)
	writeMaildirMessage(t, root, "0005", garbageFixture)

	// Pass 1 cannot read anything over 256 bytes: every fixture but the clean
	// message lands in the manifest, standing in for the parse failures.
	strict := writeRenameConfig(t, dsn, storageRoot)
	f, err := os.OpenFile(strict, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n[limits]\nmax_message_bytes=256\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	ckpt := filepath.Join(root, "archive.ckpt")
	args := func(cfg string, extra ...string) []string {
		return append([]string{"-config", cfg, "-maildir", root, "-mailbox", "archive", "-checkpoint-file", ckpt}, extra...)
	}
	count := func() int {
		var n int
		if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM messages`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if code := runImport(args(strict)); code != EX_TEMPFAIL || count() != 1 {
		t.Fatalf("pass 1: exit %d with %d message(s), want EX_TEMPFAIL and 1", code, count())
	}

	// The retry, with the default limits. A dry run first previews it and
	// writes nothing: no row, and every manifest record kept.
	cfg := writeRenameConfig(t, dsn, storageRoot)
	job := jobIdentity{Kind: "maildir-import", Source: canonicalSource(root), Destination: redactedDSN(dsn), Mailbox: "archive", Folder: "INBOX"}
	if err := bindJob(ctx, db, &job); err != nil {
		t.Fatal(err)
	}
	manifestCount := func() int64 {
		j, err := openRecovery(ckpt, job, true, true)
		if err != nil {
			t.Fatal(err)
		}
		defer j.Close()
		return j.count
	}
	if code := runImport(args(cfg, "-retry-failures", "-dry-run")); code != EX_TEMPFAIL || count() != 1 || manifestCount() != 4 {
		t.Fatalf("dry-run retry: exit %d, %d message(s), %d record(s); want EX_TEMPFAIL, 1, 4", code, count(), manifestCount())
	}
	if code := runImport(args(cfg, "-retry-failures")); code != EX_TEMPFAIL {
		t.Fatalf("retry: exit %d, want EX_TEMPFAIL for the one unreadable item", code)
	}
	if n := count(); n != 4 {
		t.Fatalf("after the retry: %d messages, want 4", n)
	}
	outcomes := map[string]int{}
	rows, err := db.Pool().Query(ctx, `SELECT outcome, coalesce(error_detail, '') FROM delivery_log`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var outcome, detail string
		if err := rows.Scan(&outcome, &detail); err != nil {
			t.Fatal(err)
		}
		outcomes[outcome]++
		if (outcome == "imported:degraded") != (detail != "") {
			t.Errorf("outcome %q with error_detail %q", outcome, detail)
		}
	}
	rows.Close()
	if outcomes["imported"] != 2 || outcomes["imported:degraded"] != 2 || len(outcomes) != 2 {
		t.Errorf("delivery_log outcomes = %v, want 2 imported (clean, header-only) and 2 imported:degraded", outcomes)
	}
	var marked int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM messages WHERE bodystructure @? '$.**.defects'`).Scan(&marked); err != nil {
		t.Fatal(err)
	}
	if marked != 2 {
		t.Errorf("%d messages carry defects in their bodystructure, want 2", marked)
	}
	j, err := openRecovery(ckpt, job, true, true)
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := j.contains("cur/0005")
	if err != nil || !remaining || j.count != 1 {
		t.Fatalf("manifest holds %d item(s) (0005 present: %v, %v), want exactly the unreadable one", j.count, remaining, err)
	}
	j.Close()

	// Exactly once: neither another retry nor a whole re-import adds a row.
	if code := runImport(args(cfg, "-retry-failures")); code != EX_TEMPFAIL || count() != 4 {
		t.Fatalf("second retry: exit %d with %d messages", code, count())
	}
	if code := runImport(args(cfg)); code != EX_TEMPFAIL || count() != 4 {
		t.Fatalf("re-import: exit %d with %d messages", code, count())
	}

	// Salvaging never touches the raw bytes: every stored blob matches its
	// source file and its content address.
	if err := os.Remove(filepath.Join(root, "cur", "0005")); err != nil {
		t.Fatal(err)
	}
	if code := runImportVerify([]string{"-config", cfg, "-maildir", root, "-mailbox", "archive"}); code != EX_OK {
		t.Fatalf("import-verify exit %d, want EX_OK", code)
	}

	// The reparse back-fill derives exactly what the import stored, so it
	// does not rewrite salvaged rows.
	store := blob.NewStore(storageRoot)
	parser := ingest.NewSalvaging(ingest.DefaultLimits())
	st, err := reparseAll(ctx, db, store, parser, reparseParams{BatchSize: 10, MaxMessageBytes: ingest.DefaultLimits().MaxMessageBytes})
	if err != nil {
		t.Fatal(err)
	}
	if st.Scanned != 4 || st.Unchanged != 4 {
		t.Errorf("reparse = %+v, want all 4 rows unchanged", st)
	}
}

// TestDeliverSalvagesButKeepsLimits covers live delivery. A malformation no
// longer bounces a message a mail client would show; it is delivered, and
// logged as degraded. The resource limits still bounce, exactly as before
// OPS-003.
func TestDeliverSalvagesButKeepsLimits(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	mailboxID := gcMustMailbox(t, ctx, db, "ops003")
	var domainID int64
	if err := db.Pool().QueryRow(ctx, `INSERT INTO domains (name) VALUES ('ops003.invalid') RETURNING id`).Scan(&domainID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `INSERT INTO aliases (domain_id, localpart, mailbox_id) VALUES ($1, 'user', $2)`, domainID, mailboxID); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Postgres.DSN = dsn
	cfg.Storage.Root = t.TempDir()
	cfg.Delivery.Timeout = "60s"

	deep := "From: a@ops003.invalid\r\nSubject: deep\r\n"
	for i := 0; i < 12; i++ {
		deep += fmt.Sprintf("Content-Type: multipart/mixed; boundary=B%d\r\n\r\n--B%d\r\n", i, i)
	}
	deep += "Content-Type: text/plain\r\n\r\ntoo deep\r\n"

	cases := []struct {
		name, raw, outcome string
		code               int
	}{
		{"truncated multipart", truncatedFixture, "delivered:degraded", EX_OK},
		{"base64 with a footer", footerFixture, "delivered:degraded", EX_OK},
		{"header only", headerOnlyFixture, "delivered", EX_OK},
		{"too deep", deep, "", EX_DATAERR},
		{"header field too large", "From: a@ops003.invalid\r\nX-Big: " + strings.Repeat("b", 17000) + "\r\n\r\nbody\r\n", "", EX_DATAERR},
		{"no header field", garbageFixture, "", EX_DATAERR},
	}
	for _, tc := range cases {
		code := deliverBytes(t, cfg, "user@ops003.invalid", "sender@ops003.invalid", tc.raw)
		if code != tc.code {
			t.Errorf("%s: exit %d (%s), want %d (%s)", tc.name, code, ExitCodeName(code), tc.code, ExitCodeName(tc.code))
			continue
		}
		if tc.outcome == "" {
			continue
		}
		var outcome, detail string
		if err := db.Pool().QueryRow(ctx,
			`SELECT outcome, coalesce(error_detail, '') FROM delivery_log WHERE message_id IS NOT NULL ORDER BY id DESC LIMIT 1`,
		).Scan(&outcome, &detail); err != nil {
			t.Fatal(err)
		}
		if outcome != tc.outcome || (outcome == "delivered:degraded") != (detail != "") {
			t.Errorf("%s: logged %q / %q, want %q", tc.name, outcome, detail, tc.outcome)
		}
	}
	var n int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM messages`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("%d messages stored, want the 3 delivered", n)
	}
}
