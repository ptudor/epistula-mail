package archive_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/archive"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

func TestDestinationFolder(t *testing.T) {
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	day := func(y int) *time.Time { d := time.Date(y, 3, 1, 0, 0, 0, 0, time.UTC); return &d }
	// Late on 31 December in UTC-8 is already the next year in UTC: the
	// sender's calendar date wins.
	nye := time.Date(2025, 1, 1, 5, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name      string
		annual    bool
		sentLocal *time.Time
		internal  time.Time
		want      string
	}{
		{"not annual", false, day(2020), nye, "A/B"},
		{"sender's date", true, day(2024), nye, "A/B/2024"},
		{"no sender date: arrival year", true, nil, nye, "A/B/2025"},
		{"implausible sender date: arrival year", true, day(1970), nye, "A/B/2025"},
		{"future sender date: arrival year", true, day(2031), nye, "A/B/2025"},
		{"next year is plausible", true, day(2027), nye, "A/B/2027"},
		{"no plausible year at all", true, day(1901), time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), "A/B"},
	} {
		if got := archive.DestinationFolder("A/B", c.annual, c.sentLocal, c.internal, now); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// TestAnnualCategoriesFileByYear: the reorganization and the live sorter put
// an annual category's mail in the same year folder, the year coming from the
// message's own date; a message already in a year folder is filed.
func TestAnnualCategoriesFileByYear(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := testContext(t)
	f := newFixture(t, ctx, db)
	mb := f.mailbox("annual")
	root := f.folder(mb, "Archive", `\Archive`)
	old := f.folder(mb, "Old Bank", "")
	f.categories(mb,
		storage.ArchiveCategory{Key: "finance/banking", Folder: "Archive/Finance/Banking"},
		storage.ArchiveCategory{Key: "finance/banking/acme-bank", Folder: "Archive/Finance/Banking/Acme Bank", Annual: true},
	)
	year2023 := f.folder(mb, "Archive/Finance/Banking/Acme Bank/2023", "")

	d := func(y int) *time.Time { t := time.Date(y, 6, 1, 0, 0, 0, 0, time.UTC); return &t }
	bySender := f.message(old, msgOpt{sentLocal: d(2019), internal: time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)})
	byArrival := f.message(old, msgOpt{internal: time.Date(2018, 5, 5, 0, 0, 0, 0, time.UTC)})
	broad := f.message(old, msgOpt{sentLocal: d(2019)})
	alreadyFiled := f.message(year2023, msgOpt{sentLocal: d(2023)})
	f.classify(bySender, "finance/banking/acme-bank", 0.9)
	f.classify(byArrival, "finance/banking/acme-bank", 0.9)
	f.classify(broad, "finance/banking", 0.9)
	f.classify(alreadyFiled, "finance/banking/acme-bank", 0.9)

	plan, err := archive.BuildPlan(ctx, db, mb, archive.DefaultRules(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Apply(ctx, db, plan, archive.ApplyOptions{Batch: "reorg-annual"}); err != nil {
		t.Fatal(err)
	}
	f.mustBeIn(bySender, "Archive/Finance/Banking/Acme Bank/2019")
	f.mustBeIn(byArrival, "Archive/Finance/Banking/Acme Bank/2018")
	f.mustBeIn(broad, "Archive/Finance/Banking") // not annual: no year folder
	f.mustBeIn(alreadyFiled, "Archive/Finance/Banking/Acme Bank/2023")

	// The live sorter agrees.
	live := f.message(root, msgOpt{sentLocal: d(2021)})
	f.classify(live, "finance/banking/acme-bank", 0.9)
	f.age(live, time.Hour)
	if _, err := archive.SortOnce(ctx, db, archive.SortOptions{SettleDelay: time.Minute, MinConfidence: 0.6}); err != nil {
		t.Fatal(err)
	}
	f.mustBeIn(live, "Archive/Finance/Banking/Acme Bank/2021")

	// A year folder that has been given a special-use role is refused as a
	// destination, by the sorter as by apply.
	trashed := f.message(root, msgOpt{sentLocal: d(2022)})
	f.classify(trashed, "finance/banking/acme-bank", 0.9)
	f.age(trashed, time.Hour)
	f.folder(mb, "Archive/Finance/Banking/Acme Bank/2022", `\Trash`)
	_, err = archive.SortOnce(ctx, db, archive.SortOptions{SettleDelay: time.Minute, MinConfidence: 0.6})
	if !errors.Is(err, archive.ErrSpecialUseDestination) {
		t.Fatalf("sort into a \\Trash year folder: err = %v", err)
	}
	f.mustBeIn(trashed, "Archive")
}
