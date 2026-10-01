package main

import (
	"context"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestAdminSubcommandsAcceptCaseVariantName is the R-048 regression: mailbox
// names are stored canonical (lower + trimmed), so the name-taking subcommands
// must resolve case-variant input rather than reporting "not found".
func TestAdminSubcommandsAcceptCaseVariantName(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	gcMustMailbox(t, ctx, db, "casebox") // stored canonical "casebox"
	cfg := writeAdminConfig(t, dsn)

	// Each of these addresses the mailbox with a different casing/whitespace.
	if code := adminMailboxDisable([]string{"-config", cfg, "-name", "CaseBox"}); code != EX_OK {
		t.Fatalf("mailbox-disable -name CaseBox exit=%d, want EX_OK", code)
	}
	if code := adminMailboxEnable([]string{"-config", cfg, "-name", "CASEBOX"}); code != EX_OK {
		t.Fatalf("mailbox-enable -name CASEBOX exit=%d, want EX_OK", code)
	}
	if code := adminFolderList([]string{"-config", cfg, "-mailbox", " CaseBox "}); code != EX_OK {
		t.Fatalf("folder-list -mailbox ' CaseBox ' exit=%d, want EX_OK", code)
	}
	if code := adminMailboxDelete([]string{"-config", cfg, "-name", "CaseBox", "-yes"}); code != EX_OK {
		t.Fatalf("mailbox-delete -name CaseBox exit=%d, want EX_OK", code)
	}

	var n int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM mailboxes WHERE name = 'casebox'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("mailbox 'casebox' still present after case-variant delete (%d rows)", n)
	}
}
