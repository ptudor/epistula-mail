package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/recipients"
)

// writeAdminConfig writes a minimal valid config pointing at the test DB.
// Only postgres.dsn and storage.root are required; everything else defaults.
func writeAdminConfig(t *testing.T, dsn string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "epistula-database.toml")
	body := "production = false\n\n[postgres]\ndsn = \"" + dsn + "\"\n\n[storage]\nroot = \"" + t.TempDir() + "\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestAdminAliasAddNormalizesLocalpart is the R-006 regression: alias-add must
// store the localpart lower+NFC-normalized so the delivery resolver matches it,
// and alias-delete (which lowercases) can then remove it.
func TestAdminAliasAddNormalizesLocalpart(t *testing.T) {
	db, dsn := pgtest.Open(t)
	pool := db.Pool()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var domainID, mailboxID int64
	if err := pool.QueryRow(ctx, `INSERT INTO domains (name) VALUES ('example.invalid') RETURNING id`).Scan(&domainID); err != nil {
		t.Fatalf("insert domain: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO mailboxes (name, password_hash) VALUES ('jdoe', 'x') RETURNING id`).Scan(&mailboxID); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}

	cfgPath := writeAdminConfig(t, dsn)
	// Operator types mixed-case "JDoe".
	if code := adminAliasAdd([]string{"-config", cfgPath, "-domain", "example.invalid", "-localpart", "JDoe", "-mailbox", "jdoe"}); code != EX_OK {
		t.Fatalf("adminAliasAdd exit = %d, want EX_OK", code)
	}

	// Stored localpart must be normalized to "jdoe".
	var stored string
	if err := pool.QueryRow(ctx, `SELECT localpart FROM aliases WHERE domain_id=$1`, domainID).Scan(&stored); err != nil {
		t.Fatalf("read alias: %v", err)
	}
	if stored != "jdoe" {
		t.Fatalf("stored localpart = %q, want %q (normalized)", stored, "jdoe")
	}

	// The delivery resolver must now match jdoe@example.invalid to the mailbox.
	match, err := recipients.New(pool).Resolve(ctx, "jdoe@example.invalid")
	if err != nil {
		t.Fatalf("resolve jdoe@example.invalid: %v", err)
	}
	if match.MailboxName != "jdoe" {
		t.Fatalf("resolved mailbox = %q, want jdoe", match.MailboxName)
	}

	// alias-delete (lowercases) with a different case must remove the row.
	if code := adminAliasDelete([]string{"-config", cfgPath, "-domain", "example.invalid", "-localpart", "JDOE"}); code != EX_OK {
		t.Fatalf("adminAliasDelete exit = %d, want EX_OK", code)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM aliases WHERE domain_id=$1`, domainID).Scan(&count); err != nil {
		t.Fatalf("count aliases: %v", err)
	}
	if count != 0 {
		t.Fatalf("alias not deleted after case-variant alias-delete; count = %d", count)
	}
}

// TestAdminAliasDeleteAcceptsNFDInput: alias-add NFC-normalizes, so
// alias-delete must too — otherwise the exact input bytes that created a
// decomposed non-ASCII alias could no longer delete it (R-006 verification
// follow-up).
func TestAdminAliasDeleteAcceptsNFDInput(t *testing.T) {
	db, dsn := pgtest.Open(t)
	pool := db.Pool()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var domainID int64
	if err := pool.QueryRow(ctx, `INSERT INTO domains (name) VALUES ('example.invalid') RETURNING id`).Scan(&domainID); err != nil {
		t.Fatalf("insert domain: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO mailboxes (name, password_hash) VALUES ('jdoe', 'x')`); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}

	cfgPath := writeAdminConfig(t, dsn)
	nfd := "cafe\u0301" // "cafe" + U+0301 COMBINING ACUTE ACCENT (decomposed)
	nfc := "caf\u00e9"  // precomposed NFC — what alias-add stores

	if code := adminAliasAdd([]string{"-config", cfgPath, "-domain", "example.invalid", "-localpart", nfd, "-mailbox", "jdoe"}); code != EX_OK {
		t.Fatalf("adminAliasAdd exit = %d, want EX_OK", code)
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT localpart FROM aliases WHERE domain_id=$1`, domainID).Scan(&stored); err != nil {
		t.Fatalf("read alias: %v", err)
	}
	if stored != nfc {
		t.Fatalf("stored localpart = %q (% x), want NFC %q", stored, []byte(stored), nfc)
	}

	// Deleting with the same NFD bytes the operator typed at add time must work.
	if code := adminAliasDelete([]string{"-config", cfgPath, "-domain", "example.invalid", "-localpart", nfd}); code != EX_OK {
		t.Fatalf("adminAliasDelete exit = %d, want EX_OK (NFD input must normalize)", code)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM aliases WHERE domain_id=$1`, domainID).Scan(&count); err != nil {
		t.Fatalf("count aliases: %v", err)
	}
	if count != 0 {
		t.Fatalf("alias not deleted via NFD input; count = %d", count)
	}
}

// TestNormalizeACLArgsNFC confirms normalizeACLArgs NFC-normalizes the
// localpart so a decomposed non-ASCII ACL localpart matches its NFC envelope
// form at delivery (R-006).
func TestNormalizeACLArgsNFC(t *testing.T) {
	nfd := "cafe\u0301" // "cafe" + U+0301 COMBINING ACUTE ACCENT (decomposed)
	nfc := "caf\u00e9"  // "café" precomposed (NFC)
	if nfd == nfc {
		t.Fatal("test setup error: NFD and NFC inputs are byte-identical")
	}
	d, lp, code := normalizeACLArgs("Example.NET", nfd)
	if code != EX_OK {
		t.Fatalf("normalizeACLArgs code = %d", code)
	}
	if d != "example.net" {
		t.Errorf("domain = %q, want example.net", d)
	}
	if lp != nfc {
		t.Errorf("localpart = %q (% x), want NFC %q (% x)", lp, []byte(lp), nfc, []byte(nfc))
	}
}
