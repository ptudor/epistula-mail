package ingest

import (
	"errors"
	"strings"
	"testing"
)

// buildForward returns a multipart/mixed message whose second part is a
// message/rfc822 wrapping innerBody (a complete RFC 5322 message).
func buildForward(innerMessage string) []byte {
	return []byte("From: fwd@rfc822.invalid\r\n" +
		"To: rcpt@rfc822.invalid\r\n" +
		"Subject: Fwd: see below\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=OUTER\r\n" +
		"\r\n" +
		"--OUTER\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"FYI, forwarding this.\r\n" +
		"--OUTER\r\n" +
		"Content-Type: message/rfc822\r\n" +
		"\r\n" +
		innerMessage +
		"\r\n--OUTER--\r\n")
}

func TestEmbeddedMessageTextIsExtracted(t *testing.T) {
	inner := "From: original@rfc822.invalid\r\n" +
		"Subject: the original\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"inner secret content\r\n"

	msg, err := New(DefaultLimits()).Parse(buildForward(inner))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !strings.Contains(msg.TextBody, "FYI, forwarding this.") {
		t.Errorf("outer text missing from TextBody: %q", msg.TextBody)
	}
	if !strings.Contains(msg.TextBody, "inner secret content") {
		t.Errorf("embedded message text missing from TextBody (FTS blind spot): %q", msg.TextBody)
	}
	// The embedded message must not be stored as an opaque attachment.
	for _, a := range msg.Attachments {
		if a.ContentType == "message/rfc822" {
			t.Errorf("embedded message stored as opaque attachment despite successful recursion")
		}
	}
	// BODYSTRUCTURE: part 2 is message/rfc822 and carries the nested
	// structure as a child.
	if len(msg.BodyStructure.Parts) != 2 {
		t.Fatalf("BodyStructure has %d parts, want 2", len(msg.BodyStructure.Parts))
	}
	p2 := msg.BodyStructure.Parts[1]
	if p2.Type != "message" || p2.Subtype != "rfc822" {
		t.Fatalf("part 2 = %s/%s, want message/rfc822", p2.Type, p2.Subtype)
	}
	if len(p2.Parts) != 1 || p2.Parts[0].Type != "text" {
		t.Errorf("message/rfc822 part lacks nested structure: %+v", p2.Parts)
	}
}

func TestEmbeddedMessageAttachmentsAreCaptured(t *testing.T) {
	inner := "From: original@rfc822.invalid\r\n" +
		"Subject: with attachment\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=INNER\r\n" +
		"\r\n" +
		"--INNER\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"see attached\r\n" +
		"--INNER\r\n" +
		"Content-Type: application/pdf\r\n" +
		"Content-Disposition: attachment; filename=report.pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"JVBERi0xLjQK\r\n" +
		"--INNER--\r\n"

	msg, err := New(DefaultLimits()).Parse(buildForward(inner))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !strings.Contains(msg.TextBody, "see attached") {
		t.Errorf("nested multipart text missing: %q", msg.TextBody)
	}
	var pdf *Attachment
	for i := range msg.Attachments {
		if msg.Attachments[i].ContentType == "application/pdf" {
			pdf = &msg.Attachments[i]
		}
	}
	if pdf == nil {
		t.Fatalf("nested PDF attachment not captured; attachments: %+v", msg.Attachments)
	}
	if pdf.Filename != "report.pdf" {
		t.Errorf("nested attachment filename = %q, want report.pdf", pdf.Filename)
	}
	// Part path descends through the rfc822 part (part 2), inner part 2.
	if pdf.PartNumber != "2.2" {
		t.Errorf("nested attachment part number = %q, want 2.2", pdf.PartNumber)
	}
}

func TestEmbeddedMessageUnparseableDegradesToAttachment(t *testing.T) {
	// No header/body separator — not a parseable message.
	msg, err := New(DefaultLimits()).Parse(buildForward("garbage-not-a-message"))
	if err != nil {
		t.Fatalf("Parse must not fail on a broken forward: %v", err)
	}
	found := false
	for _, a := range msg.Attachments {
		if a.ContentType == "message/rfc822" {
			found = true
		}
	}
	if !found {
		t.Errorf("broken embedded message not degraded to opaque attachment; attachments: %+v", msg.Attachments)
	}
}

func TestEmbeddedMessageCountsAgainstDepthLimit(t *testing.T) {
	// Nest message/rfc822 wrappers past MaxMimeDepth; the parser must
	// reject rather than recurse unboundedly.
	limits := DefaultLimits()
	limits.MaxMimeDepth = 3

	inner := "From: deepest@rfc822.invalid\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"bottom\r\n"
	wrapped := inner
	for i := 0; i < 6; i++ {
		wrapped = "From: wrap@rfc822.invalid\r\n" +
			"Content-Type: message/rfc822\r\n" +
			"\r\n" +
			wrapped
	}
	raw := []byte(wrapped)

	_, err := New(limits).Parse(raw)
	if !errors.Is(err, ErrTooDeep) {
		t.Fatalf("Parse = %v, want ErrTooDeep (rfc822 wrapping must not evade the depth cap)", err)
	}
}

func TestReadCappedHonorsBudget(t *testing.T) {
	// Expansion cap alone would allow 10×100 = 1000 bytes; a tighter budget
	// must win and trip the bomb error.
	src := strings.NewReader(strings.Repeat("x", 900))
	_, err := readCapped(src, 100, 10, 500)
	if !errors.Is(err, ErrZipBomb) {
		t.Fatalf("readCapped = %v, want ErrZipBomb when output exceeds budget", err)
	}

	// Under budget passes untouched.
	src = strings.NewReader(strings.Repeat("x", 400))
	out, err := readCapped(src, 100, 10, 500)
	if err != nil {
		t.Fatalf("readCapped: %v", err)
	}
	if len(out) != 400 {
		t.Fatalf("readCapped len = %d, want 400", len(out))
	}
}
