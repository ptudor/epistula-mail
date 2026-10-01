package archive_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/archive"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// TestPlanApplyUndo walks the one-time reorganization through every decision
// the plan can make, applies it, checks the journal, and undoes it.
func TestPlanApplyUndo(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := testContext(t)
	f := newFixture(t, ctx, db)
	mb := f.mailbox("reorg")
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

	inbox := f.folder(mb, "INBOX", "")
	root := f.folder(mb, "Archive", `\Archive`)
	trash := f.folder(mb, "Deleted Messages", `\Trash`)
	sent := f.folder(mb, "Sent Messages", `\Sent`)
	sentOld := f.folder(mb, "Sent Messages/2004", "")
	taxes := f.folder(mb, "Taxes", "")
	receipts := f.folder(mb, "Receipts", "")
	dump := f.folder(mb, "webmail-archive", "")
	f.categories(mb,
		storage.ArchiveCategory{Key: "finance/banking", Folder: "Archive/Finance/Banking"},
		storage.ArchiveCategory{Key: "shopping", Folder: "Archive/Shopping"},
		storage.ArchiveCategory{Key: "travel", Folder: "Archive/Travel"},
	)
	banking := f.folder(mb, "Archive/Finance/Banking", "")

	old := now.AddDate(-2, 0, 0)
	recent := now.AddDate(0, 0, -3)
	ids := map[string]int64{}
	add := func(name string, folder int64, o msgOpt, category string, conf float32) {
		if o.internal.IsZero() {
			o.internal = old
		}
		ids[name] = f.message(folder, o)
		if category != "" {
			f.classify(ids[name], category, conf)
		}
	}
	add("inbox-classified", inbox, msgOpt{}, "travel", 0.9)
	add("inbox-lowconf", inbox, msgOpt{}, "travel", 0.2) // drained to the root
	add("inbox-recent", inbox, msgOpt{internal: recent}, "travel", 0.9)
	add("inbox-deleted", inbox, msgOpt{flags: []string{`\Deleted`}}, "travel", 0.9)
	add("root-classified", root, msgOpt{}, "shopping", 0.8)
	add("root-unclassified", root, msgOpt{}, "", 0)   // stays in the root
	add("trash-unclassified", trash, msgOpt{}, "", 0) // drained to the root
	add("trash-classified", trash, msgOpt{}, "finance/banking", 0.7)
	add("sent", sent, msgOpt{}, "travel", 0.9)            // \Sent is kept
	add("sent-old", sentOld, msgOpt{}, "travel", 0.9)     // and so is its subtree
	add("taxes", taxes, msgOpt{}, "finance/banking", 0.9) // keep rule
	add("receipts", receipts, msgOpt{}, "travel", 0.95)   // folder rule beats the classifier
	add("dump-classified", dump, msgOpt{}, "finance/banking", 0.6)
	add("dump-lowconf", dump, msgOpt{}, "shopping", 0.59)    // not drained: stays
	add("dump-retired", dump, msgOpt{}, "retired-key", 0.99) // no active category: stays
	add("already-filed", banking, msgOpt{}, "travel", 0.99)  // in a category folder: stays

	rules, err := archive.ParseRules(strings.NewReader(`
min_confidence = 0.6
inbox_keep_days = 14
keep = ["Taxes"]
[folders]
"Receipts" = "shopping"
`))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := archive.BuildPlan(ctx, db, mb, rules, now)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	wantMoves := map[string]string{
		"inbox-classified":   "Archive/Travel",
		"inbox-lowconf":      "Archive",
		"root-classified":    "Archive/Shopping",
		"trash-unclassified": "Archive",
		"trash-classified":   "Archive/Finance/Banking",
		"receipts":           "Archive/Shopping",
		"dump-classified":    "Archive/Finance/Banking",
	}
	got := map[int64]archive.Move{}
	for _, m := range plan.Moves {
		got[m.MessageID] = m
	}
	for name, id := range ids {
		m, moving := got[id]
		want, shouldMove := wantMoves[name]
		switch {
		case shouldMove && !moving:
			t.Errorf("%s: planned to stay; want a move to %q", name, want)
		case !shouldMove && moving:
			t.Errorf("%s: planned to move to %q; want it to stay", name, m.ToFolder)
		case shouldMove && m.ToFolder != want:
			t.Errorf("%s: planned to %q; want %q", name, m.ToFolder, want)
		}
	}
	if got[ids["receipts"]].Reason != archive.ReasonFolderRule || got[ids["inbox-lowconf"]].Reason != archive.ReasonDrain {
		t.Errorf("reasons: receipts=%q inbox-lowconf=%q", got[ids["receipts"]].Reason, got[ids["inbox-lowconf"]].Reason)
	}
	var report bytes.Buffer
	if err := plan.WriteReport(&report); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"16 messages: 7 move, 9 stay", "inbox-recent", `flagged \Deleted`, "keep", "low-confidence", "unclassified", "filed"} {
		if !strings.Contains(report.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, report.String())
		}
	}
	var tsv bytes.Buffer
	if err := plan.WriteMoves(&tsv); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(tsv.String(), "\n"); lines != 8 {
		t.Errorf("TSV has %d lines, want header + 7", lines)
	}

	// A message the user moves between planning and applying is left alone.
	other := f.folder(mb, "Elsewhere", "")
	if _, err := db.Pool().Exec(ctx,
		`UPDATE messages SET folder_id = $1, uid = 1 WHERE id = $2`, other, ids["dump-classified"]); err != nil {
		t.Fatal(err)
	}
	stats, err := archive.Apply(ctx, db, plan, archive.ApplyOptions{Batch: "reorg-test", BatchSize: 2})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stats.Moved != 6 || stats.Skipped != 1 {
		t.Fatalf("apply stats = %+v; want 6 moved, 1 skipped", stats)
	}
	for name, folder := range wantMoves {
		if name == "dump-classified" {
			f.mustBeIn(ids[name], "Elsewhere")
			continue
		}
		f.mustBeIn(ids[name], folder)
	}
	if n := f.scalar(`SELECT count(*) FROM archive_moves WHERE batch = 'reorg-test' AND reason = 'reorg'`); n != 6 {
		t.Fatalf("journal rows = %d, want 6", n)
	}
	if n := f.scalar(`SELECT count(*) FROM archive_moves WHERE batch = 'reorg-test' AND category IS NULL`); n != 2 {
		t.Fatalf("journal rows without a category = %d, want the 2 drains", n)
	}

	// A fresh plan finds nothing left to do for what moved.
	again, err := archive.BuildPlan(ctx, db, mb, rules, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range again.Moves {
		if m.MessageID != ids["dump-classified"] {
			t.Errorf("re-plan moves message %d (%s → %s) again", m.MessageID, m.FromFolder, m.ToFolder)
		}
	}

	// The user files one of the moved messages by hand before the undo: it
	// must stay where the user put it.
	if _, err := db.Pool().Exec(ctx,
		`UPDATE messages SET folder_id = $1, uid = 2 WHERE id = $2`, other, ids["root-classified"]); err != nil {
		t.Fatal(err)
	}
	dry, err := archive.Undo(ctx, db, mb, "reorg-test", true, 2)
	if err != nil || dry.Restored != 5 || dry.Moved != 1 {
		t.Fatalf("undo dry run = %+v, %v; want 5 to restore, 1 moved since", dry, err)
	}
	// Prune first, so the undo has to recreate a source folder.
	if _, err := db.Pool().Exec(ctx, `DELETE FROM folders WHERE id = $1`, receipts); err != nil {
		t.Fatal(err)
	}
	undo, err := archive.Undo(ctx, db, mb, "reorg-test", false, 2)
	if err != nil || undo.Restored != 5 || undo.Moved != 1 {
		t.Fatalf("undo = %+v, %v", undo, err)
	}
	f.mustBeIn(ids["inbox-classified"], "INBOX")
	f.mustBeIn(ids["trash-unclassified"], "Deleted Messages")
	f.mustBeIn(ids["receipts"], "Receipts")
	f.mustBeIn(ids["root-classified"], "Elsewhere")
	again2, err := archive.Undo(ctx, db, mb, "reorg-test", false, 2)
	if err != nil || again2.Restored != 0 {
		t.Fatalf("second undo = %+v, %v; want nothing restored", again2, err)
	}
}

// TestPlanRefusesUnknownRuleCategory: a [folders] rule may only name an
// active key, or it would file mail into a folder nobody approved.
func TestPlanRefusesUnknownRuleCategory(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := testContext(t)
	f := newFixture(t, ctx, db)
	mb := f.mailbox("badrule")
	f.folder(mb, "Archive", `\Archive`)
	f.categories(mb, storage.ArchiveCategory{Key: "travel", Folder: "Archive/Travel"})
	rules := archive.DefaultRules()
	rules.Folders = map[string]string{"Receipts": "shopping"}
	if _, err := archive.BuildPlan(ctx, db, mb, rules, time.Now()); err == nil || !strings.Contains(err.Error(), "not active") {
		t.Fatalf("err = %v; want an unknown-category refusal", err)
	}
}
