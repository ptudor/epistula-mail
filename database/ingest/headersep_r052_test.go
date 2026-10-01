package ingest

import (
	"errors"
	"strings"
	"testing"
)

// TestEarlierSeparator covers the pure helper R-052 relies on: pick the smaller
// non-negative offset, -1 only when both are absent.
func TestEarlierSeparator(t *testing.T) {
	cases := []struct{ a, b, want int }{
		{10, 20, 10},
		{20, 10, 10},
		{-1, 15, 15},
		{15, -1, 15},
		{-1, -1, -1},
		{7, 7, 7},
	}
	for _, c := range cases {
		if got := earlierSeparator(c.a, c.b); got != c.want {
			t.Errorf("earlierSeparator(%d,%d) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// TestHeaderSectionLFHeadersWithCRLFInBody is the R-052 regression: a
// Postfix-style LF-terminated message whose body embeds a literal "\r\n\r\n"
// past the header-section limit must parse — the real header boundary is the
// earlier "\n\n". The old CRLF-first scan placed `end` deep in the body and
// falsely tripped ErrHeadersTooBig (EX_DATAERR bounce of a parseable message).
func TestHeaderSectionLFHeadersWithCRLFInBody(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxHeaderSectionBytes = 200 // tiny, so a body-placed boundary would trip it

	head := "From: a@x.invalid\nTo: b@x.invalid\nSubject: hello\n\n"
	body := strings.Repeat("x", 400) + "\r\n\r\n" + "trailing body\n"
	raw := []byte(head + body)

	p := New(lim)
	msg, err := p.Parse(raw)
	if err != nil {
		t.Fatalf("LF-headers message with body CRLF-blank-line must parse, got: %v", err)
	}
	if msg.Subject != "hello" {
		t.Errorf("Subject = %q, want %q (header boundary picked wrong)", msg.Subject, "hello")
	}
}

// TestHeaderSectionCRLFUnchanged: a genuinely oversized CRLF header section
// still trips ErrHeadersTooBig — the earlier-separator change must not mask a
// real limit violation, and no "\n\n" appears inside a pure-CRLF header block.
func TestHeaderSectionCRLFUnchanged(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxHeaderSectionBytes = 200

	var b strings.Builder
	b.WriteString("From: a@x.invalid\r\n")
	for i := 0; i < 30; i++ {
		b.WriteString("X-Filler: ")
		b.WriteString(strings.Repeat("z", 20))
		b.WriteString("\r\n")
	}
	b.WriteString("\r\nbody\r\n")

	p := New(lim)
	if _, err := p.Parse([]byte(b.String())); !errors.Is(err, ErrHeadersTooBig) {
		t.Errorf("oversized CRLF headers: got %v, want ErrHeadersTooBig", err)
	}
}
