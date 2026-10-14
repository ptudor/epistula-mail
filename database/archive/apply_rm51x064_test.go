package archive_test

import (
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/archive"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

func TestApplyRechecksDeletedFlag(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := testContext(t)
	f := newFixture(t, ctx, db)
	mb := f.mailbox("applyflag")
	root := f.folder(mb, "Archive", `\Archive`)
	f.categories(mb, storage.ArchiveCategory{Key: "travel", Folder: "Archive/Travel"})
	id := f.message(root, msgOpt{})
	f.classify(id, "travel", 0.9)
	p, err := archive.BuildPlan(ctx, db, mb, archive.DefaultRules(), time.Now())
	if err != nil || len(p.Moves) != 1 {
		t.Fatalf("plan=%+v err=%v", p, err)
	}
	if _, err := db.Pool().Exec(ctx, `UPDATE messages SET flags=ARRAY['\Deleted'] WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	stats, err := archive.Apply(ctx, db, p, archive.ApplyOptions{Batch: "flag-check"})
	if err != nil || stats.Moved != 0 || stats.Skipped != 1 {
		t.Fatalf("apply=%+v err=%v", stats, err)
	}
	f.mustBeIn(id, "Archive")
	if n := f.scalar(`SELECT count(*) FROM archive_moves WHERE message_id=$1`, id); n != 0 {
		t.Fatalf("journal=%d", n)
	}
}
