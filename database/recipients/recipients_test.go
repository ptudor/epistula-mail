package recipients

import (
	"errors"
	"testing"
)

func TestSplitAddress(t *testing.T) {
	cases := []struct {
		in     string
		local  string
		domain string
		err    error
	}{
		{"alice@example.invalid", "alice", "example.invalid", nil},
		{"Alice@Example.INVALID", "alice", "example.invalid", nil},
		{"  bob@example.invalid  ", "bob", "example.invalid", nil},
		{"user@host.example.invalid.", "user", "host.example.invalid", nil},
		{"first.last+tag@domain.invalid", "first.last+tag", "domain.invalid", nil},
		{"", "", "", ErrInvalidAddress},
		{"noatsign", "", "", ErrInvalidAddress},
		{"@nodomain.invalid", "", "", ErrInvalidAddress},
		{"nolocal@", "", "", ErrInvalidAddress},
		{"local@", "", "", ErrInvalidAddress},
	}
	for _, c := range cases {
		l, d, err := SplitAddress(c.in)
		if !errors.Is(err, c.err) {
			t.Errorf("SplitAddress(%q): err = %v, want %v", c.in, err, c.err)
		}
		if l != c.local {
			t.Errorf("SplitAddress(%q): local = %q, want %q", c.in, l, c.local)
		}
		if d != c.domain {
			t.Errorf("SplitAddress(%q): domain = %q, want %q", c.in, d, c.domain)
		}
	}
}

func TestSplitAddressNormalizesUnicode(t *testing.T) {
	// café in NFD vs NFC. NFC = café (single codepoint é).
	nfd := "u" + "́" + "@dom.invalid" // ú in NFD
	local, _, err := SplitAddress(nfd)
	if err != nil {
		t.Fatalf("SplitAddress: %v", err)
	}
	// Expect NFC: "ú" as one codepoint.
	want := "ú"
	if local != want {
		t.Errorf("local = %q (codepoints %v), want %q", local, []rune(local), want)
	}
}
