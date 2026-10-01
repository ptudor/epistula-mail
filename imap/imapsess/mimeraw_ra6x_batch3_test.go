package imapsess

import (
	"bytes"
	"strings"
	"testing"
)

// TestRawPartReturnsEncodedBytes is the RA6X-005 read half: BODY[n] must be
// the bytes the sender sent, and BODY[n.MIME] must still carry the
// Content-Transfer-Encoding that explains them.
//
// mime/multipart's NextPart decoded quoted-printable bodies and deleted the
// CTE header, so the server returned already-decoded bytes and told the client
// they were unencoded. A client that decodes by the MIME headers then decodes
// `=3D` a second time and corrupts the payload.
func TestRawPartReturnsEncodedBytes(t *testing.T) {
	encoded := "price=3D3D10 and a soft break=\r\ncontinues"
	raw := []byte("From: s@x.invalid\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n" +
		"\r\n" +
		"--b\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		encoded + "\r\n" +
		"--b--\r\n")

	e, err := resolveRawPart(raw, []int{1})
	if err != nil {
		t.Fatalf("resolveRawPart: %v", err)
	}
	if string(e.body) != encoded {
		t.Errorf("BODY[1]\n got: %q\nwant: %q", e.body, encoded)
	}
	if !bytes.Contains(e.header, []byte("Content-Transfer-Encoding: quoted-printable")) {
		t.Errorf("BODY[1.MIME] lost the transfer encoding: %q", e.header)
	}
}

// TestHeaderBytesArePreservedVerbatim is the RA6X-027 regression: a returned
// header section must be a slice of the original message, with the sender's
// own capitalisation, ordering and folding.
//
// Round-tripping through textproto sorted field names, canonicalised their
// spelling and unfolded continuation lines, so a DKIM-Signature came back on
// one line in a different position and no section was reconstructible.
func TestHeaderBytesArePreservedVerbatim(t *testing.T) {
	header := "X-second: 2\r\n" +
		"MESSAGE-ID: <weird-caps@x.invalid>\r\n" +
		"DKIM-Signature: v=1; a=rsa-sha256;\r\n" +
		"\tbh=abc;\r\n" +
		"\tb=def\r\n" +
		"X-first: 1\r\n" +
		"X-second: 2-again\r\n"
	raw := []byte(header + "\r\nbody\r\n")

	e, err := resolveRawPart(raw, nil)
	if err != nil {
		t.Fatalf("resolveRawPart: %v", err)
	}
	if got, want := string(e.header), header+"\r\n"; got != want {
		t.Errorf("BODY[HEADER]\n got: %q\nwant: %q", got, want)
	}

	// Selecting fields keeps the sender's bytes, order and folding.
	got := string(selectRawFields(e.header, []string{"dkim-signature", "message-id"}, false))
	want := "MESSAGE-ID: <weird-caps@x.invalid>\r\n" +
		"DKIM-Signature: v=1; a=rsa-sha256;\r\n" +
		"\tbh=abc;\r\n" +
		"\tb=def\r\n" +
		"\r\n"
	if got != want {
		t.Errorf("BODY[HEADER.FIELDS]\n got: %q\nwant: %q", got, want)
	}

	// Repeated fields are all kept, in order.
	got = string(selectRawFields(e.header, []string{"x-second"}, false))
	if want := "X-second: 2\r\nX-second: 2-again\r\n\r\n"; got != want {
		t.Errorf("repeated fields\n got: %q\nwant: %q", got, want)
	}

	// The inverse keeps everything else, in original order.
	got = string(selectRawFields(e.header, []string{"X-Second"}, true))
	if strings.Contains(got, "X-second") {
		t.Errorf("excluded field survived: %q", got)
	}
	if !strings.HasPrefix(got, "MESSAGE-ID:") {
		t.Errorf("inverse selection reordered the header: %q", got)
	}
}

// TestLFMessageSplitsAtItsOwnSeparator is the RA6X-006 regression.
//
// The reader preferred the first CRLF CRLF anywhere in the message and only
// fell back to LF LF. Postfix's pipe transport delivers LF-terminated mail, so
// for a message whose body contains a CRLF blank line the reader placed the
// header boundary deep in the body: part of the body became headers and
// BODY[TEXT] was truncated. ingest was corrected to take the EARLIER
// separator, so the writer and the reader disagreed about the same message.
func TestLFMessageSplitsAtItsOwnSeparator(t *testing.T) {
	raw := []byte("Subject: x\n\nfirst body line\r\n\r\nlast body line\n")

	header, body := splitHeaderBody(raw)
	if want := "Subject: x\n\n"; string(header) != want {
		t.Errorf("BODY[HEADER]\n got: %q\nwant: %q", header, want)
	}
	if !strings.Contains(string(body), "first body line") {
		t.Errorf("BODY[TEXT] lost the first body segment: %q", body)
	}
	if !strings.Contains(string(body), "last body line") {
		t.Errorf("BODY[TEXT] lost the last body segment: %q", body)
	}
}

// TestEncapsulatedPartsAreNumberedDirectly is the RA6X-015 regression.
//
// The reader inserted an extra implicit 1 when entering message/rfc822 and
// rejected every other index, so BODYSTRUCTURE — which ingest numbers per
// RFC 9051 §6.4.5 — advertised children FETCH answered with
// "message/rfc822 has only part 1".
func TestEncapsulatedPartsAreNumberedDirectly(t *testing.T) {
	inner := "From: inner@x.invalid\r\n" +
		"Subject: forwarded\r\n" +
		"Content-Type: multipart/mixed; boundary=inner\r\n" +
		"\r\n" +
		"--inner\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"FIRST-ENCLOSED\r\n" +
		"--inner\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"SECOND-ENCLOSED\r\n" +
		"--inner--\r\n"

	raw := []byte("From: s@x.invalid\r\n" +
		"Content-Type: multipart/mixed; boundary=outer\r\n" +
		"\r\n" +
		"--outer\r\n" +
		"Content-Type: message/rfc822\r\n" +
		"\r\n" + inner +
		"--outer--\r\n")

	// 1.1 and 1.2 are the enclosed message's two parts — no extra level.
	for _, tc := range []struct {
		path []int
		want string
	}{
		{[]int{1, 1}, "FIRST-ENCLOSED"},
		{[]int{1, 2}, "SECOND-ENCLOSED"},
	} {
		e, err := resolveRawPart(raw, tc.path)
		if err != nil {
			t.Fatalf("BODY[%v]: %v", tc.path, err)
		}
		if !strings.Contains(string(e.body), tc.want) {
			t.Errorf("BODY[%v] = %q, want it to contain %q", tc.path, e.body, tc.want)
		}
	}

	// Part 1 itself is the container; its HEADER and TEXT address the
	// ENCLOSED message, not the MIME container.
	e, err := resolveRawPart(raw, []int{1})
	if err != nil {
		t.Fatalf("BODY[1]: %v", err)
	}
	if !bytes.Contains(e.header, []byte("Content-Type: message/rfc822")) {
		t.Errorf("BODY[1.MIME] should be the container's own header: %q", e.header)
	}
	inner2, ok := encapsulatedOf(e)
	if !ok {
		t.Fatal("part 1 was not recognised as an encapsulated message")
	}
	if !bytes.Contains(inner2.header, []byte("Subject: forwarded")) {
		t.Errorf("BODY[1.HEADER] should be the enclosed message's header: %q", inner2.header)
	}
	if !bytes.Contains(inner2.body, []byte("FIRST-ENCLOSED")) {
		t.Errorf("BODY[1.TEXT] should be the enclosed message's body: %q", inner2.body)
	}
}

// TestUntypedPartReadsAsTextPlain pins that the reader applies the same
// absent-Content-Type defaults ingest does (RA6X-010), so the two abstractions
// cannot disagree about what a part is.
func TestUntypedPartReadsAsTextPlain(t *testing.T) {
	raw := []byte("Content-Type: multipart/mixed; boundary=b\r\n" +
		"\r\n" +
		"--b\r\n" +
		"\r\n" +
		"untyped\r\n" +
		"--b--\r\n")
	e, err := resolveRawPart(raw, []int{1})
	if err != nil {
		t.Fatalf("resolveRawPart: %v", err)
	}
	if e.ct.mediaType != "text/plain" {
		t.Errorf("untyped part = %q, want text/plain", e.ct.mediaType)
	}

	digest := []byte("Content-Type: multipart/digest; boundary=b\r\n" +
		"\r\n" +
		"--b\r\n" +
		"\r\n" +
		"From: inner@x.invalid\r\n\r\nenclosed\r\n" +
		"--b--\r\n")
	e, err = resolveRawPart(digest, []int{1})
	if err != nil {
		t.Fatalf("resolveRawPart (digest): %v", err)
	}
	if e.ct.mediaType != "message/rfc822" {
		t.Errorf("untyped digest child = %q, want message/rfc822", e.ct.mediaType)
	}
}

// TestRawMultipartHandlesLFTerminatedMail pins that boundary scanning accepts
// the LF-only line endings Postfix's pipe transport delivers.
func TestRawMultipartHandlesLFTerminatedMail(t *testing.T) {
	raw := []byte("Content-Type: multipart/mixed; boundary=b\n" +
		"\n" +
		"--b\n" +
		"Content-Type: text/plain\n" +
		"\n" +
		"lf body\n" +
		"--b--\n")
	e, err := resolveRawPart(raw, []int{1})
	if err != nil {
		t.Fatalf("resolveRawPart: %v", err)
	}
	if got := string(e.body); got != "lf body" {
		t.Errorf("BODY[1] = %q, want %q", got, "lf body")
	}
}
