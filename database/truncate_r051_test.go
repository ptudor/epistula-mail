package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncateRuneSafe is the R-051 regression: log-tail truncation must cut on
// rune boundaries so a multibyte character straddling the limit is never split
// into an invalid UTF-8 sequence (envelope addresses are internationalizable).
func TestTruncateRuneSafe(t *testing.T) {
	// 10 'é' runes = 20 bytes. Cut at n=5 bytes would have split the 3rd 'é'.
	s := strings.Repeat("é", 10)
	got := truncate(s, 5)
	if !utf8.ValidString(got) {
		t.Fatalf("truncate produced invalid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated result %q lacks the ellipsis", got)
	}
	// At most n runes total (n-1 content runes + the ellipsis).
	if n := utf8.RuneCountInString(got); n > 5 {
		t.Errorf("result has %d runes, want <= 5", n)
	}

	// A string that fits in n runes is returned untouched even if its byte
	// length exceeds n (multibyte): 3 'é' = 6 bytes but only 3 runes.
	short := strings.Repeat("é", 3)
	if got := truncate(short, 5); got != short {
		t.Errorf("truncate(%q,5) = %q, want unchanged (fits in 5 runes)", short, got)
	}

	// Pure ASCII path is unchanged.
	if got := truncate("hello world", 8); got != "hello w…" {
		t.Errorf("ascii truncate = %q, want %q", got, "hello w…")
	}
}
