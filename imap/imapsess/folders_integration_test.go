package imapsess

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

func TestCreateDeleteRenameFolder(t *testing.T) {
	sess, _ := appendFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// CREATE.
	if err := sess.Create("Projects/2026", nil); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	folderID, err := sess.lookupFolder(ctx, "Projects/2026")
	if err != nil {
		t.Fatalf("lookup created folder: %v", err)
	}

	// CREATE again → NO [ALREADYEXISTS].
	err = sess.Create("Projects/2026", nil)
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) || imapErr.Code != imap.ResponseCodeAlreadyExists {
		t.Fatalf("duplicate CREATE = %v, want NO [ALREADYEXISTS]", err)
	}

	// Populate via APPEND so DELETE has quota to reclaim.
	if _, err := sess.Append("Projects/2026", newLiteral([]byte(appendRawMsg)), &imap.AppendOptions{}); err != nil {
		t.Fatalf("APPEND into created folder: %v", err)
	}
	var usedBefore int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT used_bytes FROM mailboxes WHERE id = $1`, sess.mailboxID,
	).Scan(&usedBefore); err != nil {
		t.Fatalf("used_bytes: %v", err)
	}
	if usedBefore == 0 {
		t.Fatal("APPEND did not account quota")
	}

	// RENAME bumps uidvalidity.
	var validityBefore int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT uidvalidity FROM folders WHERE id = $1`, folderID,
	).Scan(&validityBefore); err != nil {
		t.Fatalf("uidvalidity: %v", err)
	}
	time.Sleep(1100 * time.Millisecond) // uidvalidity is unix-seconds; ensure a bump
	if err := sess.Rename("Projects/2026", "Projects/Archive", nil); err != nil {
		t.Fatalf("RENAME: %v", err)
	}
	var name string
	var validityAfter int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT name, uidvalidity FROM folders WHERE id = $1`, folderID,
	).Scan(&name, &validityAfter); err != nil {
		t.Fatalf("post-rename select: %v", err)
	}
	if name != "Projects/Archive" {
		t.Errorf("renamed folder name = %q", name)
	}
	if validityAfter <= validityBefore {
		t.Errorf("uidvalidity %d -> %d, want a bump on RENAME", validityBefore, validityAfter)
	}

	// RENAME onto an existing name → NO [ALREADYEXISTS].
	if err := sess.Create("Other", nil); err != nil {
		t.Fatalf("CREATE Other: %v", err)
	}
	err = sess.Rename("Projects/Archive", "Other", nil)
	if !errors.As(err, &imapErr) || imapErr.Code != imap.ResponseCodeAlreadyExists {
		t.Fatalf("RENAME onto existing = %v, want NO [ALREADYEXISTS]", err)
	}

	// DELETE reclaims quota and removes rows.
	if err := sess.Delete("Projects/Archive"); err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	var usedAfter int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT used_bytes FROM mailboxes WHERE id = $1`, sess.mailboxID,
	).Scan(&usedAfter); err != nil {
		t.Fatalf("used_bytes after delete: %v", err)
	}
	if usedAfter != 0 {
		t.Errorf("used_bytes after DELETE = %d, want 0 (quota reclaimed)", usedAfter)
	}
	if _, err := sess.lookupFolder(ctx, "Projects/Archive"); err == nil {
		t.Error("folder still present after DELETE")
	}
}

func TestInboxIsProtected(t *testing.T) {
	sess, _ := appendFixture(t)

	if err := sess.Delete("INBOX"); err == nil {
		t.Error("DELETE INBOX succeeded, want refusal")
	}
	if err := sess.Rename("INBOX", "OldInbox", nil); err == nil {
		t.Error("RENAME INBOX succeeded, want refusal")
	}
}
