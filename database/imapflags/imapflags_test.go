package imapflags

import "testing"

func TestCanonical(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// System flags canonicalize from any case.
		{`\Seen`, `\Seen`},
		{`\SEEN`, `\Seen`},
		{`\seen`, `\Seen`},
		{`\SeEn`, `\Seen`},
		{`\ANSWERED`, `\Answered`},
		{`\flagged`, `\Flagged`},
		{`\DELETED`, `\Deleted`},
		{`\draft`, `\Draft`},

		// Keywords are case-INSENSITIVE too (RFC 9051 §2.3.2, RFC 9007 §1.2),
		// so every spelling folds to ONE stored value — otherwise `$Junk` and
		// `$junk` are two flags, a client cannot reliably clear or find one
		// written in another case, and a message can carry both (RA6X-028).
		//
		// Well-known keywords keep their conventional capitalisation, because
		// clients render keywords verbatim.
		{`$Forwarded`, `$Forwarded`},
		{`$forwarded`, `$Forwarded`},
		{`$Junk`, `$Junk`},
		{`$JUNK`, `$Junk`},
		{`$notjunk`, `$NotJunk`},

		// Anything else gets a deterministic lowercase identity.
		{`NonJunk`, `nonjunk`},
		{`$Label1`, `$label1`},
		{`$label1`, `$label1`},

		// Not settable system flags, but they have conventional spellings.
		{`\recent`, `\Recent`},
		{`\Recent`, `\Recent`},

		// An unrecognised backslash flag still gets one identity.
		{`\Unknown`, `\unknown`},
		{`\unknown`, `\unknown`},

		{``, ``},
	} {
		if got := Canonical(tc.in); got != tc.want {
			t.Errorf("Canonical(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIsSystem(t *testing.T) {
	for _, s := range []string{`\Seen`, `\SEEN`, `\answered`, `\Flagged`, `\Deleted`, `\Draft`} {
		if !IsSystem(s) {
			t.Errorf("IsSystem(%q) = false, want true", s)
		}
	}
	for _, s := range []string{`$Forwarded`, `\Recent`, `Seen`, ``, `\Se en`} {
		if IsSystem(s) {
			t.Errorf("IsSystem(%q) = true, want false", s)
		}
	}
}

func TestValid(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{`\Seen`, true},
		{`\SEEN`, true},
		{`$Forwarded`, true},
		{`NonJunk`, true},
		{`seen`, true}, // a plausible keyword, even if it matches nothing

		{``, false},
		{` `, false},
		{"has space", false},
		{"tab\there", false},
		{"nul\x00byte", false},
		{"newline\nhere", false},
		{string(make([]byte, 65)), false}, // over maxKeywordBytes
	} {
		if got := Valid(tc.in); got != tc.want {
			t.Errorf("Valid(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestCanonicalSlice(t *testing.T) {
	in := []string{`\SEEN`, `$Forwarded`, `\draft`}
	got := CanonicalSlice(in)
	want := []string{`\Seen`, `$Forwarded`, `\Draft`}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// The input must not be mutated in place.
	if in[0] != `\SEEN` {
		t.Errorf("CanonicalSlice mutated its input: %q", in[0])
	}
}
