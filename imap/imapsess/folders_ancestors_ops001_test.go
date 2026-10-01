package imapsess

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

// waitForMailboxLockWait returns once another session in this test database is
// blocked on a `... FROM mailboxes WHERE id = $1 FOR UPDATE` row lock. It fails
// if done fires first: the command under test finished without waiting for the
// mailbox lock the test is holding.
func waitForMailboxLockWait(t *testing.T, ctx context.Context, sess *Session, done <-chan error) {
	t.Helper()
	for {
		select {
		case err := <-done:
			t.Fatalf("command finished (err=%v) while the mailbox row lock was held; it must wait for it", err)
		default:
		}
		var waiting bool
		if err := sess.be.Pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			                 WHERE datname = current_database()
			                   AND pid <> pg_backend_pid()
			                   AND wait_event_type = 'Lock'
			                   AND query LIKE '%FROM mailboxes WHERE id = $1 FOR UPDATE%')`,
		).Scan(&waiting); err != nil {
			t.Fatalf("observe lock wait: %v", err)
		}
		if waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRenameCreatesDestinationAncestors is the RENAME leg of OPS-001. RFC 3501
// §6.3.5 has the server create the superior names a RENAME needs; renaming
// onto `Old/2026/Drafts` used to leave a folder whose parents LIST never
// returned, just as the writer's import path did.
func TestRenameCreatesDestinationAncestors(t *testing.T) {
	sess := mutationFixture(t)
	for _, n := range []string{"Drafts", "Drafts/2026"} {
		if err := sess.Create(n, nil); err != nil {
			t.Fatalf("Create %q: %v", n, err)
		}
	}

	if err := sess.Rename("Drafts", "Old/2026/Drafts", nil); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	got := folderNames(t, sess)
	for _, want := range []string{"Old", "Old/2026", "Old/2026/Drafts", "Old/2026/Drafts/2026"} {
		if !contains(got, want) {
			t.Errorf("%q missing after rename; got %v", want, got)
		}
	}
	for _, gone := range []string{"Drafts", "Drafts/2026"} {
		if contains(got, gone) {
			t.Errorf("%q survived the rename; got %v", gone, got)
		}
	}
}

// TestRefusedRenameCreatesNoAncestors keeps RENAME atomic now that it can
// create rows: a rename refused for a name clash leaves the folder set exactly
// as it was, even when the clashing destination is an orphan left by a writer
// that predates OPS-001 (so its ancestors really are missing).
func TestRefusedRenameCreatesNoAncestors(t *testing.T) {
	sess := mutationFixture(t)
	if err := sess.Create("Drafts", nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := sess.be.Pool.Exec(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
		 VALUES ($1, 'Legacy/Sent', mail_next_uidvalidity(), 1)`, sess.mailboxID,
	); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}
	before := folderNames(t, sess)

	err := sess.Rename("Drafts", "Legacy/Sent", nil)
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) || imapErr.Code != imap.ResponseCodeAlreadyExists {
		t.Fatalf("Rename onto an existing name = %v, want ALREADYEXISTS", err)
	}
	after := folderNames(t, sess)
	if len(after) != len(before) {
		t.Fatalf("refused rename changed the folder set: %v -> %v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("refused rename changed the folder set: %v -> %v", before, after)
		}
	}
}

// TestCopyAndMoveAutoCreateMakeAncestors extends TestCopyAutoCreatesAncestors
// (RA6X-009) now that the destination comes from the shared
// storage.EnsureFolder (OPS-001): MOVE gets the same ancestors, and COPYUID
// reports the UIDVALIDITY of the folder the command just created.
func TestCopyAndMoveAutoCreateMakeAncestors(t *testing.T) {
	sess := mutationFixture(t)

	data, err := sess.Copy(imap.UIDSetNum(1), "Archive/2026/Q1")
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if err := sess.Move(nil, imap.UIDSetNum(2), "Old/Moved"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	got := folderNames(t, sess)
	for _, want := range []string{"Archive", "Archive/2026", "Archive/2026/Q1", "Old", "Old/Moved"} {
		if !contains(got, want) {
			t.Errorf("%q missing; got %v", want, got)
		}
	}
	if v := folderUIDValidity(t, sess, "Archive/2026/Q1"); int64(data.UIDValidity) != v {
		t.Errorf("COPYUID reported UIDVALIDITY %d, folder has %d", data.UIDValidity, v)
	}
}

// TestCreateSerializesWithDeleteOnTheMailboxLock is the concurrency half of
// OPS-001. DELETE holds the mailbox row while it checks for inferiors. A CREATE
// that did not take the same lock could find `Projects`, lose it to a DELETE
// that saw no children yet, and commit `Projects/2026` with no parent.
func TestCreateSerializesWithDeleteOnTheMailboxLock(t *testing.T) {
	sess := mutationFixture(t)
	if err := sess.Create("Projects", nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Stand in for `DELETE Projects`: the mailbox lock first, then the row.
	tx, err := sess.be.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(context.Background())
	if err := lockMailbox(ctx, tx, sess.mailboxID); err != nil {
		t.Fatalf("lock mailbox: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM folders WHERE mailbox_id = $1 AND name = 'Projects'`, sess.mailboxID,
	); err != nil {
		t.Fatalf("delete: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- sess.Create("Projects/2026", nil) }()
	waitForMailboxLockWait(t, ctx, sess, done)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("CREATE after the DELETE: %v", err)
	}
	got := folderNames(t, sess)
	for _, want := range []string{"Projects", "Projects/2026"} {
		if !contains(got, want) {
			t.Errorf("%q missing — CREATE acted on a parent the DELETE removed; got %v", want, got)
		}
	}
}

// TestRenameSerializesWithDeleteOnTheMailboxLock is the same race for RENAME,
// which now also creates the destination's ancestors.
func TestRenameSerializesWithDeleteOnTheMailboxLock(t *testing.T) {
	sess := mutationFixture(t)
	for _, n := range []string{"Drafts", "Old"} {
		if err := sess.Create(n, nil); err != nil {
			t.Fatalf("Create %q: %v", n, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tx, err := sess.be.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(context.Background())
	if err := lockMailbox(ctx, tx, sess.mailboxID); err != nil {
		t.Fatalf("lock mailbox: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM folders WHERE mailbox_id = $1 AND name = 'Old'`, sess.mailboxID,
	); err != nil {
		t.Fatalf("delete: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- sess.Rename("Drafts", "Old/Drafts", nil) }()
	waitForMailboxLockWait(t, ctx, sess, done)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("RENAME after the DELETE: %v", err)
	}
	got := folderNames(t, sess)
	for _, want := range []string{"Old", "Old/Drafts"} {
		if !contains(got, want) {
			t.Errorf("%q missing; got %v", want, got)
		}
	}
	if contains(got, "Drafts") {
		t.Errorf("Drafts survived the rename; got %v", got)
	}
}
