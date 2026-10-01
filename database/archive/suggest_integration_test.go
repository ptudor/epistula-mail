package archive_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/ptudor/epistula-mail/database/archive"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

func TestSuggestHelpers(t *testing.T) {
	for from, want := range map[string]string{
		"alerts@alerts.initech.example": "initech.example",
		"no-reply@Initech.EXAMPLE":      "initech.example",
		"news@example.co.uk":            "example.co.uk",
		"<x@mail.example.org.>":         "example.org",
		"not an address":                "",
		"x@[192.0.2.1]":                 "",
	} {
		if got := archive.SenderOrganization(from); got != want {
			t.Errorf("SenderOrganization(%q) = %q, want %q", from, got, want)
		}
	}
	for in, want := range map[string]string{
		"acmeshop":              "acmeshop",
		"Acme Bank":             "acme-bank",
		"--a..b--":              "a-b",
		"日本":                    "",
		strings.Repeat("a", 50): strings.Repeat("a", 40),
	} {
		if got := archive.Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
	if got := archive.SafeLine("Hi\x1b[31m red\r\ninjected", 100); got != "Hi [31m red injected" {
		t.Errorf("SafeLine kept control characters: %q", got)
	}
	if !archive.IsOtherKey("a/b/other") || !archive.IsOtherKey("other") || archive.IsOtherKey("brother") || archive.IsOtherKey("a/others") {
		t.Error("IsOtherKey misclassifies")
	}
}

// TestSuggestProposesSiblings: recurring senders and themes in the …/other
// buckets and among unsure classifications become proposals beside the
// bucket's key, collisions are flagged, and sender text reaches the report
// without its control characters.
func TestSuggestProposesSiblings(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := testContext(t)
	f := newFixture(t, ctx, db)
	mb := f.mailbox("suggest")
	root := f.folder(mb, "Archive", `\Archive`)
	f.categories(mb,
		storage.ArchiveCategory{Key: "shopping/stores/other", Folder: "Archive/Shopping/Stores/Other", Annual: true},
		storage.ArchiveCategory{Key: "shopping/stores/existing", Folder: "Archive/Shopping/Stores/Existing", Annual: true},
		storage.ArchiveCategory{Key: "finance/banking/initech", Folder: "Archive/Finance/Banking/Initech"},
		storage.ArchiveCategory{Key: "personal/other", Folder: "Archive/Personal/Other"},
	)
	add := func(n int, from, category string, conf float32, tags []string, subject string) {
		if tags == nil {
			tags = []string{}
		}
		for i := 0; i < n; i++ {
			id := f.message(root, msgOpt{})
			if _, err := db.Pool().Exec(ctx, `UPDATE messages SET from_addr = $2, subject = $3 WHERE id = $1`,
				id, from, fmt.Sprintf("%s %d", subject, i)); err != nil {
				t.Fatal(err)
			}
			f.classify(id, category, conf)
			if _, err := db.Pool().Exec(ctx,
				`INSERT INTO message_annotations (message_id, model, tags) VALUES ($1, 'test-model', $2)`, id, tags); err != nil {
				t.Fatal(err)
			}
		}
	}
	add(12, "news@acmeshop.example", "shopping/stores/other", 0.9, []string{"store", "sale"}, "Sale \x1b[2Jtoday")
	add(10, "alerts@alerts.acmeshop.example", "shopping/stores/other", 0.8, []string{"store"}, "Last day")
	add(20, "hi@existing.example", "shopping/stores/other", 0.9, []string{"store"}, "Existing")
	add(2, "one@rare.example", "shopping/stores/other", 0.9, nil, "Rare")
	for i := 0; i < 21; i++ { // a theme, not a sender
		add(1, fmt.Sprintf("cousin%d@family%d.example", i, i), "personal/other", 0.9, []string{"genealogy"}, "Family tree")
	}
	add(20, "offers@globex.example", "finance/banking/initech", 0.3, []string{"credit-card"}, "Pre-approved")
	add(30, "alerts@initech.example", "finance/banking/initech", 0.95, nil, "Statement") // confident: not a bucket

	opts := archive.DefaultSuggestOptions()
	opts.MinShare = 0.5    // count decides in this test
	opts.MaxTagShare = 0.5 // a small mailbox: keep its themes (see TestSuggestDropsGenericAndSenderTags)
	report, err := archive.Suggest(ctx, db, mb, opts)
	if err != nil {
		t.Fatalf("Suggest: %v", err)
	}
	find := func(bucket, label string, byTag bool) (archive.Bucket, archive.Cluster) {
		t.Helper()
		for _, b := range report.Buckets {
			if b.Key != bucket {
				continue
			}
			list := b.BySender
			if byTag {
				list = b.ByTag
			}
			for _, c := range list {
				if c.Label == label {
					return b, c
				}
			}
		}
		t.Fatalf("no cluster %q in bucket %q: %+v", label, bucket, report.Buckets)
		return archive.Bucket{}, archive.Cluster{}
	}
	b, acmeshop := find("shopping/stores/other", "acmeshop.example", false)
	if b.Count != 44 || acmeshop.Count != 22 {
		t.Fatalf("bucket %d, acmeshop.example %d; want 44 and 22 (subdomains merged)", b.Count, acmeshop.Count)
	}
	if p := acmeshop.Proposal; p.Key != "shopping/stores/acmeshop" || p.Folder != "Archive/Shopping/Stores/Acmeshop" ||
		!p.Annual || p.Conflict != "" {
		t.Fatalf("acmeshop proposal = %+v", p)
	}
	if len(acmeshop.Examples) != 3 || strings.ContainsRune(strings.Join(acmeshop.Examples, ""), 0x1b) {
		t.Fatalf("examples = %q; want 3, without control characters", acmeshop.Examples)
	}
	if acmeshop.TopTags[0] != "store" {
		t.Fatalf("top tags = %v", acmeshop.TopTags)
	}
	if _, existing := find("shopping/stores/other", "existing.example", false); existing.Proposal.Conflict != "key exists" {
		t.Fatalf("existing.example proposal = %+v; want a key collision flagged", existing.Proposal)
	}
	for _, c := range b.BySender {
		if c.Label == "rare.example" {
			t.Fatal("a 2-message sender was reported")
		}
	}
	if _, g := find("personal/other", "genealogy", true); g.Count != 21 || g.Proposal.Key != "personal/genealogy" {
		t.Fatalf("genealogy theme = %+v", g)
	}
	lb, globex := find("finance/banking/initech", "globex.example", false)
	if !lb.LowConfidence || lb.Count != 20 || globex.Proposal.Key != "finance/banking/globex" {
		t.Fatalf("low-confidence bucket %+v, cluster %+v", lb, globex)
	}

	var text, toml bytes.Buffer
	if err := report.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	if err := report.WriteTOML(&toml); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(text.String(), 0x1b) || strings.ContainsRune(toml.String(), 0x1b) {
		t.Fatal("the report carries a terminal escape from a subject")
	}
	if !strings.Contains(toml.String(), "# [[category]]\n# key         = \"shopping/stores/acmeshop\"") {
		t.Fatalf("TOML lacks the acmeshop stanza:\n%s", toml.String())
	}
	if strings.Contains(toml.String(), "\n[[category]]") {
		t.Fatal("the TOML output has an uncommented, importable stanza")
	}
	if !strings.Contains(toml.String(), "not proposed: key exists (shopping/stores/existing)") {
		t.Fatalf("TOML does not explain the collision:\n%s", toml.String())
	}
}

// TestSuggestDropsGenericAndSenderTags: a tag most of the mailbox carries
// ("newsletter") is not a theme, and a tag that is one sender under another
// name ("tidepool" = tidepool.example) is left to the sender cluster; a topical
// tag spread over many senders ("soccer") survives both.
func TestSuggestDropsGenericAndSenderTags(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := testContext(t)
	f := newFixture(t, ctx, db)
	mb := f.mailbox("themes")
	root := f.folder(mb, "Archive", `\Archive`)
	f.categories(mb,
		storage.ArchiveCategory{Key: "community/other", Folder: "Archive/Community/Other"},
		storage.ArchiveCategory{Key: "finance/banking", Folder: "Archive/Finance/Banking"},
	)
	// A thousand confidently classified newsletters as the mailbox's
	// background, in one statement.
	if _, err := db.Pool().Exec(ctx, `
		WITH ins AS (
			INSERT INTO messages (folder_id, uid, raw_sha256, raw_size, internal_date, from_addr)
			SELECT $1, 1000 + g, sha256(('bg' || g)::bytea), 10, now(), 'news@bank' || g || '.com'
			  FROM generate_series(1, 1000) g
			RETURNING id),
		cls AS (
			INSERT INTO message_classifications (message_id, category, confidence, model)
			SELECT id, 'finance/banking', 0.95, 'test-model' FROM ins)
		INSERT INTO message_annotations (message_id, model, tags)
		SELECT id, 'test-model', '{newsletter}' FROM ins`, root); err != nil {
		t.Fatal(err)
	}
	add := func(from string, tags []string) {
		id := f.message(root, msgOpt{})
		if _, err := db.Pool().Exec(ctx, `UPDATE messages SET from_addr = $2 WHERE id = $1`, id, from); err != nil {
			t.Fatal(err)
		}
		f.classify(id, "community/other", 0.9)
		if _, err := db.Pool().Exec(ctx,
			`INSERT INTO message_annotations (message_id, model, tags) VALUES ($1, 'test-model', $2)`, id, tags); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 25; i++ {
		add("news@tidepool.example", []string{"tidepool", "Newsletter"})
		add(fmt.Sprintf("club@league%d.org", i), []string{"soccer", "newsletter"})
	}

	report, err := archive.Suggest(ctx, db, mb, archive.DefaultSuggestOptions())
	if err != nil {
		t.Fatal(err)
	}
	var themes []string
	var senders []string
	for _, b := range report.Buckets {
		if b.Key != "community/other" {
			continue
		}
		for _, c := range b.ByTag {
			themes = append(themes, c.Label)
		}
		for _, c := range b.BySender {
			senders = append(senders, c.Label)
		}
	}
	if strings.Join(themes, ",") != "soccer" {
		t.Fatalf("themes = %v; want only soccer (newsletter is generic, tidepool is a sender)", themes)
	}
	if strings.Join(senders, ",") != "tidepool.example" {
		t.Fatalf("senders = %v", senders)
	}
}
