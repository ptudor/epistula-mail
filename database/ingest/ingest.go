// Package ingest parses RFC 5322 messages and recursively decodes their MIME
// structure into the form epistula-database stores: a flat header map (JSONB),
// decoded text/plain and text/html bodies, attachments as content-addressed
// candidates, and a precomputed IMAP BODYSTRUCTURE. The parser is hardened
// against zip-bomb, deep-nesting, oversize-header, and oversize-message
// pathologies via Limits; the caller (typically the LDA path) is expected to
// have already capped the input bytes before calling Parse.
//
// Malformed mail is read the way mail clients read it rather than refused
// (OPS-003). The raw bytes are stored verbatim and served for BODY[], so a
// malformation can only cost derived data: a problem confined to one MIME
// entity degrades that entity and is recorded as a defect on its
// BodyStructure node. Resource limits are the exception. A Parser from New
// refuses a message that exceeds one; a Parser from NewSalvaging, for mail
// that is already ours, stops at the limit and records that instead.
package ingest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/mail"
	"net/textproto"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits caps parser resource use per call. See DefaultLimits.
type Limits struct {
	MaxMessageBytes       int64
	MaxMimeDepth          int
	MaxMimeParts          int
	MaxHeaderBytes        int
	MaxHeaderSectionBytes int
	MaxTransferExpansion  int
}

// DefaultLimits returns the production defaults documented in CLAUDE.md.
func DefaultLimits() Limits {
	return Limits{
		MaxMessageBytes:       52 << 20,
		MaxMimeDepth:          10,
		MaxMimeParts:          200,
		MaxHeaderBytes:        16 << 10,
		MaxHeaderSectionBytes: 256 << 10,
		MaxTransferExpansion:  10,
	}
}

// Errors. The LDA path maps these to sysexit codes.
var (
	ErrTooLarge       = errors.New("ingest: message exceeds MaxMessageBytes")
	ErrMalformed      = errors.New("ingest: malformed RFC 5322 message")
	ErrTooDeep        = errors.New("ingest: MIME depth exceeds MaxMimeDepth")
	ErrTooManyParts   = errors.New("ingest: MIME parts exceed MaxMimeParts")
	ErrHeaderTooLarge = errors.New("ingest: single header exceeds MaxHeaderBytes")
	ErrHeadersTooBig  = errors.New("ingest: header section exceeds MaxHeaderSectionBytes")
	ErrZipBomb        = errors.New("ingest: transfer-decode expansion exceeds MaxTransferExpansion")
)

// Message is the ingested representation of one RFC 5322 message.
type Message struct {
	// Raw bytes (caller will pass to blob.Store.NewWriter).
	Raw       []byte
	RawSize   int64
	SHA256Hex string

	// All headers as parsed. Keys are textproto-canonical (e.g., "Message-Id").
	// Values are RFC 2047 encoded-word decoded where applicable.
	Headers map[string][]string

	// Convenience header projections. Empty if absent or unparseable.
	MessageID string
	InReplyTo string
	Subject   string
	From      string
	To        []string
	Cc        []string
	SentDate  time.Time
	// SentDateLocal is the CALENDAR DATE the sender wrote, in the Date
	// header's own offset — "2026-09-04", not an instant (RA6X-048).
	//
	// SentDate is normalized to an instant, so the sender's offset is gone by
	// the time anything reads it: a header dated `04 Sep 2026 00:30:00 +1400`
	// is September 3 in UTC, and RFC 9051 §6.4.4's sent-date criteria are
	// defined against the date the sender wrote, disregarding time and zone.
	// Empty when the message has no Date header or an unparseable one.
	SentDateLocal string

	// Decoded body projections. TextBody is text/plain concat, or an HTML→text
	// projection if the message has only text/html. HTMLBody is text/html.
	TextBody string
	HTMLBody string

	// Attachments captured as content-addressed candidates. The caller writes
	// Data to blob.Store with KindAttachment, persists the resulting sha256
	// and the rest of the metadata in PG.
	Attachments []Attachment

	// BodyStructure is the precomputed IMAP BODYSTRUCTURE, persisted as JSONB.
	BodyStructure BodyStructure

	// Defects lists every problem the parser worked around, one
	// "<where>: <kind>[: <detail>]" line each, where is "message" or
	// "part <n>" (OPS-003). Empty for a message that parsed cleanly. The
	// same defects are persisted on the BodyStructure nodes they belong to;
	// this flat list is what delivery logging reads.
	Defects []string
}

// Defect kinds recorded on BodyStructure nodes (OPS-003). They are persisted
// and operators query for them, so the strings are stable: never rename one.
const (
	// DefectMissingCloseDelimiter: a multipart body ended without its close
	// delimiter, so its last part runs to the end of the body, as mail
	// clients read it. Usually a truncated message.
	DefectMissingCloseDelimiter = "missing_close_delimiter"
	// DefectNoBodyParts: a multipart body has no delimiter line at all.
	DefectNoBodyParts = "no_body_parts"
	// DefectMissingBoundary: a multipart Content-Type without a boundary
	// parameter, so its parts cannot be found.
	DefectMissingBoundary = "missing_boundary"
	// DefectUnparseableHeader: a MIME part's header block could not be
	// parsed, so the part is kept as opaque bytes.
	DefectUnparseableHeader = "unparseable_header"
	// DefectUnparseableMessage: an enclosed message/rfc822 could not be
	// walked, so it is kept as one opaque attachment.
	DefectUnparseableMessage = "unparseable_enclosed_message"
	// DefectUndecodableBody: a body its declared transfer encoding cannot
	// decode. Whatever decoded before the error still feeds the text
	// projection; no attachment is derived from it.
	DefectUndecodableBody = "undecodable_body"

	// The remaining kinds are recorded only when salvaging: each marks a
	// resource limit that was reached and the data it cost.

	// DefectDepthExceeded: an entity nested deeper than MaxMimeDepth is kept
	// as an opaque leaf and not descended into.
	DefectDepthExceeded = "mime_depth_exceeded"
	// DefectPartsExceeded: MaxMimeParts ran out, so the parts after it are
	// not listed.
	DefectPartsExceeded = "mime_parts_exceeded"
	// DefectHeaderFieldTooLarge: fields over MaxHeaderBytes were left out of
	// the parsed headers.
	DefectHeaderFieldTooLarge = "header_field_too_large"
	// DefectHeaderSectionTooLarge: fields past MaxHeaderSectionBytes were
	// left out of the parsed headers.
	DefectHeaderSectionTooLarge = "header_section_too_large"
	// DefectDecodeLimitExceeded: decoding a body would exceed the transfer
	// expansion cap or the shared decode budget, so nothing is derived from
	// it.
	DefectDecodeLimitExceeded = "decode_limit_exceeded"
)

// Attachment captures one non-text MIME part for blob storage.
type Attachment struct {
	PartNumber  string // IMAP part path, e.g. "2.1"
	Filename    string // metadata only; NEVER used as a disk path
	ContentType string
	ContentID   string
	Disposition string // "inline" or "attachment"
	Size        int64
	Data        []byte
}

// BodyStructure mirrors the MIME tree in a form serializable to JSONB.
type BodyStructure struct {
	Type        string            `json:"type"`
	Subtype     string            `json:"subtype"`
	Params      map[string]string `json:"params,omitempty"`
	Encoding    string            `json:"encoding,omitempty"`
	ContentID   string            `json:"content_id,omitempty"`
	Size        int64             `json:"size"`
	Lines       int               `json:"lines,omitempty"`
	Disposition *BodyDisposition  `json:"disposition,omitempty"`
	Parts       []BodyStructure   `json:"parts,omitempty"`
	// Envelope is set only on a message/rfc822 part whose encapsulated
	// message parsed, and carries that message's own envelope headers
	// (RA6X-025).
	//
	// IMAP's BODYSTRUCTURE for a message/rfc822 body part includes the
	// enclosed message's envelope (RFC 9051 §7.5.2), and the reader emitted
	// NIL there because the persisted JSON had nowhere to put it — so a client
	// showing a forwarded message's sender and subject in a list had nothing
	// to show. The field is additive and omitempty, so an older row simply
	// has no envelope and behaves exactly as before.
	Envelope *BodyEnvelope `json:"envelope,omitempty"`
	// Defects records what the parser had to work around to read THIS
	// entity (OPS-003): each entry is "<kind>" or "<kind>: <detail>", kind
	// one of the Defect* constants. Details never quote message content.
	// Additive and omitempty like Envelope: a clean entity, and every row
	// written before the field existed, has none, and the IMAP reader
	// ignores it.
	Defects []string `json:"defects,omitempty"`
}

// BodyEnvelope is the subset of an encapsulated message's headers that IMAP's
// BODYSTRUCTURE envelope carries. Values are the RAW header values, unparsed,
// so the reader applies exactly the same address parsing it applies to a
// top-level message and the two cannot drift.
type BodyEnvelope struct {
	Date      string `json:"date,omitempty"`
	Subject   string `json:"subject,omitempty"`
	From      string `json:"from,omitempty"`
	Sender    string `json:"sender,omitempty"`
	ReplyTo   string `json:"reply_to,omitempty"`
	To        string `json:"to,omitempty"`
	Cc        string `json:"cc,omitempty"`
	Bcc       string `json:"bcc,omitempty"`
	InReplyTo string `json:"in_reply_to,omitempty"`
	MessageID string `json:"message_id,omitempty"`
}

// BodyDisposition is the parsed Content-Disposition.
type BodyDisposition struct {
	Type   string            `json:"type"`
	Params map[string]string `json:"params,omitempty"`
}

// Parser holds the limits for one or more Parse calls.
type Parser struct {
	limits Limits
	// salvage selects what reaching a resource limit does: refuse the
	// message (New) or record a defect and stop there (NewSalvaging).
	salvage bool
}

// New returns a Parser for mail arriving from outside: live delivery and IMAP
// APPEND. A message that exceeds any limit is refused with that limit's error,
// which the LDA turns into a bounce.
func New(limits Limits) *Parser { return &Parser{limits: limits} }

// NewSalvaging returns a Parser for mail that is already ours: the Maildir and
// blob importers and the reparse back-fill (OPS-003).
//
// Refusing a message there does not bounce anything back to a sender. The
// message is simply left out of the store. So instead of refusing, the parser
// stops at a limit, keeps what it read within the limits, and records a
// defect. No limit is raised: nothing past a limit is read or allocated.
//
// The top-level header is the one place that cannot be salvaged in place.
// The message is still refused when a structural field (Content-Type,
// Content-Transfer-Encoding) exceeds a header limit, or when an over-limit
// field is one net/mail would reject: the IMAP reader builds ENVELOPE and
// walks the parts from those exact bytes.
func NewSalvaging(limits Limits) *Parser { return &Parser{limits: limits, salvage: true} }

// Parse consumes raw RFC 5322 bytes and returns the ingested message.
// The caller MUST cap input at limits.MaxMessageBytes; Parse double-checks.
func (p *Parser) Parse(raw []byte) (*Message, error) {
	if int64(len(raw)) > p.limits.MaxMessageBytes {
		return nil, ErrTooLarge
	}

	walker := &mimeWalker{limits: p.limits, salvage: p.salvage, remaining: p.limits.MaxMessageBytes}

	// The message's own header must parse in every mode. The IMAP reader
	// builds ENVELOPE by running net/mail over these same bytes at FETCH time
	// and fails the FETCH when they do not parse, so storing such a message
	// would make its folder unreadable rather than salvage it.
	top, err := walker.readEntity(raw, true)
	if err != nil {
		return nil, err
	}

	sum := sha256.Sum256(raw)
	out := &Message{
		Raw:       raw,
		RawSize:   int64(len(raw)),
		SHA256Hex: hex.EncodeToString(sum[:]),
		Headers:   make(map[string][]string, len(top.header)),
	}

	wd := &mime.WordDecoder{CharsetReader: charsetReader}

	// Iterate the wire keys in SORTED order, not map order.
	//
	// msg.Header is a Go map, and two DISTINCT wire keys can sanitize to the
	// same string — "X-Key" and "X-K\x00ey" both become "X-Key" (verified;
	// note "X-BadKey" vs "X-Bad\x00Key" does NOT collide, because textproto
	// canonicalizes those to differently-cased keys). The merge-append below
	// then produced a VALUE ORDER that depended on map-iteration order, which
	// Go randomizes per run — so parsing the identical bytes twice yielded
	// different JSONB. That breaks byte-comparison workflows like
	// import-verify and makes epistula-api's headers passthrough irreproducible.
	//
	// Confirmed empirically: before this, a 500-iteration parse of one fixture
	// flipped ["first-value","second-value"] to the reverse within ~10 runs
	// (RO5X-045). Sorting the wire keys makes the merge order a deterministic
	// function of the input.
	wireKeys := make([]string, 0, len(top.header))
	for k := range top.header {
		wireKeys = append(wireKeys, k)
	}
	sort.Strings(wireKeys)

	for _, k := range wireKeys {
		vs := top.header[k]
		// Sanitize the key too, not just values (R-053): a NUL in a header
		// *name* would otherwise reach json.Marshal as a raw byte and Postgres
		// rejects the NUL escape even in an otherwise-valid JSONB key, failing
		// the Ingest INSERT as a non-retryable EX_SOFTWARE. SanitizeUTF8 leaves
		// clean canonical keys untouched (casing preserved).
		key := SanitizeUTF8(k)
		decoded := make([]string, 0, len(vs))
		for _, v := range vs {
			d, derr := wd.DecodeHeader(v)
			if derr != nil {
				d = v
			}
			// Un-encoded 8-bit header values (no encoded-words at all) pass
			// DecodeHeader untouched with a nil error, so every value gets
			// sanitized before it can reach a TEXT/JSONB column.
			decoded = append(decoded, SanitizeUTF8(d))
		}
		// Merge-append rather than assign: two distinct wire keys can collapse
		// to the same sanitized string (e.g. "X-Bad\x00Key" and "X-BadKey"),
		// and overwriting would silently drop one key's values.
		out.Headers[key] = append(out.Headers[key], decoded...)
	}
	populateConveniences(out)

	ct := top.header.Get("Content-Type")
	if strings.TrimSpace(ct) == "" {
		ct = "text/plain; charset=us-ascii"
	}
	bs, err := walker.walk(
		bodyPartNumber("", ct),
		ct,
		top.header.Get("Content-Transfer-Encoding"),
		top.header.Get("Content-Disposition"),
		top.header.Get("Content-ID"),
		top.body,
		0,
	)
	if err != nil {
		return nil, err
	}
	if len(top.defects) > 0 {
		bs.Defects = append(top.defects, bs.Defects...)
	}

	out.TextBody = SanitizeUTF8(strings.TrimSpace(walker.textBuf.String()))
	out.HTMLBody = SanitizeUTF8(strings.TrimSpace(walker.htmlBuf.String()))
	if out.TextBody == "" && out.HTMLBody != "" {
		out.TextBody = htmlToText(out.HTMLBody)
	}
	out.Attachments = walker.attachments
	out.BodyStructure = bs
	out.Defects = collectDefects(bs)
	return out, nil
}

// maxDefectSummary bounds DefectSummary, so a message with hundreds of broken
// parts cannot write an unbounded delivery_log row.
const maxDefectSummary = 1024

// DefectSummary joins Defects into the single line delivery logging records,
// truncated to maxDefectSummary bytes. Empty for a clean message.
func (m *Message) DefectSummary() string {
	s := strings.Join(m.Defects, "; ")
	if len(s) <= maxDefectSummary {
		return s
	}
	cut := maxDefectSummary
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// collectDefects flattens the per-node defects into Message.Defects, labelling
// each with the IMAP part path of the node it belongs to. The paths follow the
// walker's numbering: a multipart child appends its index, and the body of a
// message, the top-level one or an enclosed one, is numbered by
// messageBodyNumber. The top-level body's own defects are the message's.
func collectDefects(bs BodyStructure) []string {
	var out []string
	var visit func(n BodyStructure, path, label string)
	visit = func(n BodyStructure, path, label string) {
		for _, d := range n.Defects {
			out = append(out, label+": "+d)
		}
		if n.Type == "message" && n.Subtype == "rfc822" {
			if len(n.Parts) == 1 {
				nested := n.Parts[0]
				p := messageBodyNumber(path, nested.Type == "multipart")
				visit(nested, p, "part "+p)
			}
			return
		}
		for i, child := range n.Parts {
			p := childPath(path, i+1)
			visit(child, p, "part "+p)
		}
	}
	visit(bs, messageBodyNumber("", bs.Type == "multipart"), "message")
	return out
}

// bodyPartNumber is the IMAP part number the walker gives a message's body,
// whose Content-Type is contentType. prefix is the number the message's parts
// are numbered under: "" for the top-level message, and for an enclosed
// message the number of the message/rfc822 part that encloses it (RA6X-015).
func bodyPartNumber(prefix, contentType string) string {
	mediatype, _, err := mime.ParseMediaType(contentType)
	return messageBodyNumber(prefix, err == nil && IsMultipart(mediatype))
}

// messageBodyNumber applies RFC 9051 §6.4.5, which numbers a message's parts
// in one of two ways. A multipart body has no number of its own: its children
// are prefix.1, prefix.2, and so on. Any other body is the message's only
// part, prefix.1. That includes a body that is itself message/rfc822, so a
// forward of a forward spends one number at each level (OPS-004). A top-level
// message whose own Content-Type is message/rfc822 has its enclosed message's
// parts under 1, not directly at 1, 2, and so on.
func messageBodyNumber(prefix string, multipart bool) string {
	if multipart {
		return prefix
	}
	return childPath(prefix, 1)
}

// childPath is the IMAP part path of the i-th (1-based) child under path.
func childPath(path string, i int) string {
	if path == "" {
		return strconv.Itoa(i)
	}
	return path + "." + strconv.Itoa(i)
}

// checkHeaderSection measures raw field content (including continuation
// whitespace) and the complete field block before any header parsing/allocation.
// Line endings are excluded from per-field content limits, as at the top level
// historically; the section bound includes all bytes before the separator.
//
// A header block with no empty line after it is the whole entity: a message or
// MIME part with no body, which RFC 5322 §3.5 and RFC 2046 §5.1.1 both allow.
// It is measured like any other header section rather than refused (OPS-003).
func checkHeaderSection(raw []byte, lim Limits) error {
	header, body := SplitHeaderBody(raw)
	return checkHeaderFields(header[:headerFieldsEnd(header, body != nil)], lim)
}

// headerFieldsEnd returns where the header fields in header end: before the
// line break that terminates the last field and, when terminated is true,
// before the empty line after it. The empty line is "\n" or "\r\n", exactly
// as HeaderSeparatorEnd found it.
func headerFieldsEnd(header []byte, terminated bool) int {
	end := len(header)
	if terminated {
		end-- // the empty line's '\n'
		if end > 0 && header[end-1] == '\r' && (end == 1 || header[end-2] == '\n') {
			end-- // its '\r', when the empty line is a CRLF one
		}
	}
	if end > 0 && header[end-1] == '\n' {
		end--
	}
	if end > 0 && header[end-1] == '\r' {
		end--
	}
	return end
}

// checkHeaderFields enforces the header limits on the field content returned
// by headerFieldsEnd. It allocates nothing, so a refused header costs no more
// than the scan.
func checkHeaderFields(fields []byte, lim Limits) error {
	if len(fields) > lim.MaxHeaderSectionBytes {
		return ErrHeadersTooBig
	}
	currentLen := 0
	for i := 0; i < len(fields); {
		n := bytes.IndexByte(fields[i:], '\n')
		next := len(fields)
		if n >= 0 {
			next = i + n + 1
		}
		line := bytes.TrimSuffix(fields[i:next], []byte("\n"))
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			currentLen += len(line)
		} else {
			currentLen = len(line)
		}
		if currentLen > lim.MaxHeaderBytes {
			return ErrHeaderTooLarge
		}
		i = next
	}
	return nil
}

// headerFieldSpan is one header field as a span of a header block: its first
// line and every continuation line, with their line breaks.
type headerFieldSpan struct {
	start, end int
	// size is the field's content without line breaks, the quantity
	// checkHeaderFields compares with MaxHeaderBytes.
	size int
	// name is the canonical field name, or "" when the field has no
	// printable name before a colon on its first line.
	name string
	// colon reports whether any line of the field has a colon, which is
	// what net/mail requires of a field.
	colon bool
	// orphan marks a continuation line with no field before it: net/mail
	// refuses a header block that starts with one.
	orphan bool
}

// headerFieldSpans splits header field content into fields the same way
// checkHeaderFields measures them: a line that starts with a space or a tab
// continues the field above it.
func headerFieldSpans(fields []byte) []headerFieldSpan {
	var spans []headerFieldSpan
	for i := 0; i < len(fields); {
		next := len(fields)
		if n := bytes.IndexByte(fields[i:], '\n'); n >= 0 {
			next = i + n + 1
		}
		line := bytes.TrimSuffix(fields[i:next], []byte("\n"))
		line = bytes.TrimSuffix(line, []byte("\r"))
		continuation := len(line) > 0 && (line[0] == ' ' || line[0] == '\t')
		colon := bytes.IndexByte(line, ':')
		if continuation && len(spans) > 0 {
			s := &spans[len(spans)-1]
			s.end = next
			s.size += len(line)
			s.colon = s.colon || colon >= 0
		} else {
			f := headerFieldSpan{start: i, end: next, size: len(line), orphan: continuation, colon: colon >= 0}
			if colon >= 0 && !continuation {
				f.name = fieldName(line[:colon])
			}
			spans = append(spans, f)
		}
		i = next
	}
	return spans
}

// fieldName returns a header field name fit to appear in a defect: the
// canonical spelling of a name made only of the printable characters RFC 5322
// allows in one, at most 64 of them. Anything else yields "", so a defect
// never quotes message text.
func fieldName(raw []byte) string {
	name := bytes.TrimRight(raw, " \t")
	if len(name) == 0 || len(name) > 64 {
		return ""
	}
	for _, c := range name {
		if c < '!' || c > '~' || c == ':' {
			return ""
		}
	}
	return textproto.CanonicalMIMEHeaderKey(string(name))
}

// isStructuralField reports whether a header field decides how an entity is
// traversed. Leaving one out would make the stored structure disagree with the
// IMAP reader, which reads it from the raw bytes whatever its size.
func isStructuralField(name string) bool {
	return strings.EqualFold(name, "Content-Type") || strings.EqualFold(name, "Content-Transfer-Encoding")
}

// omittedFields accumulates one kind of omission for a single defect entry.
type omittedFields struct {
	names []string
	count int
	bytes int
}

func (o *omittedFields) add(f headerFieldSpan) {
	o.count++
	o.bytes += f.end - f.start
	if f.name != "" && len(o.names) < 5 {
		o.names = append(o.names, f.name)
	}
}

func (o omittedFields) defect(kind string) string {
	names := strings.Join(o.names, ", ")
	if o.count > len(o.names) {
		if names != "" {
			names += ", "
		}
		names += "…"
	}
	return fmt.Sprintf("%s: %s (%d field(s), %d bytes)", kind, names, o.count, o.bytes)
}

// omitOverLimitFields rebuilds a header block without the fields the header
// limits refuse, so net/mail never reads them (salvage only, OPS-003).
//
// A field over MaxHeaderBytes is left out, and so is any field that would
// take the kept fields past MaxHeaderSectionBytes. The block keeps its
// original ending, so a header-only entity stays header-only. The error is
// non-nil when a field cannot be left out:
//
//   - a structural field (isStructuralField) returns the limit it broke;
//   - a field net/mail would refuse (no colon, or a continuation line with
//     nothing before it) returns a malformed error. Leaving it out would let
//     this parse succeed where the IMAP reader's parse of the same bytes
//     fails.
func omitOverLimitFields(header, fields []byte, lim Limits) ([]byte, []string, error) {
	var (
		kept                  = make([]byte, 0, min(len(header), lim.MaxHeaderSectionBytes+4))
		keptBytes             int
		tooLarge, overSection omittedFields
	)
	for _, f := range headerFieldSpans(fields) {
		span := fields[f.start:f.end]
		var limit error
		switch {
		case f.size > lim.MaxHeaderBytes:
			limit = ErrHeaderTooLarge
			tooLarge.add(f)
		case keptBytes+len(span) > lim.MaxHeaderSectionBytes:
			limit = ErrHeadersTooBig
			overSection.add(f)
		default:
			kept = append(kept, span...)
			keptBytes += len(span)
			continue
		}
		if !f.colon || f.orphan {
			return nil, nil, &malformedError{kind: DefectUnparseableHeader}
		}
		if isStructuralField(f.name) {
			return nil, nil, limit
		}
	}
	kept = append(kept, header[len(fields):]...)

	var defects []string
	if tooLarge.count > 0 {
		defects = append(defects, tooLarge.defect(DefectHeaderFieldTooLarge))
	}
	if overSection.count > 0 {
		defects = append(defects, overSection.defect(DefectHeaderSectionTooLarge))
	}
	return kept, defects, nil
}

// entity is one MIME entity as the IMAP reader addresses it: the header block
// net/mail parsed, and the body after the first empty line.
type entity struct {
	header mail.Header
	body   []byte
	// defects records header fields a limit kept out of header (salvage).
	defects []string
}

// readEntity splits raw at its first empty line and parses the header block
// under the header limits (OPS-003).
//
// The split is SplitHeaderBody's, which the IMAP reader applies to every
// section it serves, so the body walked here is byte for byte the body a
// client fetches. The limits are checked before net/mail allocates anything.
// When salvaging, a header over a limit is parsed without its over-limit
// fields instead of being refused (omitOverLimitFields).
//
// top selects the rule for the message's own header. That header must parse,
// even when empty of fields. The body of a MIME part or of an enclosed message
// may simply have no header fields, as in an empty part between two
// delimiters. e.body is set even when an error is returned, so a caller can
// keep the entity as opaque bytes.
func (w *mimeWalker) readEntity(raw []byte, top bool) (entity, error) {
	header, body := SplitHeaderBody(raw)
	e := entity{body: body}
	block := header
	if err := checkHeaderSection(raw, w.limits); err != nil {
		if !w.salvage {
			return e, err
		}
		kept, defects, oerr := omitOverLimitFields(header, header[:headerFieldsEnd(header, body != nil)], w.limits)
		if oerr != nil {
			return e, oerr
		}
		block, e.defects = kept, defects
	}
	m, err := mail.ReadMessage(bytes.NewReader(block))
	switch {
	case err == nil:
		e.header = m.Header
	case !top && errors.Is(err, io.EOF):
		// net/mail reports a block with no fields at all as EOF.
		e.header = mail.Header{}
	default:
		return e, &malformedError{kind: DefectUnparseableHeader, cause: err}
	}
	return e, nil
}

// malformedError is a malformation confined to one entity. It matches
// ErrMalformed. kind is the defect recorded when the walker keeps the entity
// in place; cause, which may quote message text, only ever reaches the error
// string that Parse returns for logs, never a persisted defect.
type malformedError struct {
	kind  string
	cause error
}

func (e *malformedError) Error() string {
	if e.cause != nil {
		return ErrMalformed.Error() + ": " + e.cause.Error()
	}
	return ErrMalformed.Error() + ": " + e.kind
}

func (e *malformedError) Unwrap() []error {
	if e.cause != nil {
		return []error{ErrMalformed, e.cause}
	}
	return []error{ErrMalformed}
}

// undecodableError is a body its declared Content-Transfer-Encoding cannot
// decode. It matches ErrMalformed. decodeTransfer returns it together with
// whatever decoded before the error.
type undecodableError struct {
	encoding string
	err      error
}

func (e *undecodableError) Error() string {
	return ErrMalformed.Error() + ": transfer decode: " + e.err.Error()
}

func (e *undecodableError) Unwrap() []error { return []error{ErrMalformed, e.err} }

// entityDefect decides whether err, raised while reading one entity, can be
// absorbed by keeping that entity in place, and names the defect to record
// if so. Only fixed kinds and offsets appear in the defect, never message
// text.
//
// A malformation is always absorbed. A resource limit is absorbed only when
// salvaging. Anything else (a limit when refusing, or an error this package
// does not classify) returns ok == false, and the caller must propagate it,
// which refuses the message as before OPS-003.
func (w *mimeWalker) entityDefect(err error) (defect string, ok bool) {
	var me *malformedError
	var ue *undecodableError
	switch {
	case errors.As(err, &ue):
		return DefectUndecodableBody + ": " + ue.encoding + ": " + ue.err.Error(), true
	case errors.As(err, &me):
		return me.kind, true
	case errors.Is(err, ErrTooDeep):
		return DefectDepthExceeded, w.salvage
	case errors.Is(err, ErrTooManyParts):
		return DefectPartsExceeded, w.salvage
	case errors.Is(err, ErrHeaderTooLarge):
		return DefectHeaderFieldTooLarge, w.salvage
	case errors.Is(err, ErrHeadersTooBig):
		return DefectHeaderSectionTooLarge, w.salvage
	case errors.Is(err, ErrZipBomb):
		return DefectDecodeLimitExceeded, w.salvage
	default:
		return "", false
	}
}

// EarlierSeparator returns the smaller of two byte offsets, treating a
// negative value as "not found". It is exported so the IMAP read path splits
// header from body by exactly the rule the writer used (RA6X-006).
func EarlierSeparator(a, b int) int { return earlierSeparator(a, b) }

// SplitHeaderBody splits raw at the FIRST blank line of either flavour and
// returns the header section (including its separator) and the body.
//
// It exists so ingest and the IMAP FETCH path cannot disagree. Postfix's pipe
// transport hands the LDA LF-terminated content, so real headers end at
// "\n\n"; a reader that looks for "\r\n\r\n" first and only falls back to
// "\n\n" places the boundary at whatever CRLF blank line happens to appear in
// the body, turning part of the body into headers and truncating BODY[TEXT].
// ingest was corrected to take the earlier separator; the reader was not, so
// the writer and the reader disagreed about where a message's headers ended
// (RA6X-006).
//
// The returned slices are sub-slices of raw: byte-identical, separator
// included, nothing normalized.
//
// With no empty line at all, the whole of raw is the header section and body
// is nil: a message with no body, which RFC 5322 §3.5 allows and net/mail
// reads the same way (OPS-003).
func SplitHeaderBody(raw []byte) (header, body []byte) {
	end := HeaderSeparatorEnd(raw, 0)
	if end < 0 {
		return raw, nil
	}
	return raw[:end], raw[end:]
}

// HeaderSeparatorEnd returns the offset just past the empty line that ends the
// header section at the start of b, or -1 if b holds no complete one. It is
// the single definition of that boundary, shared by SplitHeaderBody and the
// IMAP reader's streamed header read so the two cannot disagree (RA6X-006,
// OPS-003).
//
// The empty line is the first line that is empty once its line break is
// removed, whether that break is "\n" or "\r\n". That is net/mail's rule, so
// the header block ingest parses is the one net/mail reads. It includes a
// CRLF empty line after a field ended by a bare LF ("\n\r\n"), a mix older
// tooling produced. Recognising only "\r\n\r\n" and "\n\n" missed it and
// ended the section at some later blank line in the body instead. A "\r\n\r\n"
// separator ends where it always did: its "\n\r\n" suffix is found at the
// same offset.
//
// from lets a streaming caller resume. b[:from] was scanned by an earlier call
// on a prefix of b that found nothing. The scan backs up two bytes, so a
// separator straddling that point is still found. Pass 0 to scan all of b.
func HeaderSeparatorEnd(b []byte, from int) int {
	// An entity with no header fields at all: its first line is already the
	// empty one. Until two bytes are in hand, a leading "\r" is undecided.
	if from < 2 {
		if bytes.HasPrefix(b, []byte("\n")) {
			return 1
		}
		if bytes.HasPrefix(b, []byte("\r\n")) {
			return 2
		}
	}
	start := max(from-2, 0)
	lf := bytes.Index(b[start:], []byte("\n\n"))
	crlf := bytes.Index(b[start:], []byte("\n\r\n"))
	// Both patterns begin with the '\n' that ends the preceding line, so the
	// earlier match is the earlier empty line; they cannot match at the same
	// offset (R-052).
	i := earlierSeparator(lf, crlf)
	switch {
	case i < 0:
		return -1
	case i == lf:
		return start + i + 2
	default:
		return start + i + 3
	}
}

// earlierSeparator returns the smaller of two byte offsets, treating a negative
// value as "not found". It returns -1 only when both are negative. Used to pick
// whichever of the CRLF or LF empty line comes first, so a body-embedded CRLF
// blank line can't be mistaken for the real LF-terminated header boundary
// (R-052).
func earlierSeparator(a, b int) int {
	switch {
	case a < 0:
		return b
	case b < 0:
		return a
	case a <= b:
		return a
	default:
		return b
	}
}

// populateConveniences fills Message scalar fields from the headers map.
func populateConveniences(m *Message) {
	if v := firstHeader(m.Headers, "Message-Id"); v != "" {
		m.MessageID = trimAngleBrackets(v)
	}
	if v := firstHeader(m.Headers, "In-Reply-To"); v != "" {
		m.InReplyTo = trimAngleBrackets(v)
	}
	if v := firstHeader(m.Headers, "Subject"); v != "" {
		m.Subject = v
	}
	if v := firstHeader(m.Headers, "From"); v != "" {
		m.From = canonicalAddress(v)
	}
	if vs := m.Headers["To"]; len(vs) > 0 {
		m.To = splitAddresses(vs)
	}
	if vs := m.Headers["Cc"]; len(vs) > 0 {
		m.Cc = splitAddresses(vs)
	}
	if v := firstHeader(m.Headers, "Date"); v != "" {
		if t, err := mail.ParseDate(v); err == nil {
			m.SentDate = t
			// mail.ParseDate keeps the header's offset in the returned
			// Location, so formatting there yields the sender's own calendar
			// date — the one IMAP sent-date search is defined against
			// (RA6X-048).
			m.SentDateLocal = t.Format("2006-01-02")
		}
	}
}

func firstHeader(h map[string][]string, key string) string {
	if vs, ok := h[key]; ok && len(vs) > 0 {
		return vs[0]
	}
	return ""
}

func trimAngleBrackets(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "<")
	s = strings.TrimSuffix(s, ">")
	return s
}

// canonicalAddress returns the bare address (no display name) when parseable;
// otherwise returns the input unchanged so caller can still log/index it.
func canonicalAddress(v string) string {
	addr, err := mail.ParseAddress(v)
	if err != nil || addr == nil {
		return v
	}
	return addr.Address
}

func splitAddresses(values []string) []string {
	joined := strings.Join(values, ", ")
	addrs, err := mail.ParseAddressList(joined)
	if err != nil || len(addrs) == 0 {
		return values
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.Address)
	}
	return out
}
