package imapsess

import (
	"bytes"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

// oneByteReader yields its data one byte per Read, forcing readHeaderSection to
// handle a separator that straddles chunk boundaries.
type oneByteReader struct {
	data []byte
	pos  int
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	p[0] = r.data[r.pos]
	r.pos++
	return 1, nil
}

// countingReader records how many bytes were actually read.
type countingReader struct {
	data []byte
	pos  int
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

// TestReadHeaderSectionMatchesSplit is the R-042 regression: the bounded header
// read must return bytes IDENTICAL to splitHeaderBody(fullRaw).header for every
// input shape, so BODY[HEADER] output is unchanged — including the trap where a
// bare "\n\n" precedes the real "\r\n\r\n".
func TestReadHeaderSectionMatchesSplit(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"crlf", "From: a\r\nSubject: b\r\n\r\nbody\r\nmore\r\n"},
		{"lf only", "From: a\nSubject: b\n\nbody\n"},
		{"lf-lf precedes crlf-crlf", "From: a\n\nX: y\r\n\r\nbody"},
		{"no separator", "From: a\r\nSubject: b\r\n"},
		{"empty body after sep", "From: a\r\n\r\n"},
		// OPS-003: the streamed read kept its own separator patterns and
		// drifted from the split. It missed a message that starts with its
		// empty line, and the empty line after a field ended by a bare LF.
		{"leading empty line", "\nbody\n\nmore"},
		{"leading crlf empty line", "\r\nbody\r\n\r\nmore"},
		{"lf field then crlf empty line", "From: a\nSubject: b\n\r\nbody\r\n\r\nmore"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(tc.raw)
			want, _ := splitHeaderBody(raw)

			got, err := readHeaderSection(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("readHeaderSection: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("bulk read: got %q, want %q", got, want)
			}

			// Same result when the separator straddles single-byte chunks.
			got2, err := readHeaderSection(&oneByteReader{data: raw})
			if err != nil {
				t.Fatalf("readHeaderSection (1-byte): %v", err)
			}
			if !bytes.Equal(got2, want) {
				t.Fatalf("1-byte read: got %q, want %q", got2, want)
			}
		})
	}
}

// TestReadHeaderSectionStopsEarly is the R-042 "no full-blob read" check: for a
// well-formed message with a huge body, readHeaderSection must consume only the
// header portion, not the whole stream.
func TestReadHeaderSectionStopsEarly(t *testing.T) {
	header := "From: a\r\nSubject: b\r\n\r\n"
	raw := []byte(header + strings.Repeat("x", 5_000_000))
	cr := &countingReader{data: raw}

	got, err := readHeaderSection(cr)
	if err != nil {
		t.Fatalf("readHeaderSection: %v", err)
	}
	if string(got) != header {
		t.Fatalf("header = %q, want %q", got, header)
	}
	if cr.pos > len(header)+64*1024 {
		t.Fatalf("read %d bytes for a %d-byte header — did not stop at the separator", cr.pos, len(header))
	}
}

// TestApplyPartialOverflowSafe is the R-013 regression: a wire-supplied
// Partial.Size near math.MaxInt64 must not overflow the end index and panic on
// a negative slice bound. Also covers offset-past-end and zero-size.
func TestApplyPartialOverflowSafe(t *testing.T) {
	p := []byte("abcdef")
	cases := []struct {
		name    string
		partial *imap.SectionPartial
		want    string
	}{
		{"overflow size from mid-offset", &imap.SectionPartial{Offset: 2, Size: math.MaxInt64}, "cdef"},
		{"overflow size from zero", &imap.SectionPartial{Offset: 0, Size: math.MaxInt64}, "abcdef"},
		{"offset past end", &imap.SectionPartial{Offset: 100, Size: 5}, ""},
		{"offset exactly at end", &imap.SectionPartial{Offset: 6, Size: 5}, ""},
		{"in-range size", &imap.SectionPartial{Offset: 1, Size: 2}, "bc"},
		{"zero size returns rest", &imap.SectionPartial{Offset: 2, Size: 0}, "cdef"},
		{"nil partial returns all", nil, "abcdef"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(applyPartial(p, tc.partial)) // must not panic
			if got != tc.want {
				t.Fatalf("applyPartial = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSplitHeaderBodyCRLF(t *testing.T) {
	raw := []byte("From: a\r\nSubject: b\r\n\r\nhello body\r\n")
	hdr, body := splitHeaderBody(raw)
	wantHdr := "From: a\r\nSubject: b\r\n\r\n"
	wantBody := "hello body\r\n"
	if string(hdr) != wantHdr {
		t.Errorf("hdr = %q, want %q", hdr, wantHdr)
	}
	if string(body) != wantBody {
		t.Errorf("body = %q, want %q", body, wantBody)
	}
}

func TestSplitHeaderBodyLFOnly(t *testing.T) {
	// Real-world mail sometimes uses bare LFs. We tolerate.
	raw := []byte("From: a\nSubject: b\n\nbody\n")
	hdr, body := splitHeaderBody(raw)
	if !strings.HasSuffix(string(hdr), "\n\n") {
		t.Errorf("hdr should end with blank-line separator: %q", hdr)
	}
	if string(body) != "body\n" {
		t.Errorf("body = %q, want %q", body, "body\n")
	}
}

func TestSplitHeaderBodyNoSeparator(t *testing.T) {
	raw := []byte("From: a\r\nSubject: b\r\n")
	hdr, body := splitHeaderBody(raw)
	if !bytes.Equal(hdr, raw) {
		t.Errorf("hdr = %q, want full raw", hdr)
	}
	if len(body) != 0 {
		t.Errorf("body = %q, want empty", body)
	}
}

func TestFilterHeaderFieldsInclude(t *testing.T) {
	header := []byte("From: a@x\r\nSubject: hi\r\nTo: b@x\r\nDate: now\r\n\r\n")
	out := filterHeaderFields(header, []string{"Subject", "Date"}, false)
	got := string(out)
	if !strings.Contains(got, "Subject: hi\r\n") {
		t.Errorf("missing Subject: %q", got)
	}
	if !strings.Contains(got, "Date: now\r\n") {
		t.Errorf("missing Date: %q", got)
	}
	if strings.Contains(got, "From:") || strings.Contains(got, "To:") {
		t.Errorf("From/To not filtered out: %q", got)
	}
	if !strings.HasSuffix(got, "\r\n\r\n") && !strings.HasSuffix(got, "\r\n") {
		t.Errorf("missing trailing blank line: %q", got)
	}
}

func TestFilterHeaderFieldsCaseInsensitive(t *testing.T) {
	header := []byte("Message-ID: <a>\r\nFrom: x\r\n\r\n")
	out := filterHeaderFields(header, []string{"message-id"}, false)
	// The MATCH is case-insensitive per RFC 5322; the OUTPUT is the sender's
	// own bytes, not a canonicalised spelling (RA6X-027). Expecting
	// "Message-Id" here was asserting the reconstruction the fix removes: a
	// client cannot verify a signature over headers the server re-spelled.
	if !strings.Contains(string(out), "Message-ID: <a>\r\n") {
		t.Errorf("case-insensitive match failed: %q", out)
	}
	if strings.Contains(string(out), "From:") {
		t.Errorf("unrequested field leaked: %q", out)
	}
}

func TestFilterHeaderFieldsNotExcludes(t *testing.T) {
	header := []byte("From: a@x\r\nSubject: hi\r\nTo: b@x\r\n\r\n")
	out := filterHeaderFields(header, []string{"From"}, true)
	got := string(out)
	if strings.Contains(got, "From:") {
		t.Errorf("From should be excluded: %q", got)
	}
	if !strings.Contains(got, "Subject: hi\r\n") || !strings.Contains(got, "To: b@x\r\n") {
		t.Errorf("non-excluded headers missing: %q", got)
	}
}

func TestFilterHeaderFieldsAbsentFieldIsNoop(t *testing.T) {
	header := []byte("From: a\r\n\r\n")
	out := filterHeaderFields(header, []string{"Subject"}, false)
	if string(out) != "\r\n" {
		t.Errorf("absent-field result = %q, want %q", out, "\r\n")
	}
}
