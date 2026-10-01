package archive_test

import (
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/archive"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// TestSortOnceFilesSettledClassifiedArchive: the live sorter files exactly the
// archived messages that have settled, are confidently classified into an
// active category, and are not marked for expunge — and only ever into a
// folder inside the \Archive folder.
func TestSortOnceFilesSettledClassifiedArchive(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := testContext(t)
	f := newFixture(t, ctx, db)
	mb := f.mailbox("sorter")
	inbox := f.folder(mb, "INBOX", "")
	root := f.folder(mb, "Archive", `\Archive`)
	f.categories(mb,
		storage.ArchiveCategory{Key: "travel", Folder: "Archive/Travel"},
		storage.ArchiveCategory{Key: "shopping", Folder: "Archive/Shopping"},
		storage.ArchiveCategory{Key: "old", Folder: "Archive/Old"},
	)
	// Retire "old" by replacing the list without it.
	f.categories(mb,
		storage.ArchiveCategory{Key: "travel", Folder: "Archive/Travel"},
		storage.ArchiveCategory{Key: "shopping", Folder: "Archive/Shopping"},
		storage.ArchiveCategory{Key: "escape", Folder: "Archive/Escape"},
	)
	// A row nobody could import: its folder is outside the archive. The
	// sorter must refuse it even though the table says so.
	if _, err := db.Pool().Exec(ctx,
		`UPDATE archive_categories SET folder = 'INBOX' WHERE mailbox_id = $1 AND key = 'escape'`, mb); err != nil {
		t.Fatal(err)
	}

	msg := func(category string, conf float32, settled bool, flags []string) int64 {
		id := f.message(root, msgOpt{flags: flags})
		if category != "" {
			f.classify(id, category, conf)
		}
		if settled {
			f.age(id, 10*time.Minute)
		}
		return id
	}
	filed := msg("travel", 0.9, true, nil)
	filed2 := msg("shopping", 0.7, true, nil)
	unsettled := msg("travel", 0.9, false, nil)
	lowConf := msg("travel", 0.3, true, nil)
	unclassified := msg("", 0, true, nil)
	deleted := msg("travel", 0.9, true, []string{`\Deleted`})
	retired := msg("old", 0.9, true, nil)
	escape := msg("escape", 0.9, true, nil)
	notArchived := f.message(inbox, msgOpt{})
	f.classify(notArchived, "travel", 0.9)
	f.age(notArchived, time.Hour)

	opts := archive.SortOptions{SettleDelay: 5 * time.Minute, MinConfidence: 0.6, BatchSize: 1}
	stats, err := archive.SortOnce(ctx, db, opts)
	if err != nil {
		t.Fatalf("SortOnce: %v", err)
	}
	if stats.Filed != 2 || stats.Mailboxes != 1 {
		t.Fatalf("stats = %+v; want 2 filed in 1 mailbox", stats)
	}
	f.mustBeIn(filed, "Archive/Travel")
	f.mustBeIn(filed2, "Archive/Shopping")
	for _, id := range []int64{unsettled, lowConf, unclassified, deleted, retired, escape} {
		f.mustBeIn(id, "Archive")
	}
	f.mustBeIn(notArchived, "INBOX")
	if n := f.scalar(`SELECT count(*) FROM archive_moves WHERE reason = 'sort' AND batch LIKE 'sort-%'`); n != 2 {
		t.Fatalf("sort journal rows = %d, want 2", n)
	}

	// Once it settles, the next pass files it.
	f.age(unsettled, 10*time.Minute)
	if _, err := archive.SortOnce(ctx, db, opts); err != nil {
		t.Fatal(err)
	}
	f.mustBeIn(unsettled, "Archive/Travel")

	// A mailbox in maintenance is left alone.
	late := msg("travel", 0.9, true, nil)
	if _, err := db.Pool().Exec(ctx, `UPDATE mailboxes SET maintenance_at = now() WHERE id = $1`, mb); err != nil {
		t.Fatal(err)
	}
	if stats, err := archive.SortOnce(ctx, db, opts); err != nil || stats.Filed != 0 {
		t.Fatalf("sort during maintenance = %+v, %v", stats, err)
	}
	f.mustBeIn(late, "Archive")
}

// TestPurgeOnceDestroysExpiredTrash: only messages that have been in the
// \Trash folder past the retention period are destroyed, with their copies.
func TestPurgeOnceDestroysExpiredTrash(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := testContext(t)
	f := newFixture(t, ctx, db)
	mb := f.mailbox("trashman")
	trash := f.folder(mb, "Deleted Messages", `\Trash`)
	inbox := f.folder(mb, "INBOX", "")
	archiveRoot := f.folder(mb, "Archive", `\Archive`)

	expired := f.message(trash, msgOpt{content: "gone"})
	f.age(expired, 25*time.Hour)
	copyOfExpired := f.message(archiveRoot, msgOpt{content: "gone"})
	fresh := f.message(trash, msgOpt{})
	f.age(fresh, time.Hour)
	oldInbox := f.message(inbox, msgOpt{})
	f.age(oldInbox, 1000*time.Hour)

	stats, err := archive.PurgeOnce(ctx, db, archive.PurgeOptions{Retention: 24 * time.Hour, AllCopies: true, BatchSize: 1})
	if err != nil {
		t.Fatalf("PurgeOnce: %v", err)
	}
	if stats.Messages != 2 || stats.Copies != 1 {
		t.Fatalf("stats = %+v; want 2 messages including 1 copy", stats)
	}
	f.mustBeGone(expired)
	f.mustBeGone(copyOfExpired)
	f.mustBeIn(fresh, "Deleted Messages")
	f.mustBeIn(oldInbox, "INBOX")

	// No \Trash folder, no purge: a mailbox without one is never touched.
	other := f.mailbox("notrash")
	otherInbox := f.folder(other, "INBOX", "")
	keep := f.message(otherInbox, msgOpt{})
	f.age(keep, 1000*time.Hour)
	if _, err := archive.PurgeOnce(ctx, db, archive.PurgeOptions{Retention: 0, AllCopies: true}); err != nil {
		t.Fatal(err)
	}
	f.mustBeIn(keep, "INBOX")
	f.mustBeGone(fresh) // retention 0: everything in Trash goes
}
