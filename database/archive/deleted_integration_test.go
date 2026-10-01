package archive_test

import (
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/archive"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestDeletedTrackerArchivesLingeringInboxDeletes: a message a client marked
// \Deleted in INBOX and never expunged is archived once the mark has stood for
// the settle delay, with its mark cleared. An undo before then keeps it, and a
// marked message with a copy elsewhere is left for its expunge.
func TestDeletedTrackerArchivesLingeringInboxDeletes(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := testContext(t)
	f := newFixture(t, ctx, db)
	mb := f.mailbox("lingering")
	inbox := f.folder(mb, "INBOX", "")
	f.folder(mb, "Archive", `\Archive`)
	other := f.folder(mb, "Receipts", "")
	sent := f.folder(mb, "Sent Messages", `\Sent`)

	marked := f.message(inbox, msgOpt{flags: []string{`\Deleted`}})
	undone := f.message(inbox, msgOpt{flags: []string{`\Deleted`}})
	moving := f.message(inbox, msgOpt{content: "copied", flags: []string{`\Deleted`}})
	f.message(other, msgOpt{content: "copied"})
	plain := f.message(inbox, msgOpt{})
	elsewhere := f.message(sent, msgOpt{flags: []string{`\Deleted`}}) // only INBOX is swept

	clock := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	opts := archive.DeletedOptions{SettleDelay: 5 * time.Minute, Now: func() time.Time { return clock }}
	tr := archive.NewDeletedTracker()

	// First sight only starts the clock.
	if n, err := tr.ArchiveOnce(ctx, db, opts); err != nil || n != 0 {
		t.Fatalf("first pass = %d, %v; want nothing archived yet", n, err)
	}
	// The user undoes one delete inside the window.
	if _, err := db.Pool().Exec(ctx, `UPDATE messages SET flags = '{}' WHERE id = $1`, undone); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(6 * time.Minute)
	n, err := tr.ArchiveOnce(ctx, db, opts)
	if err != nil || n != 1 {
		t.Fatalf("second pass = %d, %v; want 1 archived", n, err)
	}
	f.mustBeIn(marked, "Archive")
	f.mustBeIn(undone, "INBOX")
	f.mustBeIn(moving, "INBOX") // has a copy: its expunge will remove it
	f.mustBeIn(plain, "INBOX")
	f.mustBeIn(elsewhere, "Sent Messages")
	if n := f.scalar(`SELECT count(*) FROM messages WHERE id = $1 AND '\Deleted' = ANY(flags)`, marked); n != 0 {
		t.Fatal("the archived message kept its \\Deleted mark")
	}

	// A new mark after a restart (a fresh tracker) waits the full delay again.
	if _, err := db.Pool().Exec(ctx, `UPDATE messages SET flags = '{"\\Deleted"}' WHERE id = $1`, plain); err != nil {
		t.Fatal(err)
	}
	tr = archive.NewDeletedTracker()
	if n, _ := tr.ArchiveOnce(ctx, db, opts); n != 0 {
		t.Fatal("a fresh tracker archived a mark it had never seen before")
	}
}
