package ingest

import (
	"strings"
	"testing"
)

func batch3Parser() *Parser {
	return New(Limits{
		MaxMessageBytes:       10 << 20,
		MaxMimeDepth:          10,
		MaxMimeParts:          200,
		MaxHeaderBytes:        16 << 10,
		MaxHeaderSectionBytes: 256 << 10,
		MaxTransferExpansion:  10,
	})
}

// TestQuotedPrintablePartKeepsItsEncoding is the RA6X-005 ingest half.
//
// mime/multipart's NextPart silently decodes a quoted-printable body AND
// deletes its Content-Transfer-Encoding header, so the parser recorded the
// DECODED length as the BODYSTRUCTURE size (where RFC 3501 §7.4.2 requires the
// encoded one) and recorded no encoding at all. A client decoding by the
// advertised structure then had no way to know the bytes it fetched were
// already decoded.
func TestQuotedPrintablePartKeepsItsEncoding(t *testing.T) {
	// "price=3D3D10" encodes the literal text "price=3D10": a double-encoded
	// sequence, so a second decode is detectable.
	encoded := "price=3D3D10 and a soft break=\r\ncontinues here"
	raw := "From: s@x.invalid\r\n" +
		"Subject: qp\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n" +
		"\r\n" +
		"--b\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		encoded + "\r\n" +
		"--b--\r\n"

	msg, err := batch3Parser().Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(msg.BodyStructure.Parts) != 1 {
		t.Fatalf("got %d parts, want 1", len(msg.BodyStructure.Parts))
	}
	part := msg.BodyStructure.Parts[0]

	if part.Encoding != "quoted-printable" {
		t.Errorf("encoding = %q, want quoted-printable", part.Encoding)
	}
	if want := int64(len(encoded)); part.Size != want {
		t.Errorf("size = %d, want the ENCODED length %d", part.Size, want)
	}
	// The text projection must be decoded exactly ONCE.
	if !strings.Contains(msg.TextBody, "price=3D10") {
		t.Errorf("text projection = %q, want it to contain the once-decoded %q",
			msg.TextBody, "price=3D10")
	}
	if strings.Contains(msg.TextBody, "price=10") {
		t.Errorf("text projection was decoded twice: %q", msg.TextBody)
	}
	if strings.Contains(msg.TextBody, "=\n") || strings.Contains(msg.TextBody, "=\r\n") {
		t.Errorf("soft line break survived decoding: %q", msg.TextBody)
	}
}

// TestUntypedChildDefaultsToTextPlain is the RA6X-010 regression: a multipart
// child with no Content-Type is text/plain per RFC 2045 §5.2, not
// application/octet-stream. Treating absence as opaque turned an ordinary
// untyped text part into a binary attachment — excluded from the text
// projection, invisible to search, and shown to the user as a file.
func TestUntypedChildDefaultsToTextPlain(t *testing.T) {
	raw := "From: s@x.invalid\r\n" +
		"Subject: untyped\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n" +
		"\r\n" +
		"--b\r\n" +
		"\r\n" +
		"this part has no content-type\r\n" +
		"--b--\r\n"

	msg, err := batch3Parser().Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	part := msg.BodyStructure.Parts[0]
	if part.Type != "text" || part.Subtype != "plain" {
		t.Errorf("untyped child = %s/%s, want text/plain", part.Type, part.Subtype)
	}
	if !strings.Contains(msg.TextBody, "no content-type") {
		t.Errorf("untyped child did not reach the text projection: %q", msg.TextBody)
	}
	if len(msg.Attachments) != 0 {
		t.Errorf("untyped text child became %d attachment(s)", len(msg.Attachments))
	}
}

// TestDigestChildDefaultsToMessageRFC822 pins the other default RFC 2046
// §5.1.5 defines: inside multipart/digest an absent Content-Type means
// message/rfc822, which is the entire point of digest.
func TestDigestChildDefaultsToMessageRFC822(t *testing.T) {
	raw := "From: s@x.invalid\r\n" +
		"Subject: digest\r\n" +
		"Content-Type: multipart/digest; boundary=b\r\n" +
		"\r\n" +
		"--b\r\n" +
		"\r\n" +
		"From: inner@x.invalid\r\n" +
		"Subject: enclosed\r\n" +
		"\r\n" +
		"enclosed body\r\n" +
		"--b--\r\n"

	msg, err := batch3Parser().Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	part := msg.BodyStructure.Parts[0]
	if part.Type != "message" || part.Subtype != "rfc822" {
		t.Fatalf("digest child = %s/%s, want message/rfc822", part.Type, part.Subtype)
	}
	if part.Envelope == nil || !strings.Contains(part.Envelope.Subject, "enclosed") {
		t.Errorf("digest child envelope = %+v, want the enclosed message's subject", part.Envelope)
	}
}

// TestChildHeaderLimitsAreEnforced is the RA6X-016 regression: the configured
// header limits applied to the top-level message and to explicitly parsed
// embedded messages, but not to multipart children — so the same header was
// accepted or rejected depending only on how deeply it was nested.
func TestChildHeaderLimitsAreEnforced(t *testing.T) {
	huge := strings.Repeat("A", 1000)
	raw := "From: s@x.invalid\r\n" +
		"Subject: big child header\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n" +
		"\r\n" +
		"--b\r\n" +
		"Content-Type: text/plain\r\n" +
		"X-Huge: " + huge + "\r\n" +
		"\r\n" +
		"body\r\n" +
		"--b--\r\n"

	p := New(Limits{
		MaxMessageBytes:       10 << 20,
		MaxMimeDepth:          10,
		MaxMimeParts:          200,
		MaxHeaderBytes:        100, // the child header is 1000+ bytes
		MaxHeaderSectionBytes: 256 << 10,
		MaxTransferExpansion:  10,
	})
	if _, err := p.Parse([]byte(raw)); err == nil {
		t.Fatal("an oversized MIME child header was accepted")
	}

	// The same message with a generous limit is fine, so the rejection is the
	// limit and not the shape.
	if _, err := batch3Parser().Parse([]byte(raw)); err != nil {
		t.Fatalf("the same message under a generous limit failed: %v", err)
	}
}

// TestAttachedTextPartIsBothAttachmentAndText is the RA6X-049 regression under
// the chosen contract: an explicitly attached text part gets an attachment row
// so its filename is visible, AND still contributes to the text projection so
// search coverage does not regress.
func TestAttachedTextPartIsBothAttachmentAndText(t *testing.T) {
	raw := "From: s@x.invalid\r\n" +
		"Subject: report\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n" +
		"\r\n" +
		"--b\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"see the attached report\r\n" +
		"--b\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Disposition: attachment; filename=\"report.txt\"\r\n" +
		"\r\n" +
		"quarterly numbers inside\r\n" +
		"--b--\r\n"

	msg, err := batch3Parser().Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(msg.Attachments) != 1 {
		t.Fatalf("got %d attachment(s), want 1", len(msg.Attachments))
	}
	att := msg.Attachments[0]
	if att.Filename != "report.txt" {
		t.Errorf("attachment filename = %q, want report.txt", att.Filename)
	}
	if att.ContentType != "text/plain" {
		t.Errorf("attachment content type = %q, want text/plain", att.ContentType)
	}
	if att.PartNumber != "2" {
		t.Errorf("attachment part number = %q, want 2", att.PartNumber)
	}
	// And it is still searchable.
	if !strings.Contains(msg.TextBody, "quarterly numbers") {
		t.Errorf("attached text left the projection: %q", msg.TextBody)
	}
	if !strings.Contains(msg.TextBody, "see the attached report") {
		t.Errorf("inline body left the projection: %q", msg.TextBody)
	}
}

// TestInlineTextPartIsNotAnAttachment pins the boundary: an ordinary inline
// text part is still just the body.
func TestInlineTextPartIsNotAnAttachment(t *testing.T) {
	raw := "From: s@x.invalid\r\n" +
		"Subject: plain\r\n" +
		"Content-Type: text/plain\r\n" +
		"Content-Disposition: inline\r\n" +
		"\r\n" +
		"just the body\r\n"

	msg, err := batch3Parser().Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(msg.Attachments) != 0 {
		t.Fatalf("an inline text part became %d attachment(s)", len(msg.Attachments))
	}
}

// TestFailedEmbeddedParseLeavesNoPartialState is the RA6X-038 regression.
//
// The permissive message/rfc822 fallback restored only the byte budget, so text
// and attachments accumulated from an earlier valid nested part stayed behind
// when a later one failed and the whole embedded message degraded to one opaque
// attachment. The projection and the returned structure then disagreed,
// including attachment rows whose part numbers reference parts that are not in
// the structure.
//
// Since OPS-003 the unwalkable nested part no longer fails the enclosed
// message. It is a multipart child, addressed by position alone, so it is kept
// in place as an opaque leaf with a defect, and the valid parts before it
// legitimately stay in the projection. The invariant this test exists for is
// unchanged and is what it checks: every piece of text and every attachment
// belongs to a node of the returned structure.
func TestFailedEmbeddedParseLeavesNoPartialState(t *testing.T) {
	// An embedded message whose first two nested parts are a valid text part
	// and an attachment, and whose third has a Content-Type the parser cannot
	// walk (a multipart with no boundary).
	inner := "From: inner@x.invalid\r\n" +
		"Subject: forwarded\r\n" +
		"Content-Type: multipart/mixed; boundary=inner\r\n" +
		"\r\n" +
		"--inner\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"NESTED-TEXT-MARKER\r\n" +
		"--inner\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"nested.bin\"\r\n" +
		"\r\n" +
		"nested attachment bytes\r\n" +
		"--inner\r\n" +
		"Content-Type: multipart/mixed\r\n" + // no boundary: unwalkable
		"\r\n" +
		"broken\r\n" +
		"--inner--\r\n"

	raw := "From: s@x.invalid\r\n" +
		"Subject: outer\r\n" +
		"Content-Type: message/rfc822\r\n" +
		"\r\n" + inner

	msg, err := batch3Parser().Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	container := msg.BodyStructure
	if container.Type != "message" || len(container.Parts) != 1 || len(container.Parts[0].Parts) != 3 {
		t.Fatalf("structure = %+v, want message/rfc822 enclosing a three-part multipart", container)
	}
	broken := container.Parts[0].Parts[2]
	if broken.Type != "multipart" || len(broken.Parts) != 0 || len(broken.Defects) != 1 || broken.Defects[0] != DefectMissingBoundary {
		t.Errorf("unwalkable part = %+v, want an opaque multipart leaf with a %s defect", broken, DefectMissingBoundary)
	}
	if !strings.Contains(msg.TextBody, "NESTED-TEXT-MARKER") {
		t.Errorf("text of a walked nested part is missing: %q", msg.TextBody)
	}
	// The top-level body is part 1, so the enclosed multipart's third child
	// is 1.3 (RFC 9051 §6.4.5, OPS-004).
	if len(msg.Defects) != 1 || msg.Defects[0] != "part 1.3: "+DefectMissingBoundary {
		t.Errorf("Defects = %q, want the one missing boundary at part 1.3", msg.Defects)
	}

	// Every attachment must correspond to a part the structure actually has.
	paths := structurePaths(msg.BodyStructure)
	found := false
	for _, a := range msg.Attachments {
		if !paths[a.PartNumber] {
			t.Errorf("attachment part %q is not present in the returned structure %v", a.PartNumber, paths)
		}
		found = found || a.Filename == "nested.bin"
	}
	if !found {
		t.Errorf("attachment of a walked nested part is missing: %+v", msg.Attachments)
	}
}

// structurePaths returns every IMAP part path the structure advertises,
// numbered per RFC 9051 §6.4.5: a multipart child appends its index, and a
// message's body, the top-level one or an enclosed one, shares the number its
// parts are numbered under when multipart and is <n>.1 otherwise. So a
// top-level single part is 1, even a message/rfc822 one (OPS-004).
func structurePaths(bs BodyStructure) map[string]bool {
	paths := map[string]bool{}
	var visit func(n BodyStructure, path string)
	visit = func(n BodyStructure, path string) {
		if path != "" {
			paths[path] = true
		}
		if n.Type == "message" && n.Subtype == "rfc822" {
			if len(n.Parts) == 1 {
				if n.Parts[0].Type == "multipart" {
					visit(n.Parts[0], path)
				} else {
					visit(n.Parts[0], childPath(path, 1))
				}
			}
			return
		}
		for i, child := range n.Parts {
			visit(child, childPath(path, i+1))
		}
	}
	if bs.Type == "multipart" {
		visit(bs, "")
	} else {
		visit(bs, "1")
	}
	return paths
}
