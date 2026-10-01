package ingest

import (
	"encoding/base64"
	"strings"
	"testing"
)

// TestBodyStructureSizeIsEncodedLength is the RO5X-003 regression.
//
// RFC 3501 §7.4.2 / RFC 9051 §7.5.2 define the BODYSTRUCTURE body-size field
// as the size "in its transfer encoding and not the resulting size after any
// decoding". The ingest walker used to overwrite it with the decoded length,
// while the IMAP read path returns the on-wire (encoded) bytes for BODY[N] —
// so every MUA was told a base64 attachment was ~27% smaller than the bytes
// it then received.
//
// Attachment.Size is the *decoded* blob length and must stay that way: it is
// what lands in attachments.size_bytes and what epistula-api reports.
func TestBodyStructureSizeIsEncodedLength(t *testing.T) {
	// A payload whose decoded and encoded lengths differ substantially.
	payload := []byte(strings.Repeat("attachment payload bytes! ", 40)) // 1000 bytes
	encoded := base64.StdEncoding.EncodeToString(payload)

	// Wrap the base64 at 76 columns, as real MUAs do.
	var wrapped strings.Builder
	for i := 0; i < len(encoded); i += 76 {
		end := i + 76
		if end > len(encoded) {
			end = len(encoded)
		}
		wrapped.WriteString(encoded[i:end])
		wrapped.WriteString("\r\n")
	}

	raw := []byte("From: s@size.invalid\r\n" +
		"To: r@size.invalid\r\n" +
		"Subject: sizes\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=BOUND\r\n" +
		"\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"see attachment\r\n" +
		"--BOUND\r\n" +
		"Content-Type: application/octet-stream; name=\"blob.bin\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"Content-Disposition: attachment; filename=\"blob.bin\"\r\n" +
		"\r\n" +
		wrapped.String() +
		"--BOUND--\r\n")

	msg, err := New(DefaultLimits()).Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(msg.BodyStructure.Parts) != 2 {
		t.Fatalf("Parts = %d, want 2", len(msg.BodyStructure.Parts))
	}

	decodedLen := int64(len(payload))
	att := msg.BodyStructure.Parts[1]

	if att.Size == decodedLen {
		t.Errorf("BodyStructure size = %d, which is the DECODED length — "+
			"the RFC requires the encoded length (RO5X-003)", att.Size)
	}
	if att.Size <= decodedLen {
		t.Errorf("BodyStructure size = %d, want > decoded length %d for base64",
			att.Size, decodedLen)
	}

	// The attachment blob keeps the decoded length.
	if len(msg.Attachments) != 1 {
		t.Fatalf("Attachments = %d, want 1", len(msg.Attachments))
	}
	if got := msg.Attachments[0].Size; got != decodedLen {
		t.Errorf("Attachment.Size = %d, want the decoded length %d "+
			"(this is attachments.size_bytes and must NOT change)", got, decodedLen)
	}
	if got := int64(len(msg.Attachments[0].Data)); got != decodedLen {
		t.Errorf("Attachment.Data = %d bytes, want %d", got, decodedLen)
	}

	// The announced size must equal the bytes the IMAP read path hands back
	// for BODY[2] — the encoded, on-wire bytes of that part. Per RFC 2046
	// §5.1.1 the CRLF preceding a boundary delimiter belongs to the
	// delimiter, not to the part, so the body is `wrapped` less its final
	// CRLF — which is exactly what mime/multipart yields to both this
	// walker and the IMAP reader.
	if want := int64(len(wrapped.String()) - 2); att.Size != want {
		t.Errorf("BodyStructure size = %d, want %d (the part's on-wire byte count)",
			att.Size, want)
	}
}

// TestTextPartSizeAndLinesUseEncodedBody covers the quoted-printable half:
// both Size and Lines are counted over the transfer-encoded body.
func TestTextPartSizeAndLinesUseEncodedBody(t *testing.T) {
	// "=C3=A9" is a single decoded rune but six encoded bytes; the soft
	// line break "=\r\n" is an encoded line that vanishes on decode.
	qpBody := "caf=C3=A9 and a soft=\r\n break\r\nsecond line\r\n"

	raw := []byte("From: s@size.invalid\r\n" +
		"Subject: qp\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		qpBody)

	msg, err := New(DefaultLimits()).Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	bs := msg.BodyStructure
	if got, want := bs.Size, int64(len(qpBody)); got != want {
		t.Errorf("Size = %d, want %d (the encoded body length)", got, want)
	}
	// The decoded text is shorter, so a decoded-length Size would differ.
	if int64(len(msg.TextBody)) == bs.Size {
		t.Errorf("Size %d equals the decoded text length; it should be the encoded length", bs.Size)
	}
	// Lines counts the encoded body: three CRLF-terminated lines.
	if bs.Lines != 3 {
		t.Errorf("Lines = %d, want 3 (encoded lines, including the soft break)", bs.Lines)
	}
}

// TestEmbeddedMessageLinesUseEncodedContainer pins the message/rfc822 half of
// the fix.
func TestEmbeddedMessageLinesUseEncodedContainer(t *testing.T) {
	inner := "From: original@rfc822.invalid\r\n" +
		"Subject: the original\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"inner line one\r\n" +
		"inner line two\r\n"

	msg, err := New(DefaultLimits()).Parse(buildForward(inner))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(msg.BodyStructure.Parts) != 2 {
		t.Fatalf("Parts = %d, want 2", len(msg.BodyStructure.Parts))
	}
	fwd := msg.BodyStructure.Parts[1]
	if fwd.Type != "message" || fwd.Subtype != "rfc822" {
		t.Fatalf("part 2 = %s/%s, want message/rfc822", fwd.Type, fwd.Subtype)
	}
	// 7bit: encoded and decoded coincide, so this asserts the shape rather
	// than a delta — Size is the container's own byte count, not zero and
	// not the nested part's.
	if fwd.Size <= 0 {
		t.Errorf("message/rfc822 Size = %d, want the container byte count", fwd.Size)
	}
	if fwd.Lines <= 0 {
		t.Errorf("message/rfc822 Lines = %d, want the container line count", fwd.Lines)
	}
}
