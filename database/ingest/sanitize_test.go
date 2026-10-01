package ingest

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeUTF8(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"clean ascii", "hello", "hello"},
		{"clean multibyte", "héllo — ok", "héllo — ok"},
		{"empty", "", ""},
		{"lone latin1 byte", "caf\xe9", "caf�"},
		// ToValidUTF8 replaces each contiguous invalid run with ONE U+FFFD.
		{"truncated multibyte", "ok\xe4\xb8", "ok�"},
		{"nul stripped", "a\x00b", "ab"},
		{"nul and invalid", "\x00\xff", "�"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeUTF8(tt.in)
			if got != tt.want {
				t.Errorf("SanitizeUTF8(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("SanitizeUTF8(%q) produced invalid UTF-8", tt.in)
			}
			if strings.ContainsRune(got, 0) {
				t.Errorf("SanitizeUTF8(%q) retained a NUL", tt.in)
			}
		})
	}
}

// TestParseEightBitHeaderSanitized covers the single most common poison in
// legacy archives: a raw latin-1 byte in a header with no RFC 2047
// encoded-word. DecodeHeader passes it through untouched with a nil error;
// the parser must still emit valid UTF-8 for every derived column.
func TestParseEightBitHeaderSanitized(t *testing.T) {
	raw := []byte("From: sender@sanitize.invalid\r\n" +
		"To: rcpt@sanitize.invalid\r\n" +
		"Subject: caf\xe9 menu\r\n" +
		"X-Weird: \xff\xfe\x00 bytes\r\n" +
		"Date: Mon, 15 Jun 2003 10:00:00 +0000\r\n" +
		"\r\n" +
		"Plain body.\r\n")

	msg, err := New(DefaultLimits()).Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !utf8.ValidString(msg.Subject) {
		t.Errorf("Subject is invalid UTF-8: %q", msg.Subject)
	}
	if want := "caf� menu"; msg.Subject != want {
		t.Errorf("Subject = %q, want %q", msg.Subject, want)
	}
	for k, vs := range msg.Headers {
		for _, v := range vs {
			if !utf8.ValidString(v) || strings.ContainsRune(v, 0) {
				t.Errorf("header %s value not sanitized: %q", k, v)
			}
		}
	}
}

// TestParseNULAndInvalidBodySanitized: NUL bytes and invalid sequences in a
// declared-UTF-8 text part must not reach TextBody/HTMLBody.
func TestParseNULAndInvalidBodySanitized(t *testing.T) {
	raw := []byte("From: sender@sanitize.invalid\r\n" +
		"Subject: body test\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"before\x00middle\xffafter\r\n")

	msg, err := New(DefaultLimits()).Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !utf8.ValidString(msg.TextBody) {
		t.Errorf("TextBody is invalid UTF-8: %q", msg.TextBody)
	}
	if strings.ContainsRune(msg.TextBody, 0) {
		t.Errorf("TextBody retained NUL: %q", msg.TextBody)
	}
	if !strings.Contains(msg.TextBody, "before") || !strings.Contains(msg.TextBody, "after") {
		t.Errorf("TextBody lost real content: %q", msg.TextBody)
	}
	// The raw bytes are untouched — disk archives keep the wire truth.
	if !strings.Contains(string(msg.Raw), "\x00") {
		t.Error("Raw was modified by sanitization")
	}
}

// TestParseMislabeledCharsetSanitized: a part that claims a charset whose
// decode fails (or passes bytes through) must still produce valid UTF-8.
func TestParseMislabeledCharsetSanitized(t *testing.T) {
	// Declared us-ascii but actually latin-1: decodeText passes through.
	raw := []byte("From: sender@sanitize.invalid\r\n" +
		"Subject: mislabeled\r\n" +
		"Content-Type: text/plain; charset=us-ascii\r\n" +
		"\r\n" +
		"r\xe9sum\xe9 attached\r\n")

	msg, err := New(DefaultLimits()).Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !utf8.ValidString(msg.TextBody) {
		t.Errorf("TextBody is invalid UTF-8: %q", msg.TextBody)
	}
	if !strings.Contains(msg.TextBody, "attached") {
		t.Errorf("TextBody lost real content: %q", msg.TextBody)
	}
}
