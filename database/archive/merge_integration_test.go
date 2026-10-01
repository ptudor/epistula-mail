package archive_test

import (
	"errors"
	"testing"

	"github.com/ptudor/epistula-mail/database/archive"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// TestMergeFoldersCombinesSentTree mirrors a typical imported sent-mail tree: a
// plain "Sent" folder and its dated children merge into the \Sent folder
// clients use, each message kept once, and the merge undoes like a
// reorganization batch.
func TestMergeFoldersCombinesSentTree(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := testContext(t)
	f := newFixture(t, ctx, db)
	mb := f.mailbox("merger")
	sentMessages := f.folder(mb, "Sent Messages", `\Sent`)
	sent := f.folder(mb, "Sent", "")
	sent2005 := f.folder(mb, "Sent/2005/06-Jun", "")
	sentOld := f.folder(mb, "Sent/Old", "")
	unrelated := f.folder(mb, "Sentimental", "") // shares a prefix, not a parent

	already := f.message(sentMessages, msgOpt{content: "dup"})
	a := f.message(sent, msgOpt{})
	dupOfAlready := f.message(sent, msgOpt{content: "dup"})
	b := f.message(sent2005, msgOpt{})
	c := f.message(sentOld, msgOpt{content: "twice"})
	cAgain := f.message(sent2005, msgOpt{content: "twice"}) // same message in two sources
	d := f.message(unrelated, msgOpt{})

	dry, err := archive.MergeFolders(ctx, db, mb, archive.MergeOptions{
		From: "Sent", Subtree: true, To: "Sent Messages", DryRun: true,
	})
	// a, b and one of the two "twice" copies move; the copy of "dup" and
	// the second "twice" are duplicates by then.
	if err != nil || dry.Moved != 3 || dry.Skipped != 2 {
		t.Fatalf("dry run = %+v, %v; want 3 to move, 2 duplicates", dry, err)
	}
	f.mustBeIn(a, "Sent")

	stats, err := archive.MergeFolders(ctx, db, mb, archive.MergeOptions{
		From: "Sent", Subtree: true, To: "Sent Messages", DropDuplicates: true, Batch: "merge-test",
	})
	if err != nil {
		t.Fatalf("MergeFolders: %v", err)
	}
	if stats.Moved != 3 || stats.Dropped != 2 {
		t.Fatalf("stats = %+v; want 3 moved, 2 dropped", stats)
	}
	for _, id := range []int64{already, a, b} {
		f.mustBeIn(id, "Sent Messages")
	}
	f.mustBeGone(dupOfAlready)
	// "twice" lands once: whichever source came first moved, the other was
	// a duplicate by then.
	twice := 0
	for _, id := range []int64{c, cAgain} {
		if p, ok := f.where(id); ok && p.folder == "Sent Messages" {
			twice++
		}
	}
	if twice != 1 {
		t.Fatalf("the message held by two sources is in Sent Messages %d times", twice)
	}
	f.mustBeIn(d, "Sentimental")

	// Pruning removes the emptied tree; undo would recreate it.
	names, err := db.PruneEmptyFolders(ctx, mb, false)
	if err != nil || len(names) != 4 {
		t.Fatalf("pruned %v, %v; want the 4 emptied Sent folders", names, err)
	}
	undo, err := archive.Undo(ctx, db, mb, "merge-test", false, 100)
	if err != nil || undo.Restored != 3 {
		t.Fatalf("undo = %+v, %v", undo, err)
	}
	f.mustBeIn(a, "Sent")
	f.mustBeIn(b, "Sent/2005/06-Jun")

	if _, err := archive.MergeFolders(ctx, db, mb, archive.MergeOptions{
		From: "Nope", To: "Sent Messages", Batch: "x",
	}); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("merging a missing folder: err = %v", err)
	}
}

// TestMergeRedirectsLaterImports covers what a Maildir sync after a merge
// relies on (migration 021): the merge records where its emptied folders
// went, an import into one of them resolves to the destination once prune
// has removed it, a folder that still exists is its own answer, and undoing
// the merge removes the redirect with the moves.
func TestMergeRedirectsLaterImports(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := testContext(t)
	f := newFixture(t, ctx, db)
	mb := f.mailbox("redirects")
	f.message(f.folder(mb, "Sent Messages", `\Sent`), msgOpt{})
	f.message(f.folder(mb, "Sent", ""), msgOpt{})
	f.message(f.folder(mb, "Sent/Old", ""), msgOpt{content: "dup"})
	f.message(f.folder(mb, "Sent Messages", `\Sent`), msgOpt{content: "dup"})
	f.message(f.folder(mb, "Lists", ""), msgOpt{})
	f.folder(mb, "Archive", `\Archive`)
	f.message(f.folder(mb, "Sentimental", ""), msgOpt{})
	f.message(f.folder(mb, "Old", ""), msgOpt{})

	resolve := func(name string) (string, bool) {
		t.Helper()
		got, redirected, err := db.ResolveImportFolder(ctx, mb, name)
		if err != nil {
			t.Fatalf("ResolveImportFolder(%q): %v", name, err)
		}
		return got, redirected
	}
	merge := func(from string, subtree bool, to, batch string) {
		t.Helper()
		if _, err := archive.MergeFolders(ctx, db, mb, archive.MergeOptions{
			From: from, Subtree: subtree, To: to, DropDuplicates: true, Batch: batch,
		}); err != nil {
			t.Fatalf("merge %q into %q: %v", from, to, err)
		}
	}
	prune := func() {
		t.Helper()
		if _, err := db.PruneEmptyFolders(ctx, mb, false); err != nil {
			t.Fatalf("prune: %v", err)
		}
	}

	if _, err := archive.MergeFolders(ctx, db, mb, archive.MergeOptions{
		From: "Sent", Subtree: true, To: "Sent Messages", DryRun: true,
	}); err != nil {
		t.Fatalf("dry-run merge: %v", err)
	}
	if n := f.scalar(`SELECT count(*) FROM folder_redirects`); n != 0 {
		t.Fatalf("a dry-run merge recorded %d redirect(s)", n)
	}

	// Sent/Old holds only a duplicate, so the merge moves nothing
	// out of it; the subtree redirect still covers it.
	merge("Sent", true, "Sent Messages", "merge-sent")
	merge("Lists", false, "Archive", "merge-lists")

	// Emptied but not yet pruned: the folders are still there to import into.
	if got, redirected := resolve("Sent"); got != "Sent" || redirected {
		t.Fatalf("before prune, Sent resolved to %q (redirected=%v); want itself", got, redirected)
	}
	prune()

	for _, tc := range []struct {
		name, want string
		redirected bool
	}{
		{"Sent", "Sent Messages", true},
		{"Sent/Old", "Sent Messages", true},
		{"Sent/2026", "Sent Messages", true},          // new below a subtree merge
		{"Sent Messages", "Sent Messages", false},     // the destination itself
		{"Sentimental", "Sentimental", false},         // shares a prefix, and exists
		{"Sentimental/new", "Sentimental/new", false}, // a prefix is not a parent
		{"Lists", "Archive", true},                    // exact-name merge
		{"Lists/openbsd", "Lists/openbsd", false},     // not covered without -subtree
		{"Brand/new", "Brand/new", false},             // nothing ever merged
	} {
		if got, redirected := resolve(tc.name); got != tc.want || redirected != tc.redirected {
			t.Errorf("resolve(%q) = %q, %v; want %q, %v", tc.name, got, redirected, tc.want, tc.redirected)
		}
	}

	// A folder merged into one that was merged and pruned in turn resolves to
	// the last destination.
	merge("Old", false, "Mid", "merge-old")
	prune()
	merge("Mid", false, "New", "merge-mid")
	prune()
	if got, redirected := resolve("Old"); got != "New" || !redirected {
		t.Fatalf("resolve(Old) = %q, %v; want New through Mid", got, redirected)
	}

	// Undo removes the batch's redirect with its moves; the restored folders
	// are imported into as named again.
	dry, err := archive.Undo(ctx, db, mb, "merge-sent", true, 100)
	if err != nil || dry.Redirects != 1 {
		t.Fatalf("dry-run undo = %+v, %v; want 1 redirect reported", dry, err)
	}
	undo, err := archive.Undo(ctx, db, mb, "merge-sent", false, 100)
	if err != nil || undo.Redirects != 1 || undo.Restored != 1 {
		t.Fatalf("undo = %+v, %v; want 1 message restored and 1 redirect removed", undo, err)
	}
	if got, redirected := resolve("Sent"); got != "Sent" || redirected {
		t.Fatalf("after undo, resolve(Sent) = %q, %v; want the restored folder", got, redirected)
	}
	if got, redirected := resolve("Sent/2026"); got != "Sent/2026" || redirected {
		t.Fatalf("after undo, resolve(Sent/2026) = %q, %v; want no redirect", got, redirected)
	}
	if n := f.scalar(`SELECT count(*) FROM folder_redirects WHERE batch = 'merge-sent'`); n != 0 {
		t.Fatalf("undo left %d redirect row(s) for its batch", n)
	}

	// Redirects that lead back to where they started give no answer.
	for _, r := range []storage.FolderRedirect{
		{From: "Ping", To: "Pong", Batch: "merge-ping"},
		{From: "Pong", To: "Ping", Batch: "merge-pong"},
	} {
		if err := db.RecordFolderRedirect(ctx, mb, r); err != nil {
			t.Fatalf("record %+v: %v", r, err)
		}
	}
	if _, _, err := db.ResolveImportFolder(ctx, mb, "Ping"); !errors.Is(err, storage.ErrFolderRedirectCycle) {
		t.Fatalf("resolve(Ping) err = %v; want ErrFolderRedirectCycle", err)
	}
}
