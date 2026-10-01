package imapsess

import (
	"bytes"
	"errors"
	"fmt"
	"mime"
	"strconv"
	"strings"

	"github.com/ptudor/epistula-mail/database/ingest"
)

// Raw MIME traversal over the stored message bytes.
//
// Sections return original spans, or spans of an explicitly decoded enclosed
// message when traversing a transfer-encoded message/rfc822 container. The
// point (RA6X-005, RA6X-027): the previous implementation walked the tree with
// mime/multipart and re-serialized headers from a parsed textproto.MIMEHeader,
// which
//
//   - silently decoded quoted-printable part bodies and deleted their
//     Content-Transfer-Encoding header, so BODY[n] returned bytes the sender
//     never sent and BODY[n.MIME] said they were unencoded — a client that
//     trusts the MIME headers decodes `=3D` twice and corrupts the payload;
//   - sorted field names, canonicalised their capitalisation and unfolded
//     continuation lines, so a DKIM-Signature came back on one line in a
//     different position, and no section was a slice of the immutable message.
//
// Deterministic output is not the same property as byte fidelity, and only the
// second one lets a client reconstruct or verify what was stored.

// rawEntity is one MIME entity as it appears in the message: its header block
// (including the terminating blank line) and its body, both sub-slices of the
// original bytes.
type rawEntity struct {
	header []byte
	body   []byte
	ct     contentTypeInfo
	// containerSubtype is the subtype of the multipart this entity is a child
	// of, which decides what an absent Content-Type defaults to. Empty for the
	// top-level or an encapsulated message.
	containerSubtype string
}

// entityFrom splits raw into header and body at the FIRST blank line of either
// flavour — the same rule ingest uses, via the same function, so the writer and
// the reader cannot disagree about where a message's headers end (RA6X-006).
func entityFrom(raw []byte, containerSubtype string) (rawEntity, error) {
	header, body := ingest.SplitHeaderBody(raw)
	ct, err := contentTypeOfHeader(header, containerSubtype)
	if err != nil {
		return rawEntity{}, err
	}
	return rawEntity{header: header, body: body, ct: ct, containerSubtype: containerSubtype}, nil
}

// contentTypeOfHeader reads the Content-Type out of a raw header block without
// materialising the whole header map, and applies the RFC defaults for an
// absent field — text/plain, or message/rfc822 inside multipart/digest
// (RA6X-010). Ingest applies the same rule; the two must agree or
// BODYSTRUCTURE advertises a part FETCH cannot address.
func contentTypeOfHeader(header []byte, containerSubtype string) (contentTypeInfo, error) {
	value := rawHeaderValue(header, "content-type")
	if strings.TrimSpace(value) == "" {
		mt, params := defaultMediaType(containerSubtype)
		return contentTypeInfo{mediaType: mt, params: params}, nil
	}
	mt, params, err := mime.ParseMediaType(value)
	if err != nil {
		// A malformed EXPLICIT type is opaque, matching ingest.
		return contentTypeInfo{mediaType: "application/octet-stream"}, nil
	}
	return contentTypeInfo{mediaType: mt, params: params}, nil
}

func defaultMediaType(containerSubtype string) (string, map[string]string) {
	if strings.EqualFold(containerSubtype, "digest") {
		return "message/rfc822", nil
	}
	return "text/plain", map[string]string{"charset": "us-ascii"}
}

// rawHeaderValue returns the unfolded value of the named field from a raw
// header block, matching case-insensitively. Only the FIRST occurrence is
// returned, which is what every Content-* consumer wants.
func rawHeaderValue(header []byte, lowerName string) string {
	for _, f := range rawHeaderFields(header) {
		if strings.EqualFold(f.name, lowerName) {
			return f.unfoldedValue(header)
		}
	}
	return ""
}

// rawField is one header field as a span of the original header block. start
// and end bracket the complete field INCLUDING its continuation lines and its
// terminating line break, so selecting fields is a matter of copying spans.
type rawField struct {
	name  string
	start int
	end   int
	// colon is the offset of the ':' within the block, so a value can be
	// extracted without re-parsing.
	colon int
}

// unfoldedValue returns the field's value with folding removed, for the few
// callers that need to interpret it rather than reproduce it.
func (f rawField) unfoldedValue(header []byte) string {
	if f.colon < 0 || f.colon+1 > f.end {
		return ""
	}
	raw := header[f.colon+1 : f.end]
	// Replace CRLF/LF + WSP with a single space, per RFC 5322 unfolding.
	var b strings.Builder
	b.Grow(len(raw))
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c == '\r' {
			continue
		}
		if c == '\n' {
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(c)
	}
	return strings.TrimSpace(b.String())
}

// rawHeaderFields scans a raw header block and returns one span per field, in
// the order they appear. A continuation line (leading space or tab) extends the
// field above it rather than starting a new one.
//
// The terminating blank line is not a field and is excluded.
func rawHeaderFields(header []byte) []rawField {
	var fields []rawField
	i := 0
	for i < len(header) {
		lineEnd := bytes.IndexByte(header[i:], '\n')
		var next int
		if lineEnd < 0 {
			lineEnd = len(header)
			next = len(header)
		} else {
			lineEnd = i + lineEnd
			next = lineEnd + 1
		}
		line := header[i:lineEnd]
		trimmed := bytes.TrimRight(line, "\r")

		// The blank line ends the header section.
		if len(trimmed) == 0 {
			break
		}

		if line[0] == ' ' || line[0] == '\t' {
			// Continuation of the previous field.
			if n := len(fields); n > 0 {
				fields[n-1].end = next
			}
			i = next
			continue
		}

		colon := bytes.IndexByte(line, ':')
		if colon < 0 {
			// Not a field at all (a malformed line). Attach it to the previous
			// field so it is neither lost nor mistaken for a new one.
			if n := len(fields); n > 0 {
				fields[n-1].end = next
			}
			i = next
			continue
		}
		fields = append(fields, rawField{
			name:  string(bytes.TrimSpace(line[:colon])),
			start: i,
			end:   next,
			colon: i + colon,
		})
		i = next
	}
	return fields
}

// splitRawMultipart returns each child of a multipart body as its raw bytes,
// exactly as they appear between the boundary delimiters.
//
// RFC 2046 §5.1.1: the delimiter is a line beginning "--<boundary>", the close
// delimiter appends "--", and the line break preceding a delimiter belongs to
// the delimiter rather than to the part. Transport padding (trailing
// whitespace) is permitted after the boundary. Both CRLF and bare LF line
// endings are accepted, because Postfix's pipe transport delivers LF.
func splitRawMultipart(body []byte, boundary string) ([][]byte, error) {
	var parts [][]byte
	err := ingest.VisitRawMultipart(body, boundary, func(raw []byte) error { parts = append(parts, raw); return nil })
	return parts, err
}

// errPartNotFound is returned when the requested MIME part path does not
// resolve to an existing part in the raw message.
var errPartNotFound = errors.New("imapsess: MIME part not found")

// contentTypeInfo is the parsed Content-Type header of an entity.
type contentTypeInfo struct {
	mediaType string
	params    map[string]string
}

// resolveRawPart returns the entity addressed by an IMAP part path, as raw
// byte spans of the message.
//
// RFC 9051 §6.4.5 numbers the parts of a MESSAGE, the top-level one or one a
// message/rfc822 part encloses, in one of two ways:
//
//   - a multipart message's parts are the children of its body, 1 to n;
//   - any other message has exactly one part, number 1: its body. The MIME
//     header of that part is the message's own header, which is where its
//     Content-Type is.
//
// Below a part, a multipart part's children are numbered under it, and a
// message/rfc822 part's enclosed message has its parts numbered directly
// under the part's number (RA6X-015). Entering the enclosed message consumes
// no index; choosing one of its parts does. Ingest numbers parts by the same
// two rules (ingest.messageBodyNumber), and a client derives the same numbers
// from BODYSTRUCTURE.
//
// A message whose body is itself message/rfc822, such as a forward of a
// forward with no multipart around it, therefore spends one index at every
// level (OPS-004): its body is part 1, and the message that body encloses has
// its parts under 1. The previous loop entered every message/rfc822 it met
// without consuming an index, so it passed through consecutive enclosed
// messages in one step and N.1 returned the innermost message's body instead
// of the middle message.
func resolveRawPart(raw []byte, path []int) (rawEntity, error) {
	cur, err := entityFrom(raw, "")
	if err != nil {
		return rawEntity{}, err
	}

	for i, idx := range path {
		if idx < 1 {
			return rawEntity{}, fmt.Errorf("%w: invalid part index %d", errPartNotFound, idx)
		}
		// Every index after the first chooses a part below the part the
		// previous index chose. A multipart part has children; a
		// message/rfc822 part has the parts of the message it encloses; no
		// other part has any.
		if i > 0 && !ingest.IsMultipart(cur.ct.mediaType) {
			if cur.ct.mediaType != "message/rfc822" {
				return rawEntity{}, fmt.Errorf("%w: %s part %s has no parts below it",
					errPartNotFound, cur.ct.mediaType, partIntPath(path[:i]))
			}
			if cur, err = enterEncapsulated(cur); err != nil {
				return rawEntity{}, err
			}
		}

		// cur is now a message or a multipart part, and idx is one of its parts.
		if ingest.IsMultipart(cur.ct.mediaType) {
			children, err := splitRawMultipart(cur.body, cur.ct.params["boundary"])
			if err != nil {
				return rawEntity{}, err
			}
			if idx > len(children) {
				return rawEntity{}, fmt.Errorf("%w: index %d (only %d parts)", errPartNotFound, idx, len(children))
			}
			_, subtype, _ := strings.Cut(cur.ct.mediaType, "/")
			if cur, err = entityFrom(children[idx-1], subtype); err != nil {
				return rawEntity{}, err
			}
			continue
		}
		// A message that is not multipart has its body as its only part. That
		// part has the message's header as its MIME header and the message's
		// body and type, so it is the entity cur already holds.
		if idx != 1 {
			return rawEntity{}, fmt.Errorf("%w: a %s message has only part 1", errPartNotFound, cur.ct.mediaType)
		}
	}
	return cur, nil
}

// partIntPath converts an IMAP part-spec []int to a human form "2.1.3"
// for logging.
func partIntPath(p []int) string {
	if len(p) == 0 {
		return ""
	}
	parts := make([]string, len(p))
	for i, n := range p {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ".")
}

// Transfer decoding applies only when entering the enclosed message. The
// enclosing N and N.MIME sections retain their original encoded bytes.
func enterEncapsulated(e rawEntity) (rawEntity, error) {
	raw, err := ingest.DecodeEncapsulated(e.body, rawHeaderValue(e.header, "content-transfer-encoding"))
	if err != nil {
		return rawEntity{}, err
	}
	return entityFrom(raw, "")
}

// encapsulatedOf returns the message inside a message/rfc822 entity, for the
// N.HEADER and N.TEXT specifiers, which address the ENCLOSED RFC 5322 message
// rather than the MIME container (RA6X-015).
func encapsulatedOf(e rawEntity) (rawEntity, bool) {
	if e.ct.mediaType != "message/rfc822" {
		return rawEntity{}, false
	}
	inner, err := enterEncapsulated(e)
	if err != nil {
		return rawEntity{}, false
	}
	return inner, true
}

// selectRawFields returns the named header fields as EXACT spans of the
// original block, in their original order and folding, followed by the
// protocol-required blank line (RA6X-027).
//
// invert excludes the named fields instead. Names match case-insensitively per
// RFC 5322; the spelling that goes out is the sender's, not a canonicalised
// one, because the point is that the client can reconstruct the message.
func selectRawFields(header []byte, names []string, invert bool) []byte {
	wanted := make(map[string]struct{}, len(names))
	for _, n := range names {
		wanted[strings.ToLower(n)] = struct{}{}
	}
	var out bytes.Buffer
	for _, f := range rawHeaderFields(header) {
		_, named := wanted[strings.ToLower(f.name)]
		if named == invert {
			continue
		}
		out.Write(header[f.start:f.end])
	}
	out.WriteString("\r\n")
	return out.Bytes()
}
