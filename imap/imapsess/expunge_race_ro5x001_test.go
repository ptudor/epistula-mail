package imapsess

import (
	"context"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

// TestExpungeSkipsConcurrentlyUndeletedMessage is the RO5X-001 regression.
//
// Expunge reads its candidates at READ COMMITTED with no row locks, so a
// concurrent session can clear \Deleted between that read and the DELETE.
// Before the fix the DELETE was keyed on (folder_id, uid) alone and
// destroyed the row anyway — silent, permanent mail loss. The DELETE now
// repeats the \Deleted predicate, so the row survives.
//
// The race is driven deterministically via expungeRaceHook, which fires in
// exactly that window.
func TestExpungeSkipsConcurrentlyUndeletedMessage(t *testing.T) {
	sess := mutationFixture(t)
	startBytes := usedBytes(t, sess)

	// mutationFixture flags uid 3 \Deleted. Mark uid 1 too, so the expunge
	// has one candidate that survives (uid 3) and one that gets rescued
	// (uid 1) — proving the DELETE filters per row, not all-or-nothing.
	setFlags(t, sess, 1, []string{`\Deleted`})

	// In the window between the candidate SELECT and the DELETE, a
	// different session clears \Deleted on uid 1 and commits.
	fired := false
	sess.expungeRaceHook = func() {
		fired = true
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := sess.be.Pool.Exec(ctx,
			`UPDATE messages SET flags = '{}'::text[] WHERE folder_id = $1 AND uid = 1`,
			sess.selectedFolderID,
		); err != nil {
			t.Errorf("concurrent un-delete: %v", err)
		}
	}

	if err := sess.Expunge(nil, nil); err != nil {
		t.Fatalf("Expunge: %v", err)
	}
	if !fired {
		t.Fatal("race hook never fired — the test did not exercise the window")
	}

	if !messageExists(t, sess, sess.selectedFolderID, 1) {
		t.Error("uid 1 was expunged despite \\Deleted being cleared concurrently (RO5X-001)")
	}
	if messageExists(t, sess, sess.selectedFolderID, 3) {
		t.Error("uid 3 should still be expunged — it kept its \\Deleted flag")
	}

	// Only uid 3's 100 bytes come back; the rescued uid 1 must not be
	// deducted from the mailbox.
	if want, got := startBytes-100, usedBytes(t, sess); got != want {
		t.Errorf("used_bytes = %d, want %d (only the truly-deleted message)", got, want)
	}
}

// TestExpungeAllCandidatesRescued covers the boundary where the concurrent
// un-delete rescues *every* candidate: nothing is deleted, so used_bytes and
// highest_modseq must not move and the commit must still succeed.
func TestExpungeAllCandidatesRescued(t *testing.T) {
	sess := mutationFixture(t)
	startBytes := usedBytes(t, sess)
	startModseq := folderModseq(t, sess)

	sess.expungeRaceHook = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := sess.be.Pool.Exec(ctx,
			`UPDATE messages SET flags = '{}'::text[] WHERE folder_id = $1`,
			sess.selectedFolderID,
		); err != nil {
			t.Errorf("concurrent un-delete: %v", err)
		}
	}

	if err := sess.Expunge(nil, nil); err != nil {
		t.Fatalf("Expunge: %v", err)
	}
	for _, uid := range []int64{1, 2, 3} {
		if !messageExists(t, sess, sess.selectedFolderID, uid) {
			t.Errorf("uid %d expunged despite \\Deleted being cleared", uid)
		}
	}
	if got := usedBytes(t, sess); got != startBytes {
		t.Errorf("used_bytes = %d, want %d (nothing was deleted)", got, startBytes)
	}
	if got := folderModseq(t, sess); got != startModseq {
		t.Errorf("highest_modseq = %d, want %d (nothing was deleted)", got, startModseq)
	}
}

// TestExpungeNoDeletedFlagDeletesNothing is the non-racy regression: a folder
// where no message carries \Deleted must lose nothing.
func TestExpungeNoDeletedFlagDeletesNothing(t *testing.T) {
	sess := mutationFixture(t)
	setFlags(t, sess, 3, []string{}) // clear the fixture's only \Deleted
	startBytes := usedBytes(t, sess)

	if err := sess.Expunge(nil, nil); err != nil {
		t.Fatalf("Expunge: %v", err)
	}
	for _, uid := range []int64{1, 2, 3} {
		if !messageExists(t, sess, sess.selectedFolderID, uid) {
			t.Errorf("uid %d expunged from a folder with no \\Deleted messages", uid)
		}
	}
	if got := usedBytes(t, sess); got != startBytes {
		t.Errorf("used_bytes = %d, want %d", got, startBytes)
	}
}

// TestExpungeSeenOnlyDeletesNothing covers the case where every message's
// flags have been changed to \Seen only: EXPUNGE must remove nothing.
func TestExpungeSeenOnlyDeletesNothing(t *testing.T) {
	sess := mutationFixture(t)
	for _, uid := range []int64{1, 2, 3} {
		setFlags(t, sess, uid, []string{`\Seen`})
	}
	startBytes := usedBytes(t, sess)

	if err := sess.Expunge(nil, nil); err != nil {
		t.Fatalf("Expunge: %v", err)
	}
	for _, uid := range []int64{1, 2, 3} {
		if !messageExists(t, sess, sess.selectedFolderID, uid) {
			t.Errorf("uid %d expunged though it carries only \\Seen", uid)
		}
	}
	if got := usedBytes(t, sess); got != startBytes {
		t.Errorf("used_bytes = %d, want %d", got, startBytes)
	}
}

// TestUIDExpungeStillHonoursSubsetAfterRecheck proves the UID EXPUNGE subset
// filter survives the fix: a \Deleted message outside the requested UID set
// must not be removed even though the DELETE's flag predicate would match it.
func TestUIDExpungeStillHonoursSubsetAfterRecheck(t *testing.T) {
	sess := mutationFixture(t)
	setFlags(t, sess, 1, []string{`\Deleted`}) // uid 1 and uid 3 now \Deleted

	uids := imap.UIDSetNum(3) // ask for uid 3 only
	if err := sess.Expunge(nil, &uids); err != nil {
		t.Fatalf("Expunge: %v", err)
	}
	if messageExists(t, sess, sess.selectedFolderID, 3) {
		t.Error("uid 3 was in the UID set and \\Deleted; it should be gone")
	}
	if !messageExists(t, sess, sess.selectedFolderID, 1) {
		t.Error("uid 1 was outside the requested UID set and must survive")
	}
}

// setFlags overwrites a message's flag array directly, standing in for a
// concurrent session's STORE.
func setFlags(t *testing.T, sess *Session, uid int64, flags []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := sess.be.Pool.Exec(ctx,
		`UPDATE messages SET flags = $1 WHERE folder_id = $2 AND uid = $3`,
		flags, sess.selectedFolderID, uid,
	); err != nil {
		t.Fatalf("setFlags uid %d: %v", uid, err)
	}
}

// folderModseq reads the selected folder's highest_modseq.
func folderModseq(t *testing.T, sess *Session) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var ms int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT highest_modseq FROM folders WHERE id = $1`, sess.selectedFolderID,
	).Scan(&ms); err != nil {
		t.Fatalf("folderModseq: %v", err)
	}
	return ms
}
