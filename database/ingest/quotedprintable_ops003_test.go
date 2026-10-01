package ingest

import (
	"bytes"
	"io"
	"math/rand"
	"mime/quotedprintable"
	"strings"
	"testing"
)

// TestQuotedPrintableMatchesStdlib pins decodeQuotedPrintable to
// mime/quotedprintable.Reader (OPS-003): for every input the stdlib decodes
// without error, the output must be byte for byte the same, so replacing the
// decoder changes nothing for a message that parsed before. The inputs are
// weighted towards the bytes the algorithm branches on.
func TestQuotedPrintableMatchesStdlib(t *testing.T) {
	const alphabet = "==========0123456789ABCDEFabcdefXYZ \t\t\r\r\n\n\n\x00\x1b\x7f\xe9\xc3.-"
	rng := rand.New(rand.NewSource(3))
	compared := 0
	for i := 0; i < 50000; i++ {
		b := make([]byte, rng.Intn(40))
		for j := range b {
			b[j] = alphabet[rng.Intn(len(alphabet))]
		}
		want, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(b)))
		got := decodeQuotedPrintable(b)
		if len(got) > len(b) {
			t.Fatalf("%q decoded to %d bytes, longer than its %d encoded bytes", b, len(got), len(b))
		}
		if err != nil {
			continue
		}
		compared++
		if !bytes.Equal(got, want) {
			t.Fatalf("%q: decoded %q, stdlib decoded %q", b, got, want)
		}
	}
	// The weighting must leave most inputs decodable, or this compares
	// nothing.
	if compared < 10000 {
		t.Fatalf("only %d of the generated inputs were decodable by the stdlib", compared)
	}
}

// TestQuotedPrintableLongLineDecodes is the "transfer decode: bufio: buffer
// full" regression (OPS-003). The stdlib reader reads each encoded line into a
// 4096-byte buffer and fails on any longer line, which HTML and bulk mail send
// all the time.
func TestQuotedPrintableLongLineDecodes(t *testing.T) {
	// ~4.8 KB on one line, with the first escape past the 4096-byte mark: the
	// stdlib reader then fails exactly as in production. (An '=' just before
	// the mark fails it too, as "invalid bytes after =".)
	line := "<p>" + strings.Repeat("plain words ", 400) + "caf=C3=A9</p>"
	raw := "From: news@ops003.invalid\r\n" +
		"Subject: long qp line\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		line + "\r\n"

	msg, err := New(DefaultLimits()).Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want := strings.Repeat("plain words ", 400) + "café</p>"; !strings.Contains(msg.HTMLBody, want) {
		t.Errorf("HTMLBody holds %d bytes, want the whole decoded line", len(msg.HTMLBody))
	}
	if len(msg.Defects) != 0 {
		t.Errorf("a decodable body was marked degraded: %q", msg.Defects)
	}
	if msg.BodyStructure.Size != int64(len(line)+2) || msg.BodyStructure.Encoding != "quoted-printable" {
		t.Errorf("structure = %+v, want the encoded size and encoding", msg.BodyStructure)
	}
}

// TestQuotedPrintablePassesMalformedInputThrough covers the inputs the stdlib
// reader refuses: the decoder shows them literally, as mail clients do.
func TestQuotedPrintablePassesMalformedInputThrough(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a==\n", "a="},                          // soft break after a stray '='
		{"a=\rb\n", "a=\rb\n"},                   // '=' before a lone CR
		{"=", ""},                                // a lone soft break at EOF
		{"esc\x1b$B text\n", "esc\x1b$B text\n"}, // ISO-2022-JP escape bytes
		{"del\x7f\n", "del\x7f\n"},               // DEL
		{"soft=\r \nnext\n", "softnext\n"},       // transport padding after a CR
	}
	for _, tc := range cases {
		if _, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(tc.in))); err == nil {
			t.Errorf("%q: the stdlib now decodes this; move it to the equivalence test", tc.in)
		}
		if got := string(decodeQuotedPrintable([]byte(tc.in))); got != tc.want {
			t.Errorf("%q decoded to %q, want %q", tc.in, got, tc.want)
		}
	}
}
