package archive_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// TestMoveMessagesInPlace pins the contract the sorter and the reorganization
// rely on: a moved message keeps its id — so its attachments, annotations and
// classification stay attached without being copied — and arrives under fresh
// UIDs from the destination's uidnext, in chronological order, exactly as an
// IMAP MOVE would present it.
func TestMoveMessagesInPlace(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := testContext(t)
	f := newFixture(t, ctx, db)
	mb := f.mailbox("mover")
	inbox := f.folder(mb, "INBOX", "")
	other := f.folder(mb, "Other", "")

	late := f.message(inbox, msgOpt{internal: time.Date(2021, 5, 1, 0, 0, 0, 0, time.UTC)})
	early := f.message(inbox, msgOpt{internal: time.Date(2019, 5, 1, 0, 0, 0, 0, time.UTC)})
	elsewhere := f.message(other, msgOpt{})
	f.classify(late, "misc", 0.9)
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO attachments (message_id, part_number, content_type, size_bytes, sha256)
		 VALUES ($1, '2', 'application/pdf', 10, '\x00')`, late); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO message_annotations (message_id, model, summary) VALUES ($1, 'm', 's')`, late); err != nil {
		t.Fatal(err)
	}
	f.age(late, 48*time.Hour)
	usedBefore := f.scalar(`SELECT used_bytes FROM mailboxes WHERE id = $1`, mb)
	inboxModseq := f.scalar(`SELECT highest_modseq FROM folders WHERE id = $1`, inbox)

	var moved []storage.MovedMessage
	err := db.RunTx(ctx, func(tx pgx.Tx) error {
		var err error
		moved, err = storage.MoveMessages(ctx, tx, mb, []storage.MoveItem{
			{ID: late, FromFolderID: inbox},
			{ID: early, FromFolderID: inbox},
			// Expected in INBOX but actually in Other: someone moved it, so
			// it must be left alone.
			{ID: elsewhere, FromFolderID: inbox},
		}, "Archive/Travel/Flights")
		return err
	})
	if err != nil {
		t.Fatalf("MoveMessages: %v", err)
	}
	if len(moved) != 2 {
		t.Fatalf("moved %d messages, want 2: %+v", len(moved), moved)
	}
	// Chronological UIDs: the 2019 message gets the first UID.
	pe, _ := f.where(early)
	pl, _ := f.where(late)
	if pe.folder != "Archive/Travel/Flights" || pl.folder != "Archive/Travel/Flights" {
		t.Fatalf("messages are in %q and %q", pe.folder, pl.folder)
	}
	if pe.uid != 1 || pl.uid != 2 {
		t.Fatalf("uids = early %d, late %d; want 1, 2 (internal_date order)", pe.uid, pl.uid)
	}
	f.mustBeIn(elsewhere, "Other")
	for _, name := range []string{"Archive", "Archive/Travel", "Archive/Travel/Flights"} {
		if f.scalar(`SELECT count(*) FROM folders WHERE mailbox_id = $1 AND name = $2`, mb, name) != 1 {
			t.Fatalf("folder %q was not created", name)
		}
	}
	if n := f.scalar(`SELECT count(*) FROM attachments WHERE message_id = $1`, late); n != 1 {
		t.Fatalf("attachment rows after move = %d, want 1 (kept, not copied)", n)
	}
	if n := f.scalar(`SELECT count(*) FROM message_annotations WHERE message_id = $1`, late); n != 1 {
		t.Fatalf("annotation rows after move = %d, want 1", n)
	}
	if n := f.scalar(`SELECT count(*) FROM message_classifications WHERE message_id = $1`, late); n != 1 {
		t.Fatalf("classification rows after move = %d, want 1", n)
	}
	if got := f.scalar(`SELECT used_bytes FROM mailboxes WHERE id = $1`, mb); got != usedBefore {
		t.Fatalf("used_bytes changed from %d to %d; a move within a mailbox is quota-neutral", usedBefore, got)
	}
	if got := f.scalar(`SELECT highest_modseq FROM folders WHERE id = $1`, inbox); got <= inboxModseq {
		t.Fatalf("source highest_modseq did not advance (%d → %d)", inboxModseq, got)
	}
	if f.scalar(`SELECT uidnext FROM folders WHERE mailbox_id = $1 AND name = 'Archive/Travel/Flights'`, mb) != 3 {
		t.Fatal("destination uidnext was not advanced past the allocated block")
	}
	// created_at is "entered this folder at": the 48h backdate is gone.
	if f.scalar(`SELECT count(*) FROM messages WHERE id = $1 AND created_at > now() - interval '1 hour'`, late) != 1 {
		t.Fatal("created_at was not reset by the move")
	}

	// Moving again to where they already are changes nothing.
	destID := f.scalar(`SELECT id FROM folders WHERE mailbox_id = $1 AND name = 'Archive/Travel/Flights'`, mb)
	err = db.RunTx(ctx, func(tx pgx.Tx) error {
		var err error
		moved, err = storage.MoveMessages(ctx, tx, mb, []storage.MoveItem{{ID: late, FromFolderID: destID}}, "Archive/Travel/Flights")
		return err
	})
	if err != nil || len(moved) != 0 {
		t.Fatalf("re-move = %v, %v; want nothing moved", moved, err)
	}

	// A move of nothing creates nothing: an undo whose messages have all
	// been refiled must not resurrect a pruned folder.
	err = db.RunTx(ctx, func(tx pgx.Tx) error {
		var err error
		moved, err = storage.MoveMessages(ctx, tx, mb, []storage.MoveItem{{ID: elsewhere, FromFolderID: inbox}}, "Pruned/Long/Ago")
		return err
	})
	if err != nil || len(moved) != 0 {
		t.Fatalf("stale move = %v, %v; want nothing moved", moved, err)
	}
	if n := f.scalar(`SELECT count(*) FROM folders WHERE mailbox_id = $1 AND name LIKE 'Pruned%'`, mb); n != 0 {
		t.Fatalf("a move of nothing created %d folder(s)", n)
	}
}

// TestMoveMessagesStaysInMailbox: an id belonging to another mailbox is never
// moved, whatever the caller claims about it.
func TestMoveMessagesStaysInMailbox(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := testContext(t)
	f := newFixture(t, ctx, db)
	alice := f.mailbox("alice")
	bob := f.mailbox("bob")
	bobInbox := f.folder(bob, "INBOX", "")
	bobMsg := f.message(bobInbox, msgOpt{})
	err := db.RunTx(ctx, func(tx pgx.Tx) error {
		moved, err := storage.MoveMessages(ctx, tx, alice, []storage.MoveItem{{ID: bobMsg, FromFolderID: bobInbox}}, "Stolen")
		if len(moved) != 0 {
			t.Errorf("moved another mailbox's message: %+v", moved)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	f.mustBeIn(bobMsg, "INBOX")
}

// TestPurgeMessagesAllCopies: destroying a message "everywhere" takes its
// identical copies in other folders of the same mailbox, debits used_bytes by
// exactly what was deleted, and never reaches into another mailbox.
func TestPurgeMessagesAllCopies(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := testContext(t)
	f := newFixture(t, ctx, db)
	mb := f.mailbox("purger")
	other := f.mailbox("neighbour")
	trash := f.folder(mb, "Deleted Messages", `\Trash`)
	inbox := f.folder(mb, "INBOX", "")
	otherInbox := f.folder(other, "INBOX", "")

	target := f.message(trash, msgOpt{content: "same", size: 1000})
	copyInInbox := f.message(inbox, msgOpt{content: "same", size: 1000})
	unrelated := f.message(inbox, msgOpt{content: "different", size: 50})
	neighbourCopy := f.message(otherInbox, msgOpt{content: "same", size: 1000})
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO message_classifications (message_id, category, confidence, model) VALUES ($1, 'x', 0.5, 'm')`,
		copyInInbox); err != nil {
		t.Fatal(err)
	}

	var res storage.PurgeResult
	err := db.RunTx(ctx, func(tx pgx.Tx) error {
		var err error
		res, err = storage.PurgeMessages(ctx, tx, mb, []int64{target}, true)
		return err
	})
	if err != nil {
		t.Fatalf("PurgeMessages: %v", err)
	}
	if res.Messages != 2 || res.Copies != 1 || res.Bytes != 2000 {
		t.Fatalf("result = %+v; want 2 messages, 1 copy, 2000 bytes", res)
	}
	f.mustBeGone(target)
	f.mustBeGone(copyInInbox)
	f.mustBeIn(unrelated, "INBOX")
	f.mustBeIn(neighbourCopy, "INBOX")
	if got := f.scalar(`SELECT used_bytes FROM mailboxes WHERE id = $1`, mb); got != 50 {
		t.Fatalf("used_bytes = %d, want 50", got)
	}
	if n := f.scalar(`SELECT count(*) FROM message_classifications WHERE message_id = $1`, copyInInbox); n != 0 {
		t.Fatal("the classification of a destroyed message survived")
	}

	// Without allCopies only the named message goes.
	a := f.message(trash, msgOpt{content: "twin"})
	b := f.message(inbox, msgOpt{content: "twin"})
	err = db.RunTx(ctx, func(tx pgx.Tx) error {
		var err error
		res, err = storage.PurgeMessages(ctx, tx, mb, []int64{a}, false)
		return err
	})
	if err != nil || res.Messages != 1 {
		t.Fatalf("single purge = %+v, %v", res, err)
	}
	f.mustBeGone(a)
	f.mustBeIn(b, "INBOX")
}

// TestReplaceArchiveCategories covers the validation that keeps a category
// list from filing mail anywhere but inside the \Archive folder.
func TestReplaceArchiveCategories(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := testContext(t)
	f := newFixture(t, ctx, db)
	mb := f.mailbox("taxonomy")

	good := []storage.ArchiveCategory{
		{Key: "finance/banking", Folder: "Archive/Finance/Banking", Description: "statements"},
		{Key: "finance/banking/acme-bank", Folder: "Archive/Finance/Banking/Acme Bank", Annual: true},
		{Key: "travel", Folder: "Archive/Travel"},
	}
	if _, err := db.ReplaceArchiveCategories(ctx, mb, good, false); !errors.Is(err, storage.ErrNoArchiveFolder) {
		t.Fatalf("without an \\Archive folder: err = %v, want ErrNoArchiveFolder", err)
	}
	f.folder(mb, "Archive", `\Archive`)
	f.folder(mb, "Archive/Deleted", `\Trash`)

	for _, bad := range [][]storage.ArchiveCategory{
		{{Key: "Finance", Folder: "Archive/Finance"}},                                         // upper-case key
		{{Key: "a/b/c/d", Folder: "Archive/A"}},                                               // four levels
		{{Key: "a", Folder: "Archive/A", Annual: true}, {Key: "b", Folder: "Archive/A/2024"}}, // another's year folder
		{{Key: "a", Folder: "Archive/" + strings.Repeat("x", 246), Annual: true}},             // no room for the year
		{{Key: "inbox", Folder: "INBOX"}},                                                     // outside the archive
		{{Key: "root", Folder: "Archive"}},                                                    // the archive folder itself
		{{Key: "trash", Folder: "Archive/Deleted"}},                                           // a special-use folder
		{{Key: "x", Folder: "Archive//X"}},                                                    // empty segment
		{{Key: "x", Folder: "Archive/X"}, {Key: "x", Folder: "Archive/Y"}},                    // duplicate key
		{{Key: "x", Folder: "Archive/X", Description: strings.Repeat("d", 501)}},              // long description
	} {
		if _, err := db.ReplaceArchiveCategories(ctx, mb, bad, false); !errors.Is(err, storage.ErrInvalidArchiveCategory) {
			t.Errorf("%+v: err = %v, want ErrInvalidArchiveCategory", bad, err)
		}
	}

	ch, err := db.ReplaceArchiveCategories(ctx, mb, good, true)
	if err != nil || len(ch.Added) != 3 {
		t.Fatalf("dry run = %+v, %v", ch, err)
	}
	if n := f.scalar(`SELECT count(*) FROM archive_categories WHERE mailbox_id = $1`, mb); n != 0 {
		t.Fatal("dry run wrote categories")
	}
	if _, err := db.ReplaceArchiveCategories(ctx, mb, good, false); err != nil {
		t.Fatal(err)
	}

	next := []storage.ArchiveCategory{
		{Key: "finance/banking", Folder: "Archive/Money/Banks", Description: "statements"},
		{Key: "finance/banking/acme-bank", Folder: "Archive/Finance/Banking/Acme Bank"}, // no longer annual
		{Key: "shopping", Folder: "Archive/Shopping"},
	}
	ch, err = db.ReplaceArchiveCategories(ctx, mb, next, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ch.Added, ",") != "shopping" ||
		strings.Join(ch.Updated, ",") != "finance/banking,finance/banking/acme-bank" ||
		strings.Join(ch.Retired, ",") != "travel" {
		t.Fatalf("changes = %+v", ch)
	}
	all, err := db.ListArchiveCategories(ctx, mb, true)
	if err != nil || len(all) != 4 {
		t.Fatalf("list all = %+v, %v", all, err)
	}
	active, _ := db.ListArchiveCategories(ctx, mb, false)
	if len(active) != 3 {
		t.Fatalf("active = %+v", active)
	}
	for _, c := range active {
		if c.Key == "finance/banking/acme-bank" && c.Annual {
			t.Fatal("the annual flag was not cleared by the re-import")
		}
	}
	// Listing a retired key again reactivates it.
	ch, err = db.ReplaceArchiveCategories(ctx, mb, append(next, storage.ArchiveCategory{Key: "travel", Folder: "Archive/Travel"}), false)
	if err != nil || strings.Join(ch.Updated, ",") != "travel" {
		t.Fatalf("reactivate = %+v, %v", ch, err)
	}
}

// TestPruneEmptyFolders removes emptied folders bottom-up while keeping every
// folder something still needs.
func TestPruneEmptyFolders(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := testContext(t)
	f := newFixture(t, ctx, db)
	mb := f.mailbox("pruner")
	f.folder(mb, "INBOX", "")
	f.folder(mb, "Archive", `\Archive`)
	f.folder(mb, "Deleted Messages", `\Trash`)
	f.folder(mb, "Old/2004/11-Nov", "") // empty chain: all three go
	f.folder(mb, "Busy/Empty", "")      // empty child of a non-empty parent: goes
	busy := f.folder(mb, "Busy", "")
	f.message(busy, msgOpt{})
	f.folder(mb, "Keeper/Full", "") // non-empty child keeps its empty parent
	full := f.scalar(`SELECT id FROM folders WHERE mailbox_id = $1 AND name = 'Keeper/Full'`, mb)
	f.message(full, msgOpt{})
	f.categories(mb, storage.ArchiveCategory{Key: "finance/banking", Folder: "Archive/Finance/Banking"})
	f.folder(mb, "Archive/Finance/Banking", "") // an empty category folder and its parent stay

	names, err := db.PruneEmptyFolders(ctx, mb, true)
	if err != nil {
		t.Fatal(err)
	}
	want := "Old/2004/11-Nov,Busy/Empty,Old/2004,Old"
	if got := strings.Join(names, ","); got != want {
		t.Fatalf("dry run would remove %q, want %q", got, want)
	}
	if n := f.scalar(`SELECT count(*) FROM folders WHERE mailbox_id = $1 AND name LIKE 'Old%'`, mb); n != 3 {
		t.Fatal("dry run removed folders")
	}
	if _, err := db.PruneEmptyFolders(ctx, mb, false); err != nil {
		t.Fatal(err)
	}
	var left []string
	rows, err := db.Pool().Query(ctx, `SELECT name FROM folders WHERE mailbox_id = $1 ORDER BY name`, mb)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		left = append(left, n)
	}
	rows.Close()
	wantLeft := "Archive,Archive/Finance,Archive/Finance/Banking,Busy,Deleted Messages,INBOX,Keeper,Keeper/Full"
	if got := strings.Join(left, ","); got != wantLeft {
		t.Fatalf("folders left = %q, want %q", got, wantLeft)
	}
}
