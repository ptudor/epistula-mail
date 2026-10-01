package imapsess

import (
	"context"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

// TestSequenceNumbersFollowTheClientsView is the RA6X-001 regression.
//
// Client A sees UIDs [1,2,3]. Client B expunges UID 1. A then issues a
// sequence-number STORE for "2". Before the fix, every command re-derived
// sequence numbers from a fresh row_number() over the current database, so "2"
// resolved to UID 3 — while A still believed 2 was UID 2. A following EXPUNGE
// would make the wrong message's deletion permanent.
func TestSequenceNumbersFollowTheClientsView(t *testing.T) {
	a := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// A establishes its view over all three messages.
	if _, err := a.Select("INBOX", nil); err != nil {
		t.Fatalf("A SELECT: %v", err)
	}
	if got := a.view.len(); got != 3 {
		t.Fatalf("A's view holds %d messages, want 3", got)
	}

	// B removes UID 1 out from under A.
	if _, err := a.be.Pool.Exec(ctx,
		`DELETE FROM messages WHERE folder_id = $1 AND uid = 1`, a.selectedFolderID); err != nil {
		t.Fatalf("B delete: %v", err)
	}

	// A's "2" must still be UID 2 — A has not been told anything changed.
	refs, err := a.resolveTargets(ctx, imap.SeqSetNum(2))
	if err != nil {
		t.Fatalf("resolveTargets: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("sequence 2 resolved to %d messages, want 1", len(refs))
	}
	if refs[0].uid != 2 {
		t.Fatalf("sequence 2 resolved to UID %d; A was told 2 is UID 2 and nothing has told it otherwise",
			refs[0].uid)
	}
}

// TestPollReportsAnotherSessionsExpunge pins that the renumbering is
// communicated rather than silently applied: Poll must emit the EXPUNGE that
// justifies the change, and only then may the view move.
func TestPollReportsAnotherSessionsExpunge(t *testing.T) {
	a := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := a.Select("INBOX", nil); err != nil {
		t.Fatalf("SELECT: %v", err)
	}

	if _, err := a.be.Pool.Exec(ctx,
		`DELETE FROM messages WHERE folder_id = $1 AND uid = 1`, a.selectedFolderID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// With allowExpunge=false the delta must be withheld ENTIRELY: renumbering
	// a client that has not been sent the expunge is the defect.
	snapshot, err := a.folderSnapshot(ctx, a.be.Pool, a.selectedFolderID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	d := a.view.diff(snapshot)
	if len(d.expunged) != 1 || d.expunged[0] != 1 {
		t.Fatalf("diff.expunged = %v, want [1]", d.expunged)
	}
	if err := a.reconcile(ctx, nil, false); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if a.view.len() != 3 {
		t.Fatalf("the view moved to %d while expunges were not allowed; the client was renumbered without being told",
			a.view.len())
	}

	// Once expunges may be sent, the same delta applies and the view follows.
	a.view.apply(snapshot)
	if a.view.len() != 2 {
		t.Fatalf("view holds %d after applying the snapshot, want 2", a.view.len())
	}
	if got := a.view.seqOf(2); got != 1 {
		t.Fatalf("UID 2 is sequence %d after the expunge, want 1", got)
	}
}

// TestOwnExpungeKeepsTheViewInStep pins that a session's own EXPUNGE updates
// its view, so a following sequence-number command means what the client
// expects after the EXPUNGE responses it just received.
func TestOwnExpungeKeepsTheViewInStep(t *testing.T) {
	sess := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := sess.Select("INBOX", nil); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	// The fixture flags uid 3 \Deleted.
	if err := sess.Expunge(nil, nil); err != nil {
		t.Fatalf("Expunge: %v", err)
	}
	if got := sess.view.len(); got != 2 {
		t.Fatalf("view holds %d after expunging one of three, want 2", got)
	}
	if sess.view.contains(3) {
		t.Fatal("the expunged UID is still in the view")
	}

	refs, err := sess.resolveTargets(ctx, imap.SeqSetNum(2))
	if err != nil {
		t.Fatalf("resolveTargets: %v", err)
	}
	if len(refs) != 1 || refs[0].uid != 2 {
		t.Fatalf("sequence 2 = %v after the expunge, want UID 2", refs)
	}
}

// TestOwnMoveKeepsTheViewInStep is the same for MOVE, which also writes
// EXPUNGE responses for the messages it removes.
func TestOwnMoveKeepsTheViewInStep(t *testing.T) {
	sess := mutationFixture(t)
	if _, err := sess.Select("INBOX", nil); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if err := sess.Move(nil, imap.UIDSetNum(1), "Archive/2026"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if got := sess.view.len(); got != 2 {
		t.Fatalf("view holds %d after moving one of three away, want 2", got)
	}
	if sess.view.contains(1) {
		t.Fatal("the moved UID is still in the view")
	}
}

// TestArrivalsAreReportedAsExists pins the other direction: a message
// delivered while the session is selected becomes addressable only after the
// EXISTS that announces it.
func TestArrivalsAreReportedAsExists(t *testing.T) {
	sess := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := sess.Select("INBOX", nil); err != nil {
		t.Fatalf("SELECT: %v", err)
	}

	insertExtraMessage(t, ctx, sess, 9)

	// Not yet announced: the client cannot address it.
	if sess.view.contains(9) {
		t.Fatal("an unannounced arrival is already in the view")
	}
	refs, err := sess.resolveTargets(ctx, imap.UIDSetNum(9))
	if err != nil {
		t.Fatalf("resolveTargets: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("an unannounced arrival resolved to %d targets, want 0", len(refs))
	}

	// After reconciling, it is part of the view with the right sequence number.
	snapshot, err := sess.folderSnapshot(ctx, sess.be.Pool, sess.selectedFolderID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	d := sess.view.diff(snapshot)
	if !d.hasExists || d.numMessages != 4 {
		t.Fatalf("diff = %+v, want an EXISTS of 4", d)
	}
	sess.view.apply(snapshot)
	if got := sess.view.seqOf(9); got != 4 {
		t.Fatalf("the arrival is sequence %d, want 4", got)
	}
}

// TestFlagChangesByAnotherSessionAreReported pins that a flag change made
// elsewhere becomes a FETCH FLAGS update, and that a session's OWN change does
// not come back at it as unsolicited news.
func TestFlagChangesByAnotherSessionAreReported(t *testing.T) {
	sess := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := sess.Select("INBOX", nil); err != nil {
		t.Fatalf("SELECT: %v", err)
	}

	// Another session flags UID 2 (bumping mod_seq, as STORE does).
	if _, err := sess.be.Pool.Exec(ctx,
		`UPDATE messages SET flags = flags || '{"\\Seen"}'::text[], mod_seq = mod_seq + 1
		  WHERE folder_id = $1 AND uid = 2`, sess.selectedFolderID); err != nil {
		t.Fatalf("peer store: %v", err)
	}

	snapshot, err := sess.folderSnapshot(ctx, sess.be.Pool, sess.selectedFolderID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	d := sess.view.diff(snapshot)
	if len(d.flagChanged) != 1 || d.flagChanged[0] != 2 {
		t.Fatalf("diff.flagChanged = %v, want [2]", d.flagChanged)
	}
	sess.view.apply(snapshot)

	// This session's own STORE must not reappear as an update.
	if err := sess.Store(nil, imap.UIDSetNum(1), &imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Silent: true,
		Flags:  []imap.Flag{imap.FlagFlagged},
	}, nil); err != nil {
		t.Fatalf("Store: %v", err)
	}
	snapshot, err = sess.folderSnapshot(ctx, sess.be.Pool, sess.selectedFolderID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if d := sess.view.diff(snapshot); len(d.flagChanged) != 0 {
		t.Fatalf("the session's own STORE came back as an update for %v", d.flagChanged)
	}
}

// TestReadOnlySelectionKeepsItsView pins that EXAMINE gets the same view
// machinery — a read-only selection still needs correct sequence numbers.
func TestReadOnlySelectionKeepsItsView(t *testing.T) {
	sess := mutationFixture(t)
	data, err := sess.Select("INBOX", &imap.SelectOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("EXAMINE: %v", err)
	}
	if !sess.selectedReadOnly {
		t.Fatal("read-only selection was not recorded")
	}
	if sess.view.len() != int(data.NumMessages) {
		t.Fatalf("view holds %d but EXISTS announced %d", sess.view.len(), data.NumMessages)
	}
}

// TestUnselectClearsTheView pins that a deselected session carries nothing
// forward into its next SELECT.
func TestUnselectClearsTheView(t *testing.T) {
	sess := mutationFixture(t)
	if _, err := sess.Select("INBOX", nil); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if err := sess.Unselect(); err != nil {
		t.Fatalf("UNSELECT: %v", err)
	}
	if sess.view != nil {
		t.Fatal("the view survived UNSELECT")
	}
}

// insertExtraMessage adds one message directly, standing in for a delivery
// that lands while a session is selected.
func insertExtraMessage(t *testing.T, ctx context.Context, sess *Session, uid int64) {
	t.Helper()
	sha := fixtureSHA(uid)
	when := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if _, err := sess.be.Pool.Exec(ctx, `
		INSERT INTO messages (
			folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date,
			subject, from_addr, to_addrs, cc_addrs, headers, text_body, bodystructure, flags
		) VALUES ($1, $2, $3, $4::date, 100, $5::timestamptz, 'arrival', 's@x', '{}', '{}', '{}', 'body', '{}', '{}')`,
		sess.selectedFolderID, uid, sha, when, when,
	); err != nil {
		t.Fatalf("insert arrival: %v", err)
	}
}
