package imapsess

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// These cases were written for walkPart, a second resolver that FETCH no
// longer used and that still numbered enclosed messages the pre-RA6X-015 way.
// It was retired in OPS-004. The cases now pin resolveRawPart, which serves
// every numbered BODY[] section.

// flatMessage is a singlepart text/plain message.
const flatMessage = "From: a@x\r\n" +
	"Subject: flat\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"hello world\r\n"

// twoPartAlt is a multipart/alternative message with text + html parts.
const twoPartAlt = "From: a@x\r\n" +
	"Subject: alt\r\n" +
	`Content-Type: multipart/alternative; boundary="bdy"` + "\r\n" +
	"\r\n" +
	"--bdy\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"plain version\r\n" +
	"--bdy\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<p>html version</p>\r\n" +
	"--bdy--\r\n"

// nestedMixed has a multipart/mixed wrapping a multipart/alternative
// and an attachment, so a part path like [1,2] reaches the html half.
const nestedMixed = "From: a@x\r\n" +
	`Content-Type: multipart/mixed; boundary="outer"` + "\r\n" +
	"\r\n" +
	"--outer\r\n" +
	`Content-Type: multipart/alternative; boundary="inner"` + "\r\n" +
	"\r\n" +
	"--inner\r\n" +
	"Content-Type: text/plain\r\n" +
	"\r\n" +
	"plain inside\r\n" +
	"--inner\r\n" +
	"Content-Type: text/html\r\n" +
	"\r\n" +
	"<p>html inside</p>\r\n" +
	"--inner--\r\n" +
	"--outer\r\n" +
	"Content-Type: application/octet-stream\r\n" +
	"Content-Disposition: attachment; filename=blob.bin\r\n" +
	"\r\n" +
	"binary-data-here\r\n" +
	"--outer--\r\n"

func TestRawPartTopLevel(t *testing.T) {
	e, err := resolveRawPart([]byte(flatMessage), nil)
	if err != nil {
		t.Fatalf("resolveRawPart top: %v", err)
	}
	if !strings.HasPrefix(string(e.header), "From: a@x") {
		t.Errorf("top header missing From: %q", e.header)
	}
	if string(e.body) != "hello world\r\n" {
		t.Errorf("top body = %q, want %q", e.body, "hello world\r\n")
	}
}

// TestRawPartSinglepartBodyIsPartOne pins RFC 9051 §6.4.5 for a message that
// is not multipart: its body is part 1, and the MIME header of part 1 is the
// message's own header, which holds the part's Content-Type (OPS-004).
func TestRawPartSinglepartBodyIsPartOne(t *testing.T) {
	e, err := resolveRawPart([]byte(flatMessage), []int{1})
	if err != nil {
		t.Fatalf("resolveRawPart [1]: %v", err)
	}
	if string(e.body) != "hello world\r\n" {
		t.Errorf("BODY[1] = %q, want %q", e.body, "hello world\r\n")
	}
	header, _ := splitHeaderBody([]byte(flatMessage))
	if !bytes.Equal(e.header, header) {
		t.Errorf("BODY[1.MIME] = %q, want the message header %q", e.header, header)
	}
}

func TestRawPartMultipartIndex(t *testing.T) {
	// Part 1 = text/plain.
	e, err := resolveRawPart([]byte(twoPartAlt), []int{1})
	if err != nil {
		t.Fatalf("resolveRawPart [1]: %v", err)
	}
	if !bytes.Contains(e.header, []byte("text/plain")) {
		t.Errorf("part 1 header should mention text/plain: %q", e.header)
	}
	if string(e.body) != "plain version" {
		t.Errorf("part 1 body = %q, want %q", e.body, "plain version")
	}

	// Part 2 = text/html.
	e, err = resolveRawPart([]byte(twoPartAlt), []int{2})
	if err != nil {
		t.Fatalf("resolveRawPart [2]: %v", err)
	}
	if !bytes.Contains(e.header, []byte("text/html")) {
		t.Errorf("part 2 header should mention text/html: %q", e.header)
	}
	if string(e.body) != "<p>html version</p>" {
		t.Errorf("part 2 body = %q, want %q", e.body, "<p>html version</p>")
	}
}

func TestRawPartNestedMultipart(t *testing.T) {
	// outer.1.2 = the inner multipart's html sub-part.
	e, err := resolveRawPart([]byte(nestedMixed), []int{1, 2})
	if err != nil {
		t.Fatalf("resolveRawPart [1,2]: %v", err)
	}
	if !bytes.Contains(e.header, []byte("text/html")) {
		t.Errorf("nested part 1.2 header should mention text/html: %q", e.header)
	}
	if string(e.body) != "<p>html inside</p>" {
		t.Errorf("nested part 1.2 body = %q, want %q", e.body, "<p>html inside</p>")
	}

	// outer.2 = the attachment.
	e, err = resolveRawPart([]byte(nestedMixed), []int{2})
	if err != nil {
		t.Fatalf("resolveRawPart [2]: %v", err)
	}
	if !bytes.Contains(e.header, []byte("application/octet-stream")) {
		t.Errorf("attachment header should mention octet-stream: %q", e.header)
	}
	if string(e.body) != "binary-data-here" {
		t.Errorf("attachment body = %q, want %q", e.body, "binary-data-here")
	}
}

func TestRawPartOutOfRange(t *testing.T) {
	_, err := resolveRawPart([]byte(twoPartAlt), []int{3})
	if !errors.Is(err, errPartNotFound) {
		t.Errorf("resolveRawPart [3]: err = %v, want errPartNotFound", err)
	}
}

func TestRawPartInvalidIndex(t *testing.T) {
	_, err := resolveRawPart([]byte(twoPartAlt), []int{0})
	if !errors.Is(err, errPartNotFound) {
		t.Errorf("resolveRawPart [0]: err = %v, want errPartNotFound", err)
	}
}

func TestRawPartTooDeepOnSinglepart(t *testing.T) {
	// flatMessage is text/plain: part 1 is its body, and a text part has
	// no parts below it.
	for _, path := range [][]int{{1, 1}, {2}} {
		_, err := resolveRawPart([]byte(flatMessage), path)
		if !errors.Is(err, errPartNotFound) {
			t.Errorf("resolveRawPart flat %v: err = %v, want errPartNotFound", path, err)
		}
	}
}

func TestPartIntPath(t *testing.T) {
	if got := partIntPath(nil); got != "" {
		t.Errorf("empty: got %q, want \"\"", got)
	}
	if got := partIntPath([]int{2, 1, 3}); got != "2.1.3" {
		t.Errorf("got %q, want %q", got, "2.1.3")
	}
}
