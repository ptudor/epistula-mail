package ingest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// OPS-003: an archive import refused a small fraction of messages that a
// mail client displays. Each test below reproduces one of those import
// errors with synthetic mail, and fails against the parser before OPS-003
// with that error. The first group runs the refusing parser deliver and IMAP
// APPEND use: none of these is a resource limit, so live delivery reads them
// too. The raw bytes are the stored blob in every case, so salvaging costs
// derived data only.

func mustParseWith(t *testing.T, p *Parser, raw string) *Message {
	t.Helper()
	msg, err := p.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return msg
}

func wantDefects(t *testing.T, msg *Message, want ...string) {
	t.Helper()
	if len(want) == 0 && len(msg.Defects) == 0 {
		return
	}
	if !reflect.DeepEqual(msg.Defects, want) {
		t.Errorf("Defects = %q, want %q", msg.Defects, want)
	}
}

// "ingest: malformed RFC 5322 message: read part: unexpected EOF": a
// multipart body that ends without its close delimiter. The last part runs to
// the end of the body.
func TestTruncatedMultipartEndsAtEOF(t *testing.T) {
	const tail = "second part, cut off mid-sent"
	raw := "From: a@ops003.invalid\r\n" +
		"Subject: truncated\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n" +
		"\r\n" +
		"--b\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"first part\r\n" +
		"--b\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		tail

	msg := mustParseWith(t, New(DefaultLimits()), raw)
	parts := msg.BodyStructure.Parts
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want 2", len(parts))
	}
	if parts[0].Size != int64(len("first part")) || parts[1].Size != int64(len(tail)) {
		t.Errorf("part sizes = %d, %d; want %d, %d", parts[0].Size, parts[1].Size, len("first part"), len(tail))
	}
	if !strings.Contains(msg.TextBody, "first part") || !strings.Contains(msg.TextBody, tail) {
		t.Errorf("TextBody = %q, want both parts", msg.TextBody)
	}
	wantDefects(t, msg, "message: "+DefectMissingCloseDelimiter)
}

// A later delimiter with the other line ending than the first was not a
// delimiter to mime/multipart, so the part before it ran to EOF: the same
// "read part: unexpected EOF", or "expecting a new Part" the other way round.
func TestMixedLineEndingDelimiters(t *testing.T) {
	for name, body := range map[string]string{
		"crlf then lf": "--b\r\nContent-Type: text/plain\r\n\r\nhello\n--b\nContent-Type: text/plain\n\nsecond\n--b--\n",
		"lf then crlf": "--b\nContent-Type: text/plain\n\nhello\r\n--b\r\nContent-Type: text/plain\r\n\r\nsecond\r\n--b--\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			raw := "From: a@ops003.invalid\nContent-Type: multipart/mixed; boundary=b\n\n" + body
			msg := mustParseWith(t, New(DefaultLimits()), raw)
			parts := msg.BodyStructure.Parts
			if len(parts) != 2 || parts[0].Size != 5 || parts[1].Size != 6 {
				t.Fatalf("parts = %+v, want hello (5) and second (6)", parts)
			}
			if msg.TextBody != "hello\nsecond" {
				t.Errorf("TextBody = %q", msg.TextBody)
			}
			wantDefects(t, msg)
		})
	}
}

// An inner multipart whose close delimiter is missing inside an outer one that
// is complete. The defect belongs to the inner part.
func TestUnclosedInnerMultipart(t *testing.T) {
	raw := "From: a@ops003.invalid\n" +
		"Content-Type: multipart/mixed; boundary=outer\n" +
		"\n" +
		"--outer\n" +
		"Content-Type: multipart/alternative; boundary=inner\n" +
		"\n" +
		"--inner\n" +
		"Content-Type: text/plain\n" +
		"\n" +
		"inner text\n" +
		"--outer\n" +
		"Content-Type: text/plain\n" +
		"\n" +
		"outer text\n" +
		"--outer--\n"

	msg := mustParseWith(t, New(DefaultLimits()), raw)
	parts := msg.BodyStructure.Parts
	if len(parts) != 2 || len(parts[0].Parts) != 1 {
		t.Fatalf("structure = %+v, want an inner multipart of one part, then a text part", msg.BodyStructure)
	}
	if !strings.Contains(msg.TextBody, "inner text") || !strings.Contains(msg.TextBody, "outer text") {
		t.Errorf("TextBody = %q", msg.TextBody)
	}
	wantDefects(t, msg, "part 1: "+DefectMissingCloseDelimiter)
}

// "ingest: malformed RFC 5322 message: no header/body separator": a
// message with no empty line at all is header fields with no body. RFC 5322
// §3.5 makes the body optional, net/mail reads it that way, and so did the
// IMAP reader's split already; it is not a defect.
func TestHeaderOnlyMessage(t *testing.T) {
	for name, raw := range map[string]string{
		"lf at eof":    "From: a@ops003.invalid\nSubject: header only\n",
		"no eol":       "From: a@ops003.invalid\nSubject: header only",
		"crlf at eof":  "From: a@ops003.invalid\r\nSubject: header only\r\n",
		"folded field": "From: a@ops003.invalid\r\nSubject: header\r\n only\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			msg := mustParseWith(t, New(DefaultLimits()), raw)
			if msg.Subject != "header only" {
				t.Errorf("Subject = %q", msg.Subject)
			}
			bs := msg.BodyStructure
			if bs.Type != "text" || bs.Subtype != "plain" || bs.Size != 0 || msg.TextBody != "" {
				t.Errorf("body = %+v / %q, want an empty text/plain", bs, msg.TextBody)
			}
			wantDefects(t, msg)
			if header, body := SplitHeaderBody([]byte(raw)); string(header) != raw || body != nil {
				t.Errorf("SplitHeaderBody = %q, %q; want the whole message as the header", header, body)
			}
		})
	}
}

// A header-only message that declares itself multipart has no parts, which is
// a defect: nothing can be read out of it.
func TestHeaderOnlyMultipartHasNoBodyParts(t *testing.T) {
	msg := mustParseWith(t, New(DefaultLimits()),
		"From: a@ops003.invalid\nSubject: s\nContent-Type: multipart/mixed; boundary=b\n")
	if len(msg.BodyStructure.Parts) != 0 {
		t.Fatalf("parts = %+v", msg.BodyStructure.Parts)
	}
	wantDefects(t, msg, "message: "+DefectNoBodyParts)
}

// A header whose last field ends in a bare LF, followed by a CRLF empty line.
// net/mail ends the header there; the old split recognised only "\r\n\r\n" and
// "\n\n", so it found no separator at all ("no header/body separator"), or
// found a later blank line in the body and measured the body lines in between
// as header fields: "single header exceeds MaxHeaderBytes" for a long one.
func TestLFFieldThenCRLFEmptyLine(t *testing.T) {
	head := "From: a@ops003.invalid\nSubject: mixed endings\n\r\n"
	for name, body := range map[string]string{
		"no later blank line":        "line one\r\nline two\r\n",
		"long line then a blank one": strings.Repeat("x", 20000) + "\r\n\r\nline two\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			raw := head + body
			msg := mustParseWith(t, New(DefaultLimits()), raw)
			if msg.Subject != "mixed endings" {
				t.Errorf("Subject = %q", msg.Subject)
			}
			if !strings.Contains(msg.TextBody, "line two") || msg.BodyStructure.Size != int64(len(body)) {
				t.Errorf("body = %d bytes / %q, want the %d bytes after the empty line", msg.BodyStructure.Size, msg.TextBody, len(body))
			}
			if header, _ := SplitHeaderBody([]byte(raw)); string(header) != head {
				t.Errorf("header section = %q, want %q", header, head)
			}
			wantDefects(t, msg)
		})
	}
}

// What the IMAP reader cannot serve stays refused in every mode: its ENVELOPE
// runs net/mail over the message's own header at FETCH time, and a header
// net/mail rejects would fail the FETCH for the whole folder.
func TestUnparseableTopLevelHeaderIsRefused(t *testing.T) {
	for name, raw := range map[string]string{
		"no field at all":         "garbage with no separator",
		"body text in the header": "Subject: s\nnot a header line\n",
		"leading continuation":    " Subject: s\n\nbody\n",
		"empty":                   "",
	} {
		for mode, p := range map[string]*Parser{"refusing": New(DefaultLimits()), "salvaging": NewSalvaging(DefaultLimits())} {
			if _, err := p.Parse([]byte(raw)); !errors.Is(err, ErrMalformed) {
				t.Errorf("%s, %s parser: err = %v, want ErrMalformed", name, mode, err)
			}
		}
	}
}

// "ingest: malformed RFC 5322 message: transfer decode: illegal base64 data at
// input byte N": a mailing list footer appended to a base64 body. Every
// byte before the footer decodes, which is the whole text.
func TestUndecodableBase64TextKeepsWhatDecoded(t *testing.T) {
	const text = "hello from a base64 body"
	encoded := base64.StdEncoding.EncodeToString([]byte(text)) + "\r\n-- \r\nList footer: http://list.invalid/\r\n"
	raw := "From: a@ops003.invalid\r\n" +
		"Subject: footer\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" + encoded

	msg := mustParseWith(t, New(DefaultLimits()), raw)
	if msg.TextBody != text {
		t.Errorf("TextBody = %q, want %q", msg.TextBody, text)
	}
	bs := msg.BodyStructure
	if bs.Size != int64(len(encoded)) || bs.Encoding != "base64" || bs.Lines != 3 {
		t.Errorf("structure = %+v, want the encoded size, encoding and line count", bs)
	}
	if len(msg.Defects) != 1 || !strings.HasPrefix(msg.Defects[0], "message: "+DefectUndecodableBody+": base64: illegal base64 data at input byte ") {
		t.Errorf("Defects = %q", msg.Defects)
	}
}

// "ingest: malformed RFC 5322 message: transfer decode: unexpected EOF": a
// base64 attachment cut short. The part stays in the structure at its encoded
// size, so BODY[n] and BODYSTRUCTURE still agree; no attachment is derived
// from a partial decode, and the rest of the message is read normally.
func TestTruncatedBase64AttachmentKeepsPart(t *testing.T) {
	full := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 the whole attachment"))
	cut := full[:len(full)-3]
	raw := "From: a@ops003.invalid\r\n" +
		"Subject: cut attachment\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n" +
		"\r\n" +
		"--b\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"see attached\r\n" +
		"--b\r\n" +
		"Content-Type: application/pdf; name=report.pdf\r\n" +
		"Content-Disposition: attachment; filename=report.pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		cut + "\r\n" +
		"--b--\r\n"

	msg := mustParseWith(t, New(DefaultLimits()), raw)
	if len(msg.Attachments) != 0 {
		t.Errorf("an attachment was derived from a partial decode: %+v", msg.Attachments)
	}
	pdf := msg.BodyStructure.Parts[1]
	if pdf.Subtype != "pdf" || pdf.Size != int64(len(cut)) || pdf.Encoding != "base64" {
		t.Errorf("part 2 = %+v, want the pdf at its encoded size", pdf)
	}
	if !strings.Contains(msg.TextBody, "see attached") {
		t.Errorf("TextBody = %q", msg.TextBody)
	}
	wantDefects(t, msg, "part 2: "+DefectUndecodableBody+": base64: unexpected EOF")
}

// An attached text part is both an attachment and text (RA6X-049). Undecodable,
// it keeps only the text.
func TestUndecodableAttachedTextIsTextOnly(t *testing.T) {
	notes := base64.StdEncoding.EncodeToString([]byte("meeting notes"))
	raw := "From: a@ops003.invalid\r\n" +
		"Content-Type: text/plain; name=notes.txt\r\n" +
		"Content-Disposition: attachment; filename=notes.txt\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		notes + "!!\r\n"

	msg := mustParseWith(t, New(DefaultLimits()), raw)
	if len(msg.Attachments) != 0 || msg.TextBody != "meeting notes" {
		t.Errorf("attachments %+v, text %q; want no attachment and the decoded text", msg.Attachments, msg.TextBody)
	}
}

// An enclosed message whose transfer encoding does not decode is kept as a
// message/rfc822 part without structure: the IMAP reader cannot enter it
// either (DecodeEncapsulated fails), so neither side advertises its parts.
func TestUndecodableEnclosedMessageIsOpaque(t *testing.T) {
	inner := base64.StdEncoding.EncodeToString([]byte("From: inner@ops003.invalid\r\nSubject: in\r\n\r\ninner body\r\n"))
	raw := "From: a@ops003.invalid\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n" +
		"\r\n" +
		"--b\r\n" +
		"Content-Type: message/rfc822\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		inner[:len(inner)-2] + "\r\n" +
		"--b--\r\n"

	msg := mustParseWith(t, New(DefaultLimits()), raw)
	container := msg.BodyStructure.Parts[0]
	if container.Type != "message" || len(container.Parts) != 0 || container.Envelope != nil || len(msg.Attachments) != 0 {
		t.Errorf("part 1 = %+v, attachments %+v; want an opaque message/rfc822 and no attachment", container, msg.Attachments)
	}
	if strings.Contains(msg.TextBody, "inner body") {
		t.Errorf("text of an undecodable enclosed message was walked: %q", msg.TextBody)
	}
	if _, err := DecodeEncapsulated([]byte(inner[:len(inner)-2]), "base64"); err == nil {
		t.Error("DecodeEncapsulated entered a body that does not decode")
	}
	wantDefects(t, msg, "part 1: "+DefectUndecodableBody+": base64: unexpected EOF")
}

// A MIME part whose header net/mail cannot parse is kept as opaque bytes. It
// is addressed by position alone, so it still lines up with BODY[n].
func TestUnparseablePartHeaderIsOpaque(t *testing.T) {
	raw := "From: a@ops003.invalid\n" +
		"Content-Type: multipart/mixed; boundary=b\n" +
		"\n" +
		"--b\n" +
		"Content-Type: text/plain\n" +
		"Content-Transf\n" + // a header line cut short
		"\n" +
		"opaque now\n" +
		"--b\n" +
		"Content-Type: text/plain\n" +
		"\n" +
		"still read\n" +
		"--b--\n"

	msg := mustParseWith(t, New(DefaultLimits()), raw)
	first := msg.BodyStructure.Parts[0]
	if first.Type != "application" || first.Subtype != "octet-stream" || first.Size != int64(len("opaque now")) {
		t.Errorf("part 1 = %+v, want opaque bytes at the body's size", first)
	}
	if strings.Contains(msg.TextBody, "opaque now") || !strings.Contains(msg.TextBody, "still read") {
		t.Errorf("TextBody = %q", msg.TextBody)
	}
	wantDefects(t, msg, "part 1: "+DefectUnparseableHeader)
}

// deepMultipart nests multiparts depth levels below the top-level body, with
// "deepest text" in the innermost.
func deepMultipart(depth int) string {
	var b strings.Builder
	b.WriteString("From: a@ops003.invalid\r\nSubject: deep\r\n")
	for i := 0; i < depth; i++ {
		fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=B%d\r\n\r\n--B%d\r\n", i, i)
	}
	b.WriteString("Content-Type: text/plain\r\n\r\ndeepest text\r\n")
	for i := depth - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "--B%d--\r\n", i)
	}
	return b.String()
}

// The resource limits are unchanged for delivery and APPEND, which refuse.
// Salvaging, an entity nested past MaxMimeDepth is kept as an opaque leaf with
// its declared type, and nothing below it is read.
// "ingest: MIME depth exceeds MaxMimeDepth" (1+).
func TestSalvageKeepsTooDeepEntityOpaque(t *testing.T) {
	raw := deepMultipart(12)
	if _, err := New(DefaultLimits()).Parse([]byte(raw)); !errors.Is(err, ErrTooDeep) {
		t.Fatalf("refusing parser: err = %v, want ErrTooDeep", err)
	}
	msg := mustParseWith(t, NewSalvaging(DefaultLimits()), raw)
	node, path := msg.BodyStructure, ""
	for depth := 1; depth <= 11; depth++ {
		if len(node.Parts) != 1 {
			t.Fatalf("depth %d: %d parts, want 1", depth, len(node.Parts))
		}
		node, path = node.Parts[0], childPath(path, 1)
	}
	if node.Type != "multipart" || len(node.Parts) != 0 {
		t.Errorf("entity at depth 11 = %+v, want an opaque multipart", node)
	}
	if strings.Contains(msg.TextBody, "deepest text") {
		t.Error("text below the depth limit was read")
	}
	wantDefects(t, msg, "part "+path+": "+DefectDepthExceeded)
}

// An enclosed message cannot be kept opaque at its own body (whether that body
// is single-part decides its part numbering), so the message/rfc822 container
// holding it is, as for an unparseable enclosed message.
func TestSalvageKeepsTooDeepEnclosedMessageOpaque(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxMimeDepth = 3
	wrapped := "From: deepest@ops003.invalid\r\nContent-Type: text/plain\r\n\r\nbottom\r\n"
	for i := 0; i < 6; i++ {
		wrapped = "From: wrap@ops003.invalid\r\nContent-Type: message/rfc822\r\n\r\n" + wrapped
	}
	if _, err := New(limits).Parse([]byte(wrapped)); !errors.Is(err, ErrTooDeep) {
		t.Fatalf("refusing parser: err = %v, want ErrTooDeep", err)
	}
	msg := mustParseWith(t, NewSalvaging(limits), wrapped)
	// The top-level body is part 1 and each enclosed message's body is .1
	// below it, so the fourth message/rfc822 body, the one whose enclosed
	// message is past the depth limit, is 1.1.1.1 (RFC 9051 §6.4.5, OPS-004).
	if len(msg.Attachments) != 1 || msg.Attachments[0].PartNumber != "1.1.1.1" || msg.Attachments[0].ContentType != "message/rfc822" {
		t.Errorf("attachments = %+v, want the opaque message/rfc822 at 1.1.1.1", msg.Attachments)
	}
	wantDefects(t, msg, "part 1.1.1.1: "+DefectDepthExceeded)
}

// Salvaging, the parts after MaxMimeParts are not listed; the ones before it
// are read normally.
func TestSalvageStopsListingAtPartLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString("From: a@ops003.invalid\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n")
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&b, "--B\r\nContent-Type: text/plain\r\n\r\npart%d\r\n", i)
	}
	b.WriteString("--B--\r\n")
	limits := DefaultLimits()
	limits.MaxMimeParts = 5

	if _, err := New(limits).Parse([]byte(b.String())); !errors.Is(err, ErrTooManyParts) {
		t.Fatalf("refusing parser: err = %v, want ErrTooManyParts", err)
	}
	msg := mustParseWith(t, NewSalvaging(limits), b.String())
	// The multipart itself is one of the five.
	if n := len(msg.BodyStructure.Parts); n != 4 {
		t.Errorf("listed %d parts, want 4", n)
	}
	if !strings.Contains(msg.TextBody, "part4") || strings.Contains(msg.TextBody, "part5") {
		t.Errorf("TextBody = %q, want parts 1-4 only", msg.TextBody)
	}
	wantDefects(t, msg, "message: "+DefectPartsExceeded)
}

// "ingest: single header exceeds MaxHeaderBytes". Salvaging, an
// over-limit field is left out of the parsed headers without being read, and
// the message is otherwise read normally. The raw bytes, which IMAP serves,
// still have it.
func TestSalvageOmitsOverLimitHeaderField(t *testing.T) {
	var refs strings.Builder
	refs.WriteString("References:")
	for i := 0; i < 700; i++ { // 18,911 bytes unfolded, over the 16 KiB limit
		fmt.Fprintf(&refs, " <thread-%04d@list.invalid>\r\n", i)
	}
	raw := "From: a@ops003.invalid\r\n" +
		"Subject: long thread\r\n" +
		refs.String() +
		"In-Reply-To: <thread-0699@list.invalid>\r\n" +
		"\r\n" +
		"reply body\r\n"

	if _, err := New(DefaultLimits()).Parse([]byte(raw)); !errors.Is(err, ErrHeaderTooLarge) {
		t.Fatalf("refusing parser: err = %v, want ErrHeaderTooLarge", err)
	}
	msg := mustParseWith(t, NewSalvaging(DefaultLimits()), raw)
	if _, ok := msg.Headers["References"]; ok {
		t.Error("the over-limit References field was parsed")
	}
	if msg.Subject != "long thread" || msg.InReplyTo != "thread-0699@list.invalid" || msg.TextBody != "reply body" {
		t.Errorf("subject %q, in-reply-to %q, text %q", msg.Subject, msg.InReplyTo, msg.TextBody)
	}
	if sum := sha256.Sum256([]byte(raw)); msg.SHA256Hex != hex.EncodeToString(sum[:]) {
		t.Error("the stored identity is not the raw bytes' digest")
	}
	fieldBytes := len(refs.String())
	wantDefects(t, msg, fmt.Sprintf("message: %s: References (1 field(s), %d bytes)", DefectHeaderFieldTooLarge, fieldBytes))
}

// A structural field over the limit cannot be left out: the IMAP reader
// traverses by it whatever its size. On the message itself that refuses the
// message even when salvaging; on a MIME part the part is kept opaque.
func TestSalvageOverLimitStructuralField(t *testing.T) {
	huge := "; x-pad=" + strings.Repeat("p", 20000)
	top := "From: a@ops003.invalid\r\nContent-Type: text/plain" + huge + "\r\n\r\nbody\r\n"
	if _, err := NewSalvaging(DefaultLimits()).Parse([]byte(top)); !errors.Is(err, ErrHeaderTooLarge) {
		t.Errorf("top-level Content-Type: err = %v, want ErrHeaderTooLarge", err)
	}

	part := "From: a@ops003.invalid\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n" +
		"\r\n" +
		"--b\r\n" +
		"Content-Type: text/plain" + huge + "\r\n" +
		"\r\n" +
		"part body\r\n" +
		"--b--\r\n"
	msg := mustParseWith(t, NewSalvaging(DefaultLimits()), part)
	p := msg.BodyStructure.Parts[0]
	if p.Type != "application" || p.Size != int64(len("part body")) || strings.Contains(msg.TextBody, "part body") {
		t.Errorf("part 1 = %+v, text %q; want opaque bytes", p, msg.TextBody)
	}
	wantDefects(t, msg, "part 1: "+DefectHeaderFieldTooLarge)
}

// An over-limit line net/mail would reject cannot be left out of the message's
// own header either: the parse would succeed where the IMAP reader's parse of
// the same bytes fails.
func TestSalvageRefusesOverLimitLineNetMailRejects(t *testing.T) {
	raw := "From: a@ops003.invalid\r\n" + strings.Repeat("x", 20000) + "\r\n\r\nbody\r\n"
	if _, err := New(DefaultLimits()).Parse([]byte(raw)); !errors.Is(err, ErrHeaderTooLarge) {
		t.Errorf("refusing parser: err = %v, want ErrHeaderTooLarge", err)
	}
	if _, err := NewSalvaging(DefaultLimits()).Parse([]byte(raw)); !errors.Is(err, ErrMalformed) {
		t.Errorf("salvaging parser: err = %v, want ErrMalformed", err)
	}
}

// Salvaging, a header section over MaxHeaderSectionBytes keeps the fields that
// fit and leaves the rest out.
func TestSalvageHeaderSectionKeepsWhatFits(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxHeaderSectionBytes = 200
	var b strings.Builder
	b.WriteString("From: a@ops003.invalid\r\nSubject: kept\r\n")
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, "X-Filler-%02d: %s\r\n", i, strings.Repeat("f", 20))
	}
	b.WriteString("\r\nbody\r\n")

	if _, err := New(limits).Parse([]byte(b.String())); !errors.Is(err, ErrHeadersTooBig) {
		t.Fatalf("refusing parser: err = %v, want ErrHeadersTooBig", err)
	}
	msg := mustParseWith(t, NewSalvaging(limits), b.String())
	if msg.Subject != "kept" || msg.TextBody != "body" {
		t.Errorf("subject %q, text %q", msg.Subject, msg.TextBody)
	}
	if _, ok := msg.Headers["X-Filler-19"]; ok {
		t.Error("a field past the section limit was parsed")
	}
	if len(msg.Defects) != 1 || !strings.HasPrefix(msg.Defects[0], "message: "+DefectHeaderSectionTooLarge+": X-Filler-") {
		t.Errorf("Defects = %q", msg.Defects)
	}
}

// Idempotency rests on this: the same bytes always parse to the same result,
// salvaged or not, and the stored identity is always the digest of the raw
// bytes, which salvaging never touches.
func TestSalvagedParseIsDeterministic(t *testing.T) {
	fixtures := []string{
		deepMultipart(12),
		"From: a@ops003.invalid\nContent-Type: multipart/mixed; boundary=b\n\n--b\n\nopen\n--b\nContent-Type: application/octet-stream\nContent-Transfer-Encoding: base64\n\nQUJD!\n",
		"From: a@ops003.invalid\nSubject: header only\n",
	}
	for _, raw := range fixtures {
		first := mustParseWith(t, NewSalvaging(DefaultLimits()), raw)
		for i := 0; i < 20; i++ {
			again := mustParseWith(t, NewSalvaging(DefaultLimits()), raw)
			a, _ := json.Marshal(first.BodyStructure)
			b, _ := json.Marshal(again.BodyStructure)
			if string(a) != string(b) || first.TextBody != again.TextBody ||
				!reflect.DeepEqual(first.Defects, again.Defects) || !reflect.DeepEqual(first.Attachments, again.Attachments) {
				t.Fatalf("%q parsed differently on run %d", raw, i+2)
			}
		}
		if sum := sha256.Sum256([]byte(raw)); first.SHA256Hex != hex.EncodeToString(sum[:]) {
			t.Errorf("%q: identity %s is not the raw digest", raw, first.SHA256Hex)
		}
	}
}

// The delivery_log summary is bounded however many parts are broken.
func TestDefectSummaryIsBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("From: a@ops003.invalid\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n")
	for i := 0; i < 150; i++ {
		b.WriteString("--B\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\nQUJD!\r\n")
	}
	b.WriteString("--B--\r\n")
	msg := mustParseWith(t, New(DefaultLimits()), b.String())
	if len(msg.Defects) != 150 {
		t.Fatalf("%d defects, want 150", len(msg.Defects))
	}
	summary := msg.DefectSummary()
	if len(summary) > maxDefectSummary+len("…") || !strings.HasSuffix(summary, "…") {
		t.Errorf("summary is %d bytes, want at most %d and marked as cut", len(summary), maxDefectSummary)
	}
}
