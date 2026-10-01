package ingest

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
)

// TestRawSplitMatchesMultipartReader pins the multipart walk's switch from
// mime/multipart to VisitRawMultipart (OPS-003) to "nothing changes for mail
// that parsed before": for every generated body mime/multipart reads without
// error, the raw split must yield the same parts, with the same header values
// and byte-identical bodies.
//
// Line endings are consistent within each generated body. With mixed ones
// mime/multipart either fails (and nothing was stored) or, after an LF first
// delimiter, keeps the CR of a later CRLF delimiter in the part before it. The
// raw split drops that CR, as the IMAP reader always did when serving the
// part, so there the change is a correction.
func TestRawSplitMatchesMultipartReader(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	pick := func(xs ...string) string { return xs[rng.Intn(len(xs))] }
	compared := 0
	for i := 0; i < 3000; i++ {
		nl := pick("\r\n", "\n")
		var b strings.Builder
		for j := rng.Intn(3); j > 0; j-- {
			b.WriteString(pick("preamble", "--bb", "-- b", "") + nl)
		}
		parts := rng.Intn(4)
		for p := 0; p < parts; p++ {
			b.WriteString("--b" + pick("", " ", "\t ") + nl)
			for h := rng.Intn(3); h > 0; h-- {
				b.WriteString(pick("Content-Type: text/plain; charset=utf-8", "X-Note: folded", "Content-ID: <p@x.invalid>") + nl)
				if rng.Intn(4) == 0 {
					b.WriteString("\tcontinued" + nl)
				}
			}
			b.WriteString(nl)
			for l := rng.Intn(4); l > 0; l-- {
				b.WriteString(pick("text", "", "--bX", "--b-", "- -b", "--", "  indented") + nl)
			}
			if rng.Intn(3) == 0 {
				b.WriteString("no line break at the end")
				b.WriteString(nl)
			}
		}
		b.WriteString("--b--" + pick("", " ") + pick(nl, nl+"epilogue"+nl, ""))
		body := []byte(b.String())

		type part struct {
			ct   string
			body string
		}
		var want []part
		mr := multipart.NewReader(bytes.NewReader(body), "b")
		stdlibOK := true
		for {
			p, err := mr.NextRawPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				stdlibOK = false
				break
			}
			data, err := io.ReadAll(p)
			if err != nil {
				stdlibOK = false
				break
			}
			want = append(want, part{p.Header.Get("Content-Type"), string(data)})
		}
		if !stdlibOK {
			continue
		}
		compared++

		var got []part
		if err := VisitRawMultipart(body, "b", func(span []byte) error {
			header, partBody := SplitHeaderBody(span)
			m, err := mail.ReadMessage(bytes.NewReader(header))
			ct := ""
			switch {
			case err == nil:
				ct = m.Header.Get("Content-Type")
			case !errors.Is(err, io.EOF):
				return err
			}
			got = append(got, part{ct, string(partBody)})
			return nil
		}); err != nil {
			t.Fatalf("%q: raw split: %v", body, err)
		}
		if len(got) != len(want) {
			t.Fatalf("%q: raw split found %d parts, mime/multipart %d", body, len(got), len(want))
		}
		for k := range want {
			if got[k] != want[k] {
				t.Fatalf("%q part %d: raw split %+v, mime/multipart %+v", body, k+1, got[k], want[k])
			}
		}
	}
	if compared < 1000 {
		t.Fatalf("only %d generated bodies were readable by mime/multipart", compared)
	}
}

// TestHeaderSeparatorEndRule pins the one definition of where a header section
// ends (OPS-003), which ingest and the IMAP reader share.
func TestHeaderSeparatorEndRule(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"A: 1\r\n\r\nbody", 8},               // CRLF, as before
		{"A: 1\n\nbody", 6},                   // LF, as before
		{"A: 1\r\n\nbody", 7},                 // CRLF field, LF empty line, as before
		{"A: 1\n\r\nbody", 7},                 // LF field, CRLF empty line: new
		{"A: 1\n\r\nbody\r\n\r\nmore", 7},     // ...and not the later CRLF blank line
		{"\nbody\n\nmore", 1},                 // no fields: the empty line is first
		{"\r\nbody\r\n\r\nmore", 2},           // the same with CRLF
		{"A: 1\nB: 2\n", -1},                  // header-only
		{"A: 1\r\n \r\nB: 2\r\n\r\nbody", 17}, // a whitespace line is a continuation
		{"A: 1\r\r\n\r\nbody", 9},             // "\r\r\n" is not empty; the next line is
		{"", -1},
	}
	for _, tc := range cases {
		if got := HeaderSeparatorEnd([]byte(tc.raw), 0); got != tc.want {
			t.Errorf("HeaderSeparatorEnd(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

// TestHeaderSeparatorEndResumes checks that a streaming caller, resuming the
// scan after every chunk, finds exactly the separator a scan of the whole
// input finds, whatever the chunk size.
func TestHeaderSeparatorEndResumes(t *testing.T) {
	inputs := []string{
		"From: a\r\nSubject: b\r\n\r\nbody\r\n",
		"From: a\nSubject: b\n\nbody\n\nmore",
		"From: a\nSubject: b\n\r\nbody\r\n\r\nmore",
		"\r\nno fields\r\n\r\nlater",
		"\nno fields\n\nlater",
		"\rX: y\n\nbody",
		"From: a\r\nSubject: b\r\n",
		"From: a\r\n\tfolded\r\n \r\nX: y\r\n\r\nbody",
	}
	for _, in := range inputs {
		b := []byte(in)
		want := HeaderSeparatorEnd(b, 0)
		for chunk := 1; chunk <= 5; chunk++ {
			got, scanned := -1, 0
			for n := min(chunk, len(b)); ; n = min(n+chunk, len(b)) {
				if got = HeaderSeparatorEnd(b[:n], scanned); got >= 0 || n == len(b) {
					break
				}
				scanned = n
			}
			if got != want {
				t.Errorf("%q in %d-byte chunks: %d, want %d", in, chunk, got, want)
			}
		}
	}
}
