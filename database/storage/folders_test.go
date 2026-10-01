package storage

import (
	"reflect"
	"testing"
)

// TestFolderAncestors covers the hierarchy helper's edge cases directly. It
// moved here from epistula-imap (where it was ancestorNames, RO5X-008) when
// every folder writer started sharing one creation path (OPS-001).
func TestFolderAncestors(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"a", nil},
		{"a/b", []string{"a"}},
		{"a/b/c", []string{"a", "a/b"}},
		{"", nil},
		{"/leading", nil},       // empty segment yields no ancestor
		{"a//b", []string{"a"}}, // doubled delimiter
		{"trailing/", []string{"trailing"}},
		{"Sent Messages", nil}, // a space is not a delimiter
		{"oldmbox/mail/IN/Old/20020417", []string{
			"oldmbox", "oldmbox/mail", "oldmbox/mail/IN", "oldmbox/mail/IN/Old",
		}},
		{"Ärchiv/2026", []string{"Ärchiv"}}, // '/' never occurs inside a UTF-8 sequence
	} {
		if got := FolderAncestors(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("FolderAncestors(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestMissingAncestors pins the backfill's plan: every absent ancestor once,
// nothing that exists, and ancestors ordered before their descendants.
func TestMissingAncestors(t *testing.T) {
	got := missingAncestors([]string{
		"INBOX",
		"Sent",
		"Sent Messages",
		"Sent/2004/11-Nov",
		"Sent/2004/12-Dec",
		"oldmbox/mail/IN/Old/20020417",
		"oldmbox/mail/OUT/200205/alice/project",
		"webmail-archive/2010-and-before/2010-inbox",
	})
	want := []string{
		"Sent/2004",
		"oldmbox",
		"oldmbox/mail",
		"oldmbox/mail/IN",
		"oldmbox/mail/IN/Old",
		"oldmbox/mail/OUT",
		"oldmbox/mail/OUT/200205",
		"oldmbox/mail/OUT/200205/alice",
		"webmail-archive",
		"webmail-archive/2010-and-before",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("missingAncestors = %q\nwant              %q", got, want)
	}
	if got := missingAncestors([]string{"a", "a/b", "a/b/c"}); len(got) != 0 {
		t.Fatalf("a complete hierarchy needs nothing; got %q", got)
	}
}

// TestCanonicalSpecialUse pins the accepted set to the one the schema comment
// on folders.special_use enumerates, matched case-insensitively and returned
// in canonical spelling (OPS-002).
func TestCanonicalSpecialUse(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{`\Sent`, `\Sent`, true},
		{`\sent`, `\Sent`, true},
		{`\SENT`, `\Sent`, true},
		{`\Drafts`, `\Drafts`, true},
		{`\Trash`, `\Trash`, true},
		{`\Junk`, `\Junk`, true},
		{`\Archive`, `\Archive`, true},
		{`\All`, `\All`, true},
		{`\Flagged`, `\Flagged`, true},
		{`\important`, `\Important`, true},

		{`Sent`, ``, false}, // the backslash is part of the attribute
		{`\NotAThing`, ``, false},
		{`\Noselect`, ``, false}, // a LIST attribute, but not a special use
		{`\Sent `, ``, false},
		{``, ``, false},
	} {
		got, ok := CanonicalSpecialUse(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("CanonicalSpecialUse(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	for _, a := range SpecialUseAttributes() {
		if got, ok := CanonicalSpecialUse(a); !ok || got != a {
			t.Errorf("listed attribute %q is not its own canonical form", a)
		}
	}
}
