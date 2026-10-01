package imapsess

import (
	"encoding/json"
	"errors"
	"net/mail"

	"github.com/emersion/go-imap/v2"
)

// persistedBS mirrors epistula-database's ingest.BodyStructure JSONB shape.
// Kept local to avoid pulling the ingest package into the IMAP server
// just for one type definition that has no behaviour. If the JSONB shape
// ever drifts, the field names below break first and a single migration +
// edit fixes it — no cross-module dance.
type persistedBS struct {
	Type        string            `json:"type"`
	Subtype     string            `json:"subtype"`
	Params      map[string]string `json:"params,omitempty"`
	Encoding    string            `json:"encoding,omitempty"`
	ContentID   string            `json:"content_id,omitempty"`
	Size        int64             `json:"size"`
	Lines       int               `json:"lines,omitempty"`
	Disposition *persistedDisp    `json:"disposition,omitempty"`
	Parts       []persistedBS     `json:"parts,omitempty"`
	// Envelope is present only on a message/rfc822 part whose encapsulated
	// message parsed at ingest (RA6X-025). Additive and omitempty, so a row
	// written before the field existed simply has none.
	Envelope *persistedEnvelope `json:"envelope,omitempty"`
}

// persistedEnvelope mirrors ingest.BodyEnvelope: the RAW header values of an
// encapsulated message, parsed here by the same address code that handles a
// top-level message so the two cannot drift.
type persistedEnvelope struct {
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

type persistedDisp struct {
	Type   string            `json:"type"`
	Params map[string]string `json:"params,omitempty"`
}

// errEmptyBodyStructure is returned when the persisted JSONB is empty
// (e.g. legacy rows from before BodyStructure was being persisted, or
// rows where ingest stored "{}" because the message had no parsable
// MIME structure). The caller falls back to a single-part placeholder.
var errEmptyBodyStructure = errors.New("imapsess: empty BodyStructure JSONB")

// decodeBodyStructure deserializes the messages.bodystructure JSONB into
// an emersion imap.BodyStructure (either *BodyStructureSinglePart or
// *BodyStructureMultiPart, walked recursively).
func decodeBodyStructure(raw []byte) (imap.BodyStructure, error) {
	if len(raw) == 0 {
		return nil, errEmptyBodyStructure
	}
	var p persistedBS
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	// An "empty" record (no Type means nothing was persisted yet) is a
	// recoverable miss — callers fall back to application/octet-stream.
	if p.Type == "" {
		return nil, errEmptyBodyStructure
	}
	return convertBS(p), nil
}

// convertBS maps one persisted node onto the wire type.
//
// The discriminator is the persisted TYPE, not the child count. The ingest
// walker attaches a child to message/rfc822 nodes too (R-027's encapsulated
// message recursion), so a "does it have children?" test serialized every
// forwarded message as multipart/RFC822 — discarding Type ("message") and
// emitting `(( … ) "RFC822" … )` where RFC 3501 requires
// `("MESSAGE" "RFC822" (params) id desc enc size (envelope) (body) lines)`.
// Clients enumerating attachments from BODYSTRUCTURE never saw the forward,
// and clients addressing BODY[N.1] from it computed the wrong part path
// (RO5X-004).
func convertBS(p persistedBS) imap.BodyStructure {
	switch {
	case p.Type == "message" && p.Subtype == "rfc822":
		sp := singlePartFrom(p)
		// Exactly one child is the recurse-success shape. Zero children is
		// the "unparseable embedded message, degraded to an opaque
		// attachment" path (ingest/mime.go): serialize it as a plain
		// single-part message/rfc822 rather than synthesizing an empty
		// child.
		if len(p.Parts) == 1 {
			sp.MessageRFC822 = &imap.BodyStructureMessageRFC822{
				BodyStructure: convertBS(p.Parts[0]),
				NumLines:      int64(p.Lines),
				// The enclosed message's envelope, when ingest recorded one
				// (RA6X-025). A row written before the field existed still
				// yields nil, which the library writes as NIL — the previous
				// behaviour for every message.
				Envelope: convertEnvelope(p.Envelope),
			}
		}
		return sp

	case len(p.Parts) > 0:
		// Real multipart nodes keep today's behaviour exactly. This also
		// catches any persisted node that carries children under an
		// unexpected type, which previously landed here too — narrowing it
		// to `p.Type == "multipart"` would silently drop those children.
		children := make([]imap.BodyStructure, 0, len(p.Parts))
		for _, child := range p.Parts {
			children = append(children, convertBS(child))
		}
		return &imap.BodyStructureMultiPart{
			Children: children,
			Subtype:  p.Subtype,
			Extended: &imap.BodyStructureMultiPartExt{
				Params:      p.Params,
				Disposition: convertDisposition(p.Disposition),
			},
		}

	default:
		sp := singlePartFrom(p)
		if p.Type == "text" && p.Lines >= 0 {
			sp.Text = &imap.BodyStructureText{NumLines: int64(p.Lines)}
		}
		return sp
	}
}

// singlePartFrom builds the fields every single-part node shares.
func singlePartFrom(p persistedBS) *imap.BodyStructureSinglePart {
	return &imap.BodyStructureSinglePart{
		Type:     p.Type,
		Subtype:  p.Subtype,
		Params:   p.Params,
		ID:       p.ContentID,
		Encoding: p.Encoding,
		Size:     uint32(p.Size),
		Extended: &imap.BodyStructureSinglePartExt{
			Disposition: convertDisposition(p.Disposition),
		},
	}
}

func convertDisposition(d *persistedDisp) *imap.BodyStructureDisposition {
	if d == nil {
		return nil
	}
	return &imap.BodyStructureDisposition{
		Value:  d.Type,
		Params: d.Params,
	}
}

// convertEnvelope turns the persisted raw header values of an encapsulated
// message into an imap.Envelope, using the same address parsing the top-level
// envelope uses (RA6X-025).
func convertEnvelope(e *persistedEnvelope) *imap.Envelope {
	if e == nil {
		return nil
	}
	env := &imap.Envelope{
		Subject:   e.Subject,
		MessageID: trimAngle(e.MessageID),
		InReplyTo: splitMsgIDs(e.InReplyTo),
		From:      parseAddressList(e.From),
		Sender:    parseAddressList(e.Sender),
		ReplyTo:   parseAddressList(e.ReplyTo),
		To:        parseAddressList(e.To),
		Cc:        parseAddressList(e.Cc),
		Bcc:       parseAddressList(e.Bcc),
	}
	if t, err := mail.ParseDate(e.Date); err == nil {
		env.Date = t
	}
	// RFC 9051: absent Sender / Reply-To default to From, exactly as the
	// top-level envelope does.
	if len(env.Sender) == 0 {
		env.Sender = env.From
	}
	if len(env.ReplyTo) == 0 {
		env.ReplyTo = env.From
	}
	return env
}
