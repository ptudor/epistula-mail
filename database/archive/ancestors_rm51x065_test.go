package archive_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/archive"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

func TestCategorySpecialUseAncestorRefused(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := testContext(t)
	f := newFixture(t, ctx, db)
	mb := f.mailbox("roles")
	root := f.folder(mb, "Archive", `\Archive`)
	cats := []storage.ArchiveCategory{{Key: "finance/banking", Folder: "Archive/Finance/Banking"}}
	f.categories(mb, cats...)
	id := f.message(root, msgOpt{})
	f.classify(id, "finance/banking", 0.9)
	f.age(id, time.Hour)
	plan, err := archive.BuildPlan(ctx, db, mb, archive.DefaultRules(), time.Now())
	if err != nil || len(plan.Moves) != 1 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	f.folder(mb, "Archive/Finance", `\Junk`)
	if _, err := db.ReplaceArchiveCategories(ctx, mb, cats, false); !errors.Is(err, storage.ErrInvalidArchiveCategory) {
		t.Fatalf("import=%v", err)
	}
	if stats, err := archive.SortOnce(ctx, db, archive.SortOptions{SettleDelay: time.Minute, MinConfidence: 0.6}); err != nil || stats.Filed != 0 {
		t.Fatalf("sort=%+v err=%v", stats, err)
	}
	if _, err := archive.Apply(ctx, db, plan, archive.ApplyOptions{Batch: "ancestor-check"}); !errors.Is(err, archive.ErrSpecialUseDestination) {
		t.Fatalf("apply=%v", err)
	}
	f.mustBeIn(id, "Archive")
}
