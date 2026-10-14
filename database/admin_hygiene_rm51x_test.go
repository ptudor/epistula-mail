package main

import (
	"context"
	"errors"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/recipients"
	"io"
	"os"
	"strings"
	"testing"
)

func adminOutput(t *testing.T, run func() int) (int, string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	old := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = old }()
	code := run()
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return code, string(b)
}
func TestTerminalSafeAndLogTail(t *testing.T) {
	if got := terminalSafe("a\x1b\x07\x7f\u0085\t\n雪"); got != "a\\x1b\\x07\\x7f\\x85\t\n雪" {
		t.Fatal(got)
	}
	db, dsn := pgtest.Open(t)
	hostile := "\x1b]0;x\x07a@b.invalid"
	if _, err := db.Pool().Exec(context.Background(), `INSERT INTO delivery_log(envelope_from,envelope_to,bytes,outcome) VALUES($1,$1,1,'delivered')`, hostile); err != nil {
		t.Fatal(err)
	}
	code, out := adminOutput(t, func() int { return adminLogTail([]string{"-config", writeAdminConfig(t, dsn)}) })
	if code != EX_OK || strings.ContainsAny(out, "\x1b\x07") || !strings.Contains(out, `\x1b`) {
		t.Fatal(code, out)
	}
	var stored string
	if err := db.Pool().QueryRow(context.Background(), `SELECT envelope_to FROM delivery_log`).Scan(&stored); err != nil || stored != hostile {
		t.Fatal(stored, err)
	}
}
func TestAdminRejectsInvalidIdentifiers(t *testing.T) {
	db, dsn := pgtest.Open(t)
	if code := adminAliasAdd([]string{"-config", writeAdminConfig(t, dsn), "-domain", "example.invalid", "-mailbox", "alice", "-localpart", "a\x01b"}); code != EX_USAGE {
		t.Fatal(code)
	}
	var n int
	if err := db.Pool().QueryRow(context.Background(), `SELECT count(*) FROM aliases`).Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	for _, lp := range []string{"", strings.Repeat("a", 65), "a@b", "a\x7fb"} {
		if validateLocalpart(lp) == nil {
			t.Fatalf("accepted %q", lp)
		}
	}
	if code := adminMailboxAdd([]string{"-name", "alice", "-quota-bytes", "-1"}); code != EX_USAGE {
		t.Fatal(code)
	}
	if got, err := normalizeTokenName(" foo "); err != nil || got != "foo" {
		t.Fatal(got, err)
	}
	for _, s := range []string{" ", "foo\x01bar", "foo\u0085bar"} {
		if _, err := normalizeTokenName(s); err == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}
func TestDomainSetPreservesAliasesAndACLs(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx := context.Background()
	cfg := writeAdminConfig(t, dsn)
	mailbox := mustMailbox(t, ctx, db, "alice")
	var domain int64
	if err := db.Pool().QueryRow(ctx, `INSERT INTO domains(name) VALUES('example.invalid') RETURNING id`).Scan(&domain); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `INSERT INTO aliases(domain_id,localpart,mailbox_id) VALUES($1,'user',$2);`, domain, mailbox); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `INSERT INTO domain_acl(domain_id,localpart,kind) VALUES($1,'allowed','allow')`, domain); err != nil {
		t.Fatal(err)
	}
	r := recipients.New(db.Pool())
	for _, step := range []struct {
		flag string
		want error
	}{{"", recipients.ErrNoMatch}, {"-wildcard", recipients.ErrNotAllowlisted}, {"-no-wildcard", recipients.ErrNoMatch}} {
		if step.flag != "" {
			if code := adminDomainSet([]string{"-config", cfg, "-name", "example.invalid", step.flag}); code != EX_OK {
				t.Fatal(code)
			}
		}
		if _, err := r.Resolve(ctx, "other@example.invalid"); !errors.Is(err, step.want) {
			t.Fatal(step.flag, err)
		}
		if got, err := r.Resolve(ctx, "user@example.invalid"); err != nil || got.MailboxID != mailbox {
			t.Fatal(got, err)
		}
	}
	var n int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM domain_acl WHERE domain_id=$1`, domain).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
