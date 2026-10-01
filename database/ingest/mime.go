package ingest

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/mail"
	"strings"
)

// mimeWalker accumulates ingest output as it traverses the MIME tree.
type mimeWalker struct {
	limits Limits
	// salvage selects what reaching a resource limit does to the entity
	// that reaches it: refuse the whole message (false, New) or keep the
	// entity in place with a defect (true, NewSalvaging).
	salvage bool
	// remaining is the decode budget: total decoded output across every
	// leaf part may not exceed MaxMessageBytes. For real transfer encodings
	// (base64/qp/8bit) decoded output is never larger than its encoded
	// input, so the budget binds only against decoder pathologies — it
	// turns the old "every part may independently claim 10× MaxMessageBytes"
	// ceiling into a single shared O(MaxMessageBytes) bound.
	remaining   int64
	parts       int
	textBuf     bytes.Buffer
	htmlBuf     bytes.Buffer
	attachments []Attachment
}

// walk processes one MIME entity (the top-level message body or a
// multipart sub-part). partNumber is the IMAP part path, e.g. "1.2.3".
// depth is the current nesting depth: 0 at the top-level message body.
func (w *mimeWalker) walk(partNumber, ctHeader, cteHeader, cdHeader, cidHeader string, body []byte, depth int) (BodyStructure, error) {
	return w.walkIn(partNumber, ctHeader, cteHeader, cdHeader, cidHeader, body, depth, "")
}

// count charges one entity against MaxMimeParts.
func (w *mimeWalker) count() error {
	w.parts++
	if w.parts > w.limits.MaxMimeParts {
		return ErrTooManyParts
	}
	return nil
}

// walkIn is walk with the CONTAINING multipart's subtype, which decides what
// an absent Content-Type defaults to (RA6X-010). Empty means "not inside a
// multipart", i.e. the top-level message or an encapsulated one.
//
// An error from walkIn is confined to this entity unless it is a resource
// limit the walker is refusing. The returned structure is always classified
// (type, parameters, encoding, size), so a caller that can keep the entity in
// place records it as what it declares itself to be (OPS-003).
func (w *mimeWalker) walkIn(partNumber, ctHeader, cteHeader, cdHeader, cidHeader string, body []byte, depth int, containerSubtype string) (BodyStructure, error) {
	if err := w.count(); err != nil {
		return BodyStructure{}, err
	}

	mediatype, params, err := mime.ParseMediaType(ctHeader)
	switch {
	case strings.TrimSpace(ctHeader) == "":
		// ABSENT Content-Type is not the same as a malformed one (RA6X-010).
		// RFC 2045 §5.2 gives an absent field the default text/plain;
		// us-ascii, and RFC 2046 §5.1.5 changes that default to
		// message/rfc822 inside multipart/digest. Treating absence as
		// application/octet-stream turned an ordinary untyped text part into a
		// binary attachment: excluded from the text projection, invisible to
		// full-text search, and reported to clients as a file to download.
		//
		// The IMAP read path already defaulted the same part to text/plain, so
		// ingest and FETCH disagreed about what it was.
		mediatype, params = defaultContentType(containerSubtype)
	case err != nil || mediatype == "":
		// A malformed EXPLICIT type keeps the conservative fallback: the
		// sender said something, we could not read it, and guessing text would
		// be worse than treating the bytes as opaque.
		mediatype = "application/octet-stream"
		params = nil
	}
	mainType, subType, _ := strings.Cut(mediatype, "/")
	encoding := strings.ToLower(strings.TrimSpace(cteHeader))

	bs := BodyStructure{
		Type:        SanitizeUTF8(mainType),
		Subtype:     SanitizeUTF8(subType),
		Params:      sanitizeParams(params),
		Encoding:    SanitizeUTF8(encoding),
		ContentID:   SanitizeUTF8(trimAngleBrackets(cidHeader)),
		Disposition: parseDisposition(cdHeader),
		Size:        int64(len(body)),
	}

	// Checked after classification, so a salvaged entity keeps its declared
	// type (OPS-003).
	if depth > w.limits.MaxMimeDepth {
		return bs, ErrTooDeep
	}

	if IsMultipart(mediatype) {
		return w.walkMultipart(partNumber, subType, params, bs, body, depth)
	}

	decoded, derr := decodeTransfer(body, encoding, w.limits.MaxTransferExpansion, w.remaining)
	// complete is false when the body did not decode in full. The part stays
	// in the structure at its encoded size, so BODY[n] still lines up with
	// what BODYSTRUCTURE advertises, and what DID decode still feeds the
	// text projection. Nothing that claims to BE the content (an attachment
	// row, or an enclosed message's structure) is derived from a partial
	// decode (OPS-003).
	complete := derr == nil
	if derr != nil {
		defect, ok := w.entityDefect(derr)
		if !ok {
			return bs, derr
		}
		bs.Defects = append(bs.Defects, defect)
		var ue *undecodableError
		if !errors.As(derr, &ue) {
			// A decode limit: nothing was decoded to keep.
			bs.Lines = textLines(bs, body)
			return bs, nil
		}
	}
	w.remaining -= int64(len(decoded))
	if w.remaining < 0 {
		// Clamp rather than trusting the decoded<=encoded invariant to hold
		// forever. A negative budget used to disable the shared bound
		// entirely; now it can only ever mean "exhausted" (RO5X-040).
		w.remaining = 0
	}

	// bs.Size deliberately keeps the ENCODED length set at construction
	// above. RFC 3501 §7.4.2 / RFC 9051 §7.5.2 define the BODYSTRUCTURE
	// body-size field as the size "in its transfer encoding and not the
	// resulting size after any decoding", and the IMAP read path returns
	// the on-wire bytes for BODY[N] with no decoding applied — so
	// overwriting this with len(decoded) told every MUA a base64 part was
	// ~27% smaller than the bytes it was about to receive (RO5X-003).
	//
	// Attachment.Size below is the DECODED length and must stay that way:
	// it is the length of the bytes actually written to att/…/<sha>.bin and
	// stored in attachments.size_bytes. Two different quantities that
	// happen to live near each other — do not unify them.

	if mainType == "message" && subType == "rfc822" {
		if !complete {
			// The IMAP reader cannot enter an enclosed message whose body
			// does not decode (DecodeEncapsulated fails), so its structure
			// is not advertised either.
			return bs, nil
		}
		// The container bytes were charged once above. On the recurse-success
		// path walkEmbedded charges the embedded parts individually, so refund
		// the container charge to avoid double-counting a forwarded message
		// against MaxMessageBytes — a legitimate ~0.55×budget forward inside a
		// ~0.6×budget message would otherwise bounce as a "zip bomb" (R-027).
		// Snapshot the single-charge state to restore it on the
		// degrade-to-attachment path, where the bytes ARE stored once.
		remainingSingleCharge := w.remaining
		w.remaining += int64(len(decoded))

		// Snapshot EVERY piece of shared projection state, not just the byte
		// budget (RA6X-038). The recursion writes into textBuf, htmlBuf,
		// attachments and the part counter as it goes; if a later nested part
		// fails and the whole embedded message degrades to one opaque
		// attachment, whatever an earlier valid nested part contributed stays
		// behind. The result is derived text and an attachment list that
		// disagree with the returned BODYSTRUCTURE, including attachment rows
		// whose part numbers reference parts the structure does not contain.
		mark := w.snapshot()

		nested, nerr := w.walkEmbedded(partNumber, decoded, depth+1)
		if nerr == nil {
			// Forwarded/encapsulated message: its text reached the FTS
			// buffers and its own attachments were captured during the
			// recursion. The BODYSTRUCTURE nests the embedded structure. The
			// embedded parts' bytes are already charged; the refund above
			// prevents the container's bytes being counted a second time.
			// Lines, like Size, counts the encoded container per RFC.
			bs.Lines = countLines(string(body))
			bs.Parts = append(bs.Parts, nested.structure)
			// The enclosed message's envelope, so the reader can answer
			// BODYSTRUCTURE for a forwarded message without re-reading the
			// raw blob (RA6X-025).
			bs.Envelope = nested.envelope
			// Header fields a limit left out of the enclosed message's
			// header (salvage) belong to this node, which carries that
			// header's envelope.
			bs.Defects = append(bs.Defects, nested.defects...)
			return bs, nil
		}
		defect, ok := w.entityDefect(nerr)
		if !ok {
			// Resource-limit violations propagate when refusing — an
			// attacker must not dodge the caps by wrapping a bomb in
			// message/rfc822. The refund gave the embedded message the full
			// remaining budget, so a genuine bomb still trips the cap here.
			return bs, nerr
		}
		// The enclosed message cannot be walked here. Its own body is not a
		// place to keep it opaque: whether that body is single-part decides
		// how its parts are numbered, so an opaque stand-in would advertise
		// part paths the IMAP reader resolves differently. This container is
		// such a place, since clients do not descend into a message/rfc822
		// with no structure. Degrade to an opaque attachment below. Restore
		// the single-charge state so the stored bytes are charged exactly
		// once, undoing any partial charge the failed recursion made and
		// re-applying the container charge (R-027).
		w.remaining = remainingSingleCharge
		// Roll back everything the failed recursion accumulated, so the
		// opaque container below is the ONLY thing this part contributes.
		w.restore(mark)
		if errors.Is(nerr, ErrMalformed) {
			defect = DefectUnparseableMessage + ": " + defect
		}
		bs.Defects = append(bs.Defects, defect)
		slog.Debug("ingest embedded message unparseable; storing as attachment",
			"part", partNumber, "err", nerr)
	}

	// Each part yields at most one attachment row (OPS-006). Body text
	// (text/plain, text/html) feeds the projection below and gets a row only
	// when explicitly ATTACHED, in which case it is both (RA6X-049): the row
	// makes its filename visible to the API and MCP, and the text keeps
	// full-text search coverage from regressing for mail already in the store.
	// Classification is by disposition — media type alone made a `.txt`
	// report indistinguishable from the body. Every other part, other text/*
	// types included, is an attachment whatever its disposition; recording it
	// in a second branch as well doubled an attached .csv or .ics.
	bodyText := mainType == "text" && (subType == "plain" || subType == "html")
	if complete && (!bodyText || isAttachedDisposition(bs.Disposition)) {
		w.appendAttachment(partNumber, mediatype, params, bs, decoded)
	}

	switch {
	case mainType == "text" && subType == "plain":
		text, derr := decodeText(decoded, params["charset"])
		if derr != nil {
			// The raw bytes pass through and are sanitized at the Parse
			// level; surface the charset problem for corpus debugging.
			slog.Debug("ingest charset decode failed", "charset", params["charset"], "part", partNumber, "err", derr)
		}
		if w.textBuf.Len() > 0 {
			w.textBuf.WriteString("\n")
		}
		w.textBuf.WriteString(text)
		// Lines counts the transfer-encoded body per RFC, not the
		// charset-decoded string (RO5X-003).
		bs.Lines = textLines(bs, body)

	case mainType == "text" && subType == "html":
		text, derr := decodeText(decoded, params["charset"])
		if derr != nil {
			slog.Debug("ingest charset decode failed", "charset", params["charset"], "part", partNumber, "err", derr)
		}
		if w.htmlBuf.Len() > 0 {
			w.htmlBuf.WriteString("\n")
		}
		w.htmlBuf.WriteString(text)
		bs.Lines = textLines(bs, body)
	}
	return bs, nil
}

// walkMultipart walks the children of one multipart entity.
//
// The children are split by VisitRawMultipart, the function the IMAP reader
// uses for BODY[n], so a child here is byte for byte the child a client
// fetches (OPS-003). mime/multipart used to split them. It refused a body
// whose close delimiter was missing ("read part: unexpected EOF"), and after
// its first delimiter it held to that line ending, so a later delimiter with
// the other ending was not a delimiter to it at all. Truncated and converted
// archives are full of both. Mail clients show such a message with its last
// part running to the end of the body, and so does this walk; the missing
// close delimiter is recorded as a defect.
//
// A raw span also keeps the RA6X-005 invariant by construction: a part's body
// is its transfer-encoded bytes, BodyStructure.Size and Encoding describe the
// wire, and decodeTransfer decodes exactly once.
func (w *mimeWalker) walkMultipart(partNumber, subType string, params map[string]string, bs BodyStructure, body []byte, depth int) (BodyStructure, error) {
	boundary := params["boundary"]
	if boundary == "" {
		return bs, &malformedError{kind: DefectMissingBoundary}
	}
	i := 0
	closed, err := visitRawMultipart(body, boundary, func(span []byte) error {
		i++
		child, err := w.walkChild(childPath(partNumber, i), span, depth+1, subType)
		if err != nil {
			return err
		}
		bs.Parts = append(bs.Parts, child)
		return nil
	})
	switch {
	case w.salvage && errors.Is(err, ErrTooManyParts):
		// The part budget ran out: the children read so far stay listed,
		// the rest are not.
		bs.Defects = append(bs.Defects, DefectPartsExceeded)
	case err != nil:
		return bs, err
	case len(bs.Parts) == 0:
		bs.Defects = append(bs.Defects, DefectNoBodyParts)
	case !closed:
		bs.Defects = append(bs.Defects, DefectMissingCloseDelimiter)
	}
	return bs, nil
}

// walkChild reads one child of a multipart. A child is addressed by its
// position alone (BODY[n]), which makes it the place where an entity the
// walker cannot fully read is kept as an opaque leaf: whatever it turns out
// to be, BODY[n] returns its body and nothing below it is advertised, so the
// stored structure still agrees with the IMAP reader's walk of the same bytes
// (OPS-003).
func (w *mimeWalker) walkChild(partNumber string, span []byte, depth int, containerSubtype string) (BodyStructure, error) {
	e, err := w.readEntity(span, false)
	if err != nil {
		defect, ok := w.entityDefect(err)
		if !ok {
			return BodyStructure{}, err
		}
		// A header this walk cannot read says nothing reliable about the
		// part, so the part is opaque bytes.
		if cerr := w.count(); cerr != nil {
			return BodyStructure{}, cerr
		}
		return BodyStructure{
			Type:    "application",
			Subtype: "octet-stream",
			Size:    int64(len(e.body)),
			Defects: []string{defect},
		}, nil
	}
	mark := w.snapshot()
	bs, err := w.walkIn(
		partNumber,
		e.header.Get("Content-Type"),
		e.header.Get("Content-Transfer-Encoding"),
		e.header.Get("Content-Disposition"),
		e.header.Get("Content-ID"),
		e.body,
		depth,
		containerSubtype,
	)
	if err != nil {
		defect, ok := w.entityDefect(err)
		if !ok || errors.Is(err, ErrTooManyParts) {
			// Refusing, or the part budget ran out, which ends the
			// enclosing multipart's list rather than this one entity.
			return BodyStructure{}, err
		}
		// Nothing the failed walk accumulated survives: this entity
		// contributes only its opaque self (RA6X-038).
		w.restore(mark)
		bs.Parts = nil
		bs.Envelope = nil
		bs.Lines = textLines(bs, e.body)
		bs.Defects = append(bs.Defects, defect)
	}
	if len(e.defects) > 0 {
		bs.Defects = append(e.defects, bs.Defects...)
	}
	return bs, nil
}

// textLines is the BODYSTRUCTURE line count of a text/plain or text/html
// body: the lines of its transfer-encoded bytes, per RFC 3501 (RO5X-003).
// Other types carry none.
func textLines(bs BodyStructure, body []byte) int {
	if bs.Type == "text" && (bs.Subtype == "plain" || bs.Subtype == "html") {
		return countLines(string(body))
	}
	return 0
}

// appendAttachment records one part as an attachment row. Size is the DECODED
// length — the length of the bytes written to att/…/<sha>.bin — which is a
// different quantity from BodyStructure.Size, the encoded length RFC 3501
// requires. They live near each other; do not unify them.
func (w *mimeWalker) appendAttachment(partNumber, mediatype string, params map[string]string, bs BodyStructure, decoded []byte) {
	filename := params["name"]
	if bs.Disposition != nil {
		if dn := bs.Disposition.Params["filename"]; dn != "" {
			filename = dn
		}
	}
	dispType := ""
	if bs.Disposition != nil {
		dispType = bs.Disposition.Type
	}
	w.attachments = append(w.attachments, Attachment{
		PartNumber:  partNumber,
		Filename:    SanitizeUTF8(filename),
		ContentType: SanitizeUTF8(mediatype),
		ContentID:   bs.ContentID,
		Disposition: SanitizeUTF8(dispType),
		Size:        int64(len(decoded)),
		Data:        decoded,
	})
}

// isAttachedDisposition reports whether a part is explicitly presented as a
// file rather than as message content: Content-Disposition: attachment, or an
// inline disposition that nonetheless names a file.
func isAttachedDisposition(d *BodyDisposition) bool {
	if d == nil {
		return false
	}
	if strings.EqualFold(d.Type, "attachment") {
		return true
	}
	return d.Params["filename"] != ""
}

// walkerMark is a snapshot of every accumulator walk mutates, so an embedded
// message that fails partway can be rolled back completely (RA6X-038).
type walkerMark struct {
	textLen     int
	htmlLen     int
	attachments int
	parts       int
}

func (w *mimeWalker) snapshot() walkerMark {
	return walkerMark{
		textLen:     w.textBuf.Len(),
		htmlLen:     w.htmlBuf.Len(),
		attachments: len(w.attachments),
		parts:       w.parts,
	}
}

// restore rewinds to a mark. The part counter is restored too: the failed
// recursion's parts do not exist in the returned structure, and leaving them
// counted would let a later sibling trip MaxMimeParts for parts nobody can
// address. The opaque container the caller is about to append is charged by
// its own walk.
func (w *mimeWalker) restore(m walkerMark) {
	w.textBuf.Truncate(m.textLen)
	w.htmlBuf.Truncate(m.htmlLen)
	w.attachments = w.attachments[:m.attachments]
	w.parts = m.parts
}

// defaultContentType returns the media type an ABSENT Content-Type field
// implies for a part inside a multipart of the given subtype (RA6X-010).
//
// RFC 2045 §5.2: absent means text/plain; charset=us-ascii.
// RFC 2046 §5.1.5: inside multipart/digest it means message/rfc822 instead,
// which is the whole point of digest — a list of encapsulated messages nobody
// has to type a header for.
func defaultContentType(containerSubtype string) (string, map[string]string) {
	if strings.EqualFold(containerSubtype, "digest") {
		return "message/rfc822", nil
	}
	return "text/plain", map[string]string{"charset": "us-ascii"}
}

// IsMultipart reports whether an entity of a media type, as
// mime.ParseMediaType returns it, has children rather than a body of its own.
// The main type alone decides, so "multipart" with no subtype counts too.
//
// The IMAP reader asks the same question of the same entities through this
// function (OPS-004). If the two answered differently, BODYSTRUCTURE would
// advertise children that FETCH cannot address, or the reverse.
func IsMultipart(mediatype string) bool {
	mainType, _, _ := strings.Cut(mediatype, "/")
	return mainType == "multipart"
}

// enclosed is an encapsulated message as walkEmbedded read it.
type enclosed struct {
	structure BodyStructure
	envelope  *BodyEnvelope
	// defects records header fields a limit left out of the enclosed
	// message's header (salvage).
	defects []string
}

// walkEmbedded parses one encapsulated message/rfc822 payload and recurses
// the walker over its content. IMAP part numbering: the embedded message's
// parts live under the rfc822 part's number (see bodyPartNumber).
//
// The payload is split and its header parsed by readEntity, as the IMAP
// reader splits an enclosed message it enters.
func (w *mimeWalker) walkEmbedded(partNumber string, raw []byte, depth int) (enclosed, error) {
	e, err := w.readEntity(raw, false)
	if err != nil {
		return enclosed{}, err
	}

	ct := e.header.Get("Content-Type")
	if strings.TrimSpace(ct) == "" {
		ct = "text/plain; charset=us-ascii"
	}
	bs, err := w.walk(
		bodyPartNumber(partNumber, ct),
		ct,
		e.header.Get("Content-Transfer-Encoding"),
		e.header.Get("Content-Disposition"),
		e.header.Get("Content-ID"),
		e.body,
		depth,
	)
	if err != nil {
		return enclosed{}, err
	}
	return enclosed{structure: bs, envelope: embeddedEnvelope(e.header), defects: e.defects}, nil
}

// embeddedEnvelope reads the envelope headers out of an encapsulated message's
// parsed header. Values are stored raw; the IMAP reader parses them with the
// same code it uses for a top-level message, so display names, groups and
// encoded words are handled identically in both places (RA6X-025).
//
// A header with none of the envelope fields yields nil, which serialises to an
// absent field and leaves the reader emitting NIL, exactly as before.
func embeddedEnvelope(h mail.Header) *BodyEnvelope {
	e := &BodyEnvelope{
		Date:      SanitizeUTF8(h.Get("Date")),
		Subject:   SanitizeUTF8(h.Get("Subject")),
		From:      SanitizeUTF8(h.Get("From")),
		Sender:    SanitizeUTF8(h.Get("Sender")),
		ReplyTo:   SanitizeUTF8(h.Get("Reply-To")),
		To:        SanitizeUTF8(h.Get("To")),
		Cc:        SanitizeUTF8(h.Get("Cc")),
		Bcc:       SanitizeUTF8(h.Get("Bcc")),
		InReplyTo: SanitizeUTF8(h.Get("In-Reply-To")),
		MessageID: SanitizeUTF8(h.Get("Message-ID")),
	}
	if *e == (BodyEnvelope{}) {
		return nil
	}
	return e
}

func parseDisposition(cd string) *BodyDisposition {
	cd = strings.TrimSpace(cd)
	if cd == "" {
		return nil
	}
	dispType, params, err := mime.ParseMediaType(cd)
	if err != nil || dispType == "" {
		return nil
	}
	return &BodyDisposition{Type: SanitizeUTF8(dispType), Params: sanitizeParams(params)}
}

// decodeTransfer decodes one part's body according to its
// Content-Transfer-Encoding, bounded by both the per-part expansion ratio and
// the message-wide shared budget.
//
// A body the encoding cannot decode returns an *undecodableError together
// with whatever decoded before the error (OPS-003). Only base64 can fail that
// way: quoted-printable is decoded leniently, as mail clients decode it.
//
// INVARIANT: decoded output is never larger than encoded input for every
// supported CTE. base64 shrinks by 3/4, quoted-printable shrinks or stays
// equal, and the passthrough encodings return the input unchanged. The caller
// subtracts len(decoded) from the shared budget on every leaf, so that
// invariant is what keeps the budget from going negative. A decoder that could
// EXPAND (a hypothetical gzip or x-uuencode CTE, or a charset transcode
// charged here) would break it and must justify itself — the clamp in the
// caller and the budget<=0 fast path in readCapped are the backstop
// (RO5X-040).
func decodeTransfer(body []byte, encoding string, maxExpansion int, budget int64) ([]byte, error) {
	encoded := int64(len(body))
	switch encoding {
	case "", "7bit", "8bit", "binary":
		// Charge the passthrough encodings against the budget too, so a
		// message made entirely of 7bit parts cannot exhaust it silently.
		if budget <= 0 || encoded > budget {
			return nil, ErrZipBomb
		}
		return body, nil
	case "quoted-printable":
		// The decoded length never exceeds the encoded length, so the
		// expansion ratio cannot bind; the shared budget still does.
		if budget <= 0 {
			return nil, ErrZipBomb
		}
		decoded := decodeQuotedPrintable(body)
		if int64(len(decoded)) > budget {
			return nil, ErrZipBomb
		}
		return decoded, nil
	case "base64":
		// IMAP/MIME base64 often has internal whitespace; the stdlib decoder
		// rejects newlines unless we wrap it. base64.StdEncoding tolerates
		// padding but not whitespace; we strip whitespace first.
		clean := stripBase64Whitespace(body)
		decoded, err := readCapped(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(clean)), encoded, maxExpansion, budget)
		if err != nil && !errors.Is(err, ErrZipBomb) {
			return decoded, &undecodableError{encoding: encoding, err: err}
		}
		return decoded, err
	default:
		// Unknown encoding (e.g., x-uuencode): return raw and let the caller
		// surface as an opaque attachment. We do not invent decoders — but the
		// bytes still count against the shared budget.
		if budget <= 0 || encoded > budget {
			return nil, ErrZipBomb
		}
		return body, nil
	}
}

// readCapped reads at most min(encoded × maxExpansion with a 1 KiB floor,
// budget) bytes. Exceeding the cap is a bomb signal, not a truncation. A read
// error comes back with the bytes read before it.
func readCapped(r io.Reader, encoded int64, maxExpansion int, budget int64) ([]byte, error) {
	if maxExpansion <= 0 {
		maxExpansion = 10
	}
	// An exhausted budget is a hard stop, not a fall-through. The old guard
	// was `if budget >= 0 && limit > budget`, so a NEGATIVE budget silently
	// stopped applying and the shared O(MaxMessageBytes) bound reverted to the
	// old per-part `encoded x MaxTransferExpansion` ceiling — exactly the
	// regression the shared budget was introduced to prevent (RO5X-040).
	if budget <= 0 {
		return nil, ErrZipBomb
	}
	limit := encoded * int64(maxExpansion)
	if limit < 1024 {
		limit = 1024
	}
	// Unconditional: the budget always binds.
	if limit > budget {
		limit = budget
	}
	buf, err := io.ReadAll(io.LimitReader(r, limit+1))
	if int64(len(buf)) > limit {
		return nil, ErrZipBomb
	}
	return buf, err
}

func stripBase64Whitespace(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			out = append(out, c)
		}
	}
	return out
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// DecodeEncapsulated shares ingest's CTE policy with IMAP traversal. Standard
// supported encodings cannot expand past their encoded input, so the original
// container length is also a finite allocation bound for historical reads.
// A body that does not decode in full is an error: ingest does not walk into
// such an enclosed message either, so neither side advertises its parts.
func DecodeEncapsulated(body []byte, encoding string) ([]byte, error) {
	return decodeTransfer(body, strings.ToLower(strings.TrimSpace(encoding)), 1, int64(len(body)))
}
