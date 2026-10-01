package ingest

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func mustParse(t *testing.T, raw []byte) *Message {
	t.Helper()
	p := New(DefaultLimits())
	m, err := p.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return m
}

const plainTextMsg = "From: alice@example.invalid\r\n" +
	"To: bob@example.invalid\r\n" +
	"Subject: Hello\r\n" +
	"Date: Mon, 17 May 2026 12:00:00 +0000\r\n" +
	"Message-ID: <plain-1@example.invalid>\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Hello, world.\r\n"

func TestPlainTextMessage(t *testing.T) {
	m := mustParse(t, []byte(plainTextMsg))
	if m.Subject != "Hello" {
		t.Errorf("Subject = %q", m.Subject)
	}
	if m.MessageID != "plain-1@example.invalid" {
		t.Errorf("MessageID = %q", m.MessageID)
	}
	if m.From != "alice@example.invalid" {
		t.Errorf("From = %q", m.From)
	}
	if len(m.To) != 1 || m.To[0] != "bob@example.invalid" {
		t.Errorf("To = %v", m.To)
	}
	if !strings.Contains(m.TextBody, "Hello, world.") {
		t.Errorf("TextBody = %q", m.TextBody)
	}
	if m.HTMLBody != "" {
		t.Errorf("HTMLBody = %q (expected empty)", m.HTMLBody)
	}
	if m.SentDate.IsZero() {
		t.Error("SentDate not parsed")
	}
	if m.SHA256Hex == "" || len(m.SHA256Hex) != 64 {
		t.Errorf("SHA256Hex = %q", m.SHA256Hex)
	}
	sum := sha256.Sum256([]byte(plainTextMsg))
	if m.SHA256Hex != hex.EncodeToString(sum[:]) {
		t.Errorf("SHA256 mismatch")
	}
	if m.BodyStructure.Type != "text" || m.BodyStructure.Subtype != "plain" {
		t.Errorf("BodyStructure: %s/%s", m.BodyStructure.Type, m.BodyStructure.Subtype)
	}
	if len(m.Attachments) != 0 {
		t.Errorf("unexpected attachments: %d", len(m.Attachments))
	}
}

func TestEncodedWordSubject(t *testing.T) {
	raw := "From: a@example.invalid\r\n" +
		"To: b@example.invalid\r\n" +
		"Subject: =?utf-8?B?VGVzdCBlbW9qaSDwn5iA?=\r\n" +
		"Content-Type: text/plain\r\n\r\nbody\r\n"
	m := mustParse(t, []byte(raw))
	if !strings.Contains(m.Subject, "Test emoji") || !strings.Contains(m.Subject, "\U0001F600") {
		t.Errorf("encoded-word not decoded: %q", m.Subject)
	}
}

func TestLatin1EncodedWord(t *testing.T) {
	raw := "From: a@example.invalid\r\n" +
		"To: b@example.invalid\r\n" +
		"Subject: =?iso-8859-1?Q?caf=E9?=\r\n" +
		"Content-Type: text/plain\r\n\r\nbody\r\n"
	m := mustParse(t, []byte(raw))
	if !strings.Contains(m.Subject, "café") {
		t.Errorf("latin-1 encoded-word: %q", m.Subject)
	}
}

func TestMultipartAlternative(t *testing.T) {
	raw := "From: a@example.invalid\r\n" +
		"To: b@example.invalid\r\n" +
		"Subject: multipart\r\n" +
		"Content-Type: multipart/alternative; boundary=ABC\r\n\r\n" +
		"--ABC\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"plain text body\r\n" +
		"--ABC\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n\r\n" +
		"<p>html body</p>\r\n" +
		"--ABC--\r\n"
	m := mustParse(t, []byte(raw))
	if !strings.Contains(m.TextBody, "plain text body") {
		t.Errorf("TextBody = %q", m.TextBody)
	}
	if !strings.Contains(m.HTMLBody, "<p>html body</p>") {
		t.Errorf("HTMLBody = %q", m.HTMLBody)
	}
	if m.BodyStructure.Type != "multipart" || m.BodyStructure.Subtype != "alternative" {
		t.Errorf("top BodyStructure: %s/%s", m.BodyStructure.Type, m.BodyStructure.Subtype)
	}
	if len(m.BodyStructure.Parts) != 2 {
		t.Errorf("BodyStructure.Parts = %d", len(m.BodyStructure.Parts))
	}
}

func TestMultipartMixedWithAttachment(t *testing.T) {
	attData := []byte("\x89PNG\r\n\x1a\n...fake png bytes...")
	b64 := base64.StdEncoding.EncodeToString(attData)
	raw := "From: a@example.invalid\r\n" +
		"To: b@example.invalid\r\n" +
		"Subject: with attachment\r\n" +
		"Content-Type: multipart/mixed; boundary=XYZ\r\n\r\n" +
		"--XYZ\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"see attached\r\n" +
		"--XYZ\r\n" +
		"Content-Type: image/png; name=cat.png\r\n" +
		"Content-Disposition: attachment; filename=cat.png\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		b64 + "\r\n" +
		"--XYZ--\r\n"
	m := mustParse(t, []byte(raw))
	if !strings.Contains(m.TextBody, "see attached") {
		t.Errorf("TextBody = %q", m.TextBody)
	}
	if len(m.Attachments) != 1 {
		t.Fatalf("Attachments = %d, want 1", len(m.Attachments))
	}
	att := m.Attachments[0]
	if att.ContentType != "image/png" {
		t.Errorf("ContentType = %q", att.ContentType)
	}
	if att.Filename != "cat.png" {
		t.Errorf("Filename = %q", att.Filename)
	}
	if att.Disposition != "attachment" {
		t.Errorf("Disposition = %q", att.Disposition)
	}
	if !bytes.Equal(att.Data, attData) {
		t.Errorf("base64 round-trip mismatch")
	}
	if att.PartNumber != "2" {
		t.Errorf("PartNumber = %q", att.PartNumber)
	}
}

func TestQuotedPrintableBody(t *testing.T) {
	raw := "From: a@example.invalid\r\n" +
		"To: b@example.invalid\r\n" +
		"Subject: qp\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"caf=C3=A9\r\n"
	m := mustParse(t, []byte(raw))
	if !strings.Contains(m.TextBody, "café") {
		t.Errorf("TextBody = %q", m.TextBody)
	}
}

func TestNonUTF8TextBody(t *testing.T) {
	// iso-8859-1 "café" = c a f \xe9
	raw := []byte("From: a@example.invalid\r\n" +
		"To: b@example.invalid\r\n" +
		"Subject: latin1\r\n" +
		"Content-Type: text/plain; charset=iso-8859-1\r\n\r\n" +
		"caf\xe9\r\n")
	m := mustParse(t, raw)
	if !strings.Contains(m.TextBody, "café") {
		t.Errorf("TextBody = %q", m.TextBody)
	}
}

func TestNestedMultipart(t *testing.T) {
	raw := "From: a@example.invalid\r\n" +
		"To: b@example.invalid\r\n" +
		"Subject: nested\r\n" +
		"Content-Type: multipart/mixed; boundary=OUTER\r\n\r\n" +
		"--OUTER\r\n" +
		"Content-Type: multipart/alternative; boundary=INNER\r\n\r\n" +
		"--INNER\r\n" +
		"Content-Type: text/plain\r\n\r\n" +
		"plain\r\n" +
		"--INNER\r\n" +
		"Content-Type: text/html\r\n\r\n" +
		"<p>html</p>\r\n" +
		"--INNER--\r\n" +
		"--OUTER--\r\n"
	m := mustParse(t, []byte(raw))
	if !strings.Contains(m.TextBody, "plain") {
		t.Errorf("TextBody = %q", m.TextBody)
	}
	if !strings.Contains(m.HTMLBody, "<p>html</p>") {
		t.Errorf("HTMLBody = %q", m.HTMLBody)
	}
}

func TestNestedMultipartAttachmentPartNumber(t *testing.T) {
	attData := []byte("report bytes")
	b64 := base64.StdEncoding.EncodeToString(attData)
	raw := "From: a@example.invalid\r\n" +
		"To: b@example.invalid\r\n" +
		"Subject: nested attachment\r\n" +
		"Content-Type: multipart/mixed; boundary=OUTER\r\n\r\n" +
		"--OUTER\r\n" +
		"Content-Type: multipart/related; boundary=INNER\r\n\r\n" +
		"--INNER\r\n" +
		"Content-Type: text/plain\r\n\r\n" +
		"body\r\n" +
		"--INNER\r\n" +
		"Content-Type: application/pdf; name=report.pdf\r\n" +
		"Content-Disposition: attachment; filename=report.pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		b64 + "\r\n" +
		"--INNER--\r\n" +
		"--OUTER--\r\n"
	m := mustParse(t, []byte(raw))
	if len(m.Attachments) != 1 {
		t.Fatalf("Attachments = %d, want 1", len(m.Attachments))
	}
	if m.Attachments[0].PartNumber != "1.2" {
		t.Errorf("PartNumber = %q, want 1.2", m.Attachments[0].PartNumber)
	}
}

func TestHTMLOnlyGetsTextProjection(t *testing.T) {
	raw := "From: a@example.invalid\r\n" +
		"To: b@example.invalid\r\n" +
		"Subject: html only\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n\r\n" +
		"<html><body><p>Hello <b>world</b></p><script>alert(1)</script></body></html>\r\n"
	m := mustParse(t, []byte(raw))
	if !strings.Contains(m.HTMLBody, "<p>Hello") {
		t.Errorf("HTMLBody = %q", m.HTMLBody)
	}
	if !strings.Contains(m.TextBody, "Hello") || !strings.Contains(m.TextBody, "world") {
		t.Errorf("TextBody projection missing words: %q", m.TextBody)
	}
	if strings.Contains(m.TextBody, "alert(1)") {
		t.Errorf("script content leaked into text: %q", m.TextBody)
	}
}

func TestReadCappedExceedsLimit(t *testing.T) {
	// readCapped is the zip-bomb defense backstop. For the encodings we
	// support (base64, quoted-printable, identity), decoded never meaningfully
	// exceeds encoded, so the ratio cap doesn't fire in real traffic — but
	// it exists to protect against future encoders or bugs that produce
	// unbounded output.
	r := strings.NewReader(strings.Repeat("X", 4096))
	_, err := readCapped(r, 100, 1, 50<<20)
	if !errors.Is(err, ErrZipBomb) {
		t.Errorf("expected ErrZipBomb, got %v", err)
	}
}

func TestReadCappedRespectsFloor(t *testing.T) {
	// Tiny encoded length should not produce a cap below 1024.
	r := strings.NewReader(strings.Repeat("X", 1000))
	_, err := readCapped(r, 1, 1, 50<<20)
	if err != nil {
		t.Errorf("readCapped with floor: %v", err)
	}
}

func TestMaxDepthExceeded(t *testing.T) {
	// Build a multipart nested N levels deep, with N > MaxMimeDepth. Every
	// level has its empty line: without them each nested header ran into the
	// next delimiter, the parser bailed out on the malformed header first,
	// and the test accepted that without ever reaching the depth limit. Since
	// OPS-003 a malformed header costs one part, not the message, so only the
	// depth limit can refuse this.
	const depth = 12 // > DefaultLimits.MaxMimeDepth (10)
	var b strings.Builder
	b.WriteString("From: a@example.invalid\r\nTo: b@example.invalid\r\nSubject: deep\r\n")
	for i := 0; i < depth; i++ {
		fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=B%d\r\n\r\n", i)
		fmt.Fprintf(&b, "--B%d\r\n", i)
	}
	b.WriteString("Content-Type: text/plain\r\n\r\nbody\r\n")
	for i := depth - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "--B%d--\r\n", i)
	}
	p := New(DefaultLimits())
	_, err := p.Parse([]byte(b.String()))
	if !errors.Is(err, ErrTooDeep) {
		t.Errorf("deeply-nested message: err = %v, want ErrTooDeep", err)
	}
}

func TestMaxPartsExceeded(t *testing.T) {
	// Build a multipart/mixed with > MaxMimeParts leaf parts.
	lim := DefaultLimits()
	lim.MaxMimeParts = 5
	var b strings.Builder
	b.WriteString("From: a@example.invalid\r\nTo: b@example.invalid\r\nSubject: many\r\n")
	b.WriteString("Content-Type: multipart/mixed; boundary=B\r\n\r\n")
	for i := 0; i < 10; i++ {
		b.WriteString("--B\r\nContent-Type: text/plain\r\n\r\np")
		fmt.Fprintf(&b, "%d\r\n", i)
	}
	b.WriteString("--B--\r\n")
	p := New(lim)
	_, err := p.Parse([]byte(b.String()))
	if !errors.Is(err, ErrTooManyParts) {
		t.Errorf("expected ErrTooManyParts, got %v", err)
	}
}

func TestHeaderTooLarge(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxHeaderBytes = 100
	raw := "From: a@example.invalid\r\nTo: b@example.invalid\r\nX-Big: " +
		strings.Repeat("a", 200) + "\r\nSubject: x\r\n\r\nbody\r\n"
	p := New(lim)
	_, err := p.Parse([]byte(raw))
	if !errors.Is(err, ErrHeaderTooLarge) {
		t.Errorf("expected ErrHeaderTooLarge, got %v", err)
	}
}

func TestHeaderSectionTooLarge(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxHeaderSectionBytes = 200
	var b strings.Builder
	b.WriteString("From: a@example.invalid\r\n")
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, "X-N-%d: filler line %d\r\n", i, i)
	}
	b.WriteString("Subject: x\r\n\r\nbody\r\n")
	p := New(lim)
	_, err := p.Parse([]byte(b.String()))
	if !errors.Is(err, ErrHeadersTooBig) {
		t.Errorf("expected ErrHeadersTooBig, got %v", err)
	}
}

func TestTooLarge(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxMessageBytes = 100
	raw := []byte(plainTextMsg + strings.Repeat("padding\r\n", 500))
	p := New(lim)
	_, err := p.Parse(raw)
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("expected ErrTooLarge, got %v", err)
	}
}

func TestNoHeaderSeparatorMalformed(t *testing.T) {
	p := New(DefaultLimits())
	_, err := p.Parse([]byte("garbage with no separator"))
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

func TestMessageWithNoContentType(t *testing.T) {
	raw := "From: a@example.invalid\r\nTo: b@example.invalid\r\nSubject: bare\r\n\r\nhi\r\n"
	m := mustParse(t, []byte(raw))
	if !strings.Contains(m.TextBody, "hi") {
		t.Errorf("TextBody = %q", m.TextBody)
	}
	if m.BodyStructure.Type != "text" || m.BodyStructure.Subtype != "plain" {
		t.Errorf("default content-type not applied: %s/%s", m.BodyStructure.Type, m.BodyStructure.Subtype)
	}
}

func TestDateParsing(t *testing.T) {
	m := mustParse(t, []byte(plainTextMsg))
	if m.SentDate.Year() != 2026 || m.SentDate.Month() != 5 {
		t.Errorf("SentDate = %v", m.SentDate)
	}
}

func TestAddressListParsing(t *testing.T) {
	raw := "From: a@example.invalid\r\n" +
		"To: \"Bob\" <bob@example.invalid>, charlie@example.invalid\r\n" +
		"Subject: x\r\nContent-Type: text/plain\r\n\r\nbody\r\n"
	m := mustParse(t, []byte(raw))
	if len(m.To) != 2 {
		t.Fatalf("To = %v, want 2", m.To)
	}
	if m.To[0] != "bob@example.invalid" || m.To[1] != "charlie@example.invalid" {
		t.Errorf("To = %v", m.To)
	}
}

func TestCollapseWhitespace(t *testing.T) {
	got := collapseWhitespace("  foo   bar\n\n  baz\t  ")
	want := "foo bar\n\n baz"
	if got != want {
		t.Errorf("collapseWhitespace = %q, want %q", got, want)
	}
}

func TestHTMLToText(t *testing.T) {
	in := "<html><head><title>x</title></head><body><p>Hello</p>" +
		"<style>.a{color:red}</style><script>bad()</script>" +
		"<p>World</p></body></html>"
	out := htmlToText(in)
	if !strings.Contains(out, "Hello") || !strings.Contains(out, "World") {
		t.Errorf("htmlToText: %q", out)
	}
	if strings.Contains(out, "color:red") || strings.Contains(out, "bad()") {
		t.Errorf("style/script content leaked: %q", out)
	}
	if strings.Contains(out, "<title>") || strings.Contains(out, "x") {
		// "x" is the title content (inside <head>), which we drop.
		if strings.Contains(out, "x") {
			t.Errorf("title text leaked: %q", out)
		}
	}
}
