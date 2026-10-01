package archive_test

import (
	"strings"
	"testing"

	"github.com/ptudor/epistula-mail/database/archive"
)

func TestParseTaxonomy(t *testing.T) {
	cats, err := archive.ParseTaxonomy(strings.NewReader(`
[[category]]
key = "finance/banking"
folder = "Archive/Finance/Banking"
description = "  Bank statements  "

[[category]]
key = "travel"
folder = "Archive/Travel"
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cats) != 2 || cats[0].Description != "Bank statements" || cats[1].Folder != "Archive/Travel" {
		t.Fatalf("cats = %+v", cats)
	}
	for _, bad := range []string{
		``,
		`[[category]]
key = "x"`,
		`[[category]]
key = "x"
folder = "Archive/X"
colour = "blue"`,
	} {
		if _, err := archive.ParseTaxonomy(strings.NewReader(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestParseRules(t *testing.T) {
	r, err := archive.ParseRules(strings.NewReader(`keep = ["Taxes"]`))
	if err != nil {
		t.Fatal(err)
	}
	if r.MinConfidence != 0.6 || r.InboxKeepDays != 30 || len(r.Keep) != 1 {
		t.Fatalf("defaults not kept: %+v", r)
	}
	for _, bad := range []string{
		`min_confidence = 1.5`,
		`inbox_keep_days = -1`,
		`keep = ["a//b"]`,
		`[folders]
"Receipts" = "Not A Key"`,
		`unknown = 1`,
	} {
		if _, err := archive.ParseRules(strings.NewReader(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
