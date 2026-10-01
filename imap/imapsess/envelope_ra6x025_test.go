package imapsess

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

// TestEnvelopeKeepsDisplayNames is the RA6X-025 regression.
//
// messages.from_addr / to_addrs / cc_addrs are deliberately lossy — ingest
// stores bare addresses there so routing has a normalized value to match on —
// and the envelope builder read them, so every display name and group
// structure the sender sent was thrown away. A mail client showed
// "alice@example.invalid" where the message said "Alice Example". Sender,
// Reply-To and Bcc were already read from the retained headers, which is
// exactly the inconsistency.
func TestEnvelopeKeepsDisplayNames(t *testing.T) {
	headers := map[string][]string{
		"From":     {`"Alice Example" <alice@example.invalid>`},
		"To":       {`Bob <bob@example.invalid>, "Carol, C." <carol@example.invalid>`},
		"Cc":       {`Dave <dave@example.invalid>`},
		"Reply-To": {`Alice Reply <reply@example.invalid>`},
	}
	blob, err := json.Marshal(headers)
	if err != nil {
		t.Fatalf("marshal headers: %v", err)
	}

	sub := "hello"
	row := fetchRow{
		subject:      &sub,
		internalDate: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		// The convenience columns hold the bare, normalized forms — what
		// ingest really writes.
		fromAddr: strp("alice@example.invalid"),
		toAddrs:  []string{"bob@example.invalid", "carol@example.invalid"},
		ccAddrs:  []string{"dave@example.invalid"},
		headers:  blob,
	}

	env := buildEnvelope(row)

	if len(env.From) != 1 || env.From[0].Name != "Alice Example" {
		t.Errorf("From = %+v, want the display name Alice Example", env.From)
	}
	if len(env.To) != 2 {
		t.Fatalf("To = %+v, want 2 addresses", env.To)
	}
	if env.To[0].Name != "Bob" || env.To[1].Name != "Carol, C." {
		t.Errorf("To names = %q / %q, want Bob / Carol, C.", env.To[0].Name, env.To[1].Name)
	}
	if len(env.Cc) != 1 || env.Cc[0].Name != "Dave" {
		t.Errorf("Cc = %+v, want the display name Dave", env.Cc)
	}
	if len(env.ReplyTo) != 1 || env.ReplyTo[0].Name != "Alice Reply" {
		t.Errorf("Reply-To = %+v", env.ReplyTo)
	}
	// Absent Sender defaults to From, per RFC 9051.
	if len(env.Sender) != 1 || env.Sender[0].Name != "Alice Example" {
		t.Errorf("Sender = %+v, want it to default to From", env.Sender)
	}
}

// TestEnvelopeFallsBackToConvenienceColumns pins that a row written before the
// headers map carried a value — or whose header is unparseable — still gets an
// envelope from the columns.
func TestEnvelopeFallsBackToConvenienceColumns(t *testing.T) {
	row := fetchRow{
		internalDate: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		fromAddr:     strp("legacy@example.invalid"),
		toAddrs:      []string{"dest@example.invalid"},
	}
	env := buildEnvelope(row)
	if len(env.From) != 1 || env.From[0].Mailbox != "legacy" {
		t.Errorf("From = %+v, want the convenience column", env.From)
	}
	if len(env.To) != 1 || env.To[0].Mailbox != "dest" {
		t.Errorf("To = %+v, want the convenience column", env.To)
	}
}

// TestEmbeddedMessageEnvelopeIsPopulated is the second half of RA6X-025: the
// BODYSTRUCTURE of a forwarded message must carry the enclosed message's
// envelope. It used to be nil unconditionally.
func TestEmbeddedMessageEnvelopeIsPopulated(t *testing.T) {
	persisted := []byte(`{
		"type":"message","subtype":"rfc822","size":100,"lines":5,
		"envelope":{
			"subject":"forwarded subject",
			"from":"\"Inner Sender\" <inner@example.invalid>",
			"to":"Recipient <rcpt@example.invalid>",
			"message_id":"<fwd@example.invalid>",
			"date":"Mon, 01 Jun 2026 09:00:00 +0000"
		},
		"parts":[{"type":"text","subtype":"plain","size":50}]
	}`)

	bs, err := decodeBodyStructure(persisted)
	if err != nil {
		t.Fatalf("decodeBodyStructure: %v", err)
	}
	sp, ok := bs.(*imap.BodyStructureSinglePart)
	if !ok {
		t.Fatalf("got %T, want a single part", bs)
	}
	if sp.MessageRFC822 == nil {
		t.Fatal("message/rfc822 part has no nested structure")
	}
	env := sp.MessageRFC822.Envelope
	if env == nil {
		t.Fatal("the enclosed message's envelope is nil")
	}
	if env.Subject != "forwarded subject" {
		t.Errorf("subject = %q", env.Subject)
	}
	if len(env.From) != 1 || env.From[0].Name != "Inner Sender" {
		t.Errorf("From = %+v, want the display name Inner Sender", env.From)
	}
	if len(env.To) != 1 || env.To[0].Name != "Recipient" {
		t.Errorf("To = %+v", env.To)
	}
	if env.MessageID != "fwd@example.invalid" {
		t.Errorf("Message-ID = %q, want the brackets stripped", env.MessageID)
	}
	if env.Date.IsZero() {
		t.Error("Date was not parsed")
	}
}

// TestEmbeddedMessageWithoutEnvelopeStaysNil pins backward compatibility: a row
// written before the field existed behaves exactly as it did.
func TestEmbeddedMessageWithoutEnvelopeStaysNil(t *testing.T) {
	persisted := []byte(`{
		"type":"message","subtype":"rfc822","size":100,"lines":5,
		"parts":[{"type":"text","subtype":"plain","size":50}]
	}`)
	bs, err := decodeBodyStructure(persisted)
	if err != nil {
		t.Fatalf("decodeBodyStructure: %v", err)
	}
	sp := bs.(*imap.BodyStructureSinglePart)
	if sp.MessageRFC822 == nil {
		t.Fatal("message/rfc822 part has no nested structure")
	}
	if sp.MessageRFC822.Envelope != nil {
		t.Fatalf("a row with no persisted envelope produced %+v", sp.MessageRFC822.Envelope)
	}
}

func strp(s string) *string { return &s }
