package archive_test

import (
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/archive"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// TestReclassifyRefilesWhatWasFiled: clearing a key's classifications leaves
// other keys alone, and -refile returns to \Archive exactly the messages the
// sorter filed under the key that are still where it put them. What the owner
// filed by hand, or moved since, stays.
func TestReclassifyRefilesWhatWasFiled(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := testContext(t)
	f := newFixture(t, ctx, db)
	mb := f.mailbox("reclass")
	root := f.folder(mb, "Archive", `\Archive`)
	f.categories(mb,
		storage.ArchiveCategory{Key: "personal/other", Folder: "Archive/Personal/Other"},
		storage.ArchiveCategory{Key: "travel", Folder: "Archive/Travel"},
	)
	var filed []int64
	for i := 0; i < 3; i++ {
		id := f.message(root, msgOpt{})
		f.classify(id, "personal/other", 0.9)
		f.age(id, time.Hour)
		filed = append(filed, id)
	}
	travel := f.message(root, msgOpt{})
	f.classify(travel, "travel", 0.9)
	f.age(travel, time.Hour)
	if _, err := archive.SortOnce(ctx, db, archive.SortOptions{SettleDelay: time.Minute, MinConfidence: 0.6}); err != nil {
		t.Fatal(err)
	}
	for _, id := range filed {
		f.mustBeIn(id, "Archive/Personal/Other")
	}
	otherFolder := f.folder(mb, "Archive/Personal/Other", "")
	handFiled := f.message(otherFolder, msgOpt{})
	f.classify(handFiled, "personal/other", 0.9)
	unfiled := f.message(root, msgOpt{})
	f.classify(unfiled, "personal/other", 0.4)
	// The owner moves one of the sorter's filings elsewhere.
	elsewhere := f.folder(mb, "Keep", "")
	if _, err := db.Pool().Exec(ctx, `UPDATE messages SET folder_id = $1, uid = 1 WHERE id = $2`, elsewhere, filed[2]); err != nil {
		t.Fatal(err)
	}

	dry, err := archive.Reclassify(ctx, db, mb, archive.ReclassifyOptions{Key: "personal/other", Refile: true, DryRun: true})
	if err != nil || dry.Cleared != 5 || dry.Refiled != 2 || dry.MovedSince != 1 {
		t.Fatalf("dry run = %+v, %v; want 5 cleared, 2 refiled, 1 moved since", dry, err)
	}
	st, err := archive.Reclassify(ctx, db, mb, archive.ReclassifyOptions{Key: "personal/other", Refile: true, Batch: "refile-test"})
	if err != nil || st.Cleared != 5 || st.Refiled != 2 {
		t.Fatalf("reclassify = %+v, %v", st, err)
	}
	f.mustBeIn(filed[0], "Archive")
	f.mustBeIn(filed[1], "Archive")
	f.mustBeIn(filed[2], "Keep")
	f.mustBeIn(handFiled, "Archive/Personal/Other")
	f.mustBeIn(unfiled, "Archive")
	f.mustBeIn(travel, "Archive/Travel")
	if n := f.scalar(`SELECT count(*) FROM message_classifications WHERE category = 'personal/other'`); n != 0 {
		t.Fatalf("%d personal/other classifications remain", n)
	}
	if n := f.scalar(`SELECT count(*) FROM message_classifications WHERE category = 'travel'`); n != 1 {
		t.Fatal("another key's classification was cleared")
	}

	// Unclassified, the refiled messages are not filed again until the
	// worker classifies them.
	for _, id := range filed[:2] {
		f.age(id, time.Hour)
	}
	if st, _ := archive.SortOnce(ctx, db, archive.SortOptions{SettleDelay: time.Minute, MinConfidence: 0.6}); st.Filed != 0 {
		t.Fatalf("the sorter re-filed %d unclassified message(s)", st.Filed)
	}
	// And the refile undoes like any batch.
	undo, err := archive.Undo(ctx, db, mb, "refile-test", false, 100)
	if err != nil || undo.Restored != 2 {
		t.Fatalf("undo = %+v, %v", undo, err)
	}
	f.mustBeIn(filed[0], "Archive/Personal/Other")

	if _, err := archive.Reclassify(ctx, db, mb, archive.ReclassifyOptions{Key: "Not A Key", DryRun: true}); err == nil {
		t.Fatal("an invalid key was accepted")
	}
}
