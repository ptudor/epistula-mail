package imapsess

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// serializeBodyStructure drives the real go-imap server writer over a
// loopback connection and returns the untagged FETCH line it emits for bs.
//
// Reaching the wire matters here: the whole of RO5X-004 is about the bytes a
// client actually sees, and the library's writer — not our converter — is
// what decides whether a nil Envelope is safe, a question this harness
// answers empirically.
func serializeBodyStructure(t *testing.T, bs imap.BodyStructure) string {
	t.Helper()

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &bsFetchSession{bs: bs}, &imapserver.GreetingData{PreAuth: true}, nil
		},
		InsecureAuth: true,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go srv.Serve(ln)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}

	br := bufio.NewReader(conn)
	if _, err := br.ReadString('\n'); err != nil { // greeting
		t.Fatalf("greeting: %v", err)
	}
	fmt.Fprint(conn, "a1 SELECT INBOX\r\n")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if strings.HasPrefix(line, "a1 ") {
			if !strings.HasPrefix(line, "a1 OK") {
				t.Fatalf("SELECT failed: %s", line)
			}
			break
		}
	}

	fmt.Fprint(conn, "a2 FETCH 1 (BODYSTRUCTURE)\r\n")
	var fetchLine string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			// The stub session implements only what SELECT and FETCH need,
			// so the connection may end once the response is written. That
			// is fine provided we captured the untagged line.
			if fetchLine != "" {
				break
			}
			t.Fatalf("fetch: %v", err)
		}
		if strings.HasPrefix(line, "* 1 FETCH") {
			fetchLine = line
		}
		if strings.HasPrefix(line, "a2 ") {
			if !strings.HasPrefix(line, "a2 OK") {
				t.Fatalf("FETCH failed: %s", line)
			}
			break
		}
	}
	if fetchLine == "" {
		t.Fatal("no untagged FETCH response")
	}
	return strings.TrimRight(fetchLine, "\r\n")
}

// bsFetchSession is a minimal session that reports one message and answers
// FETCH with a fixed BODYSTRUCTURE.
type bsFetchSession struct {
	imapserver.Session // nil: only the methods below are exercised
	bs                 imap.BodyStructure
}

func (s *bsFetchSession) Close() error { return nil }

func (s *bsFetchSession) Select(mailbox string, options *imap.SelectOptions) (*imap.SelectData, error) {
	return &imap.SelectData{NumMessages: 1, UIDNext: 2, UIDValidity: 1}, nil
}

func (s *bsFetchSession) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error { return nil }

func (s *bsFetchSession) Fetch(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	mw := w.CreateMessage(1)
	mw.WriteUID(1)
	mw.WriteBodyStructure(s.bs)
	return mw.Close()
}

// ingestShapeForwardedMessage is the JSONB the epistula-database ingest walker
// produces for a forwarded message: a message/rfc822 node carrying exactly
// one child, per R-027's encapsulated-message recursion.
const ingestShapeForwardedMessage = `{
  "type": "message", "subtype": "rfc822", "encoding": "7bit",
  "size": 4096, "lines": 120,
  "parts": [
    {"type": "text", "subtype": "plain", "encoding": "7bit",
     "size": 300, "lines": 12, "params": {"charset": "utf-8"}}
  ]
}`

// TestForwardedMessageSerializesAsMessageRFC822 is the RO5X-004 regression.
//
// The persisted node has children, so the old child-count discriminator made
// it a multipart whose subtype was RFC822. On the wire that is
// `(( … ) "RFC822" …)`; the RFC requires `("MESSAGE" "RFC822" …)`.
func TestForwardedMessageSerializesAsMessageRFC822(t *testing.T) {
	var p persistedBS
	if err := json.Unmarshal([]byte(ingestShapeForwardedMessage), &p); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	bs := convertBS(p)

	sp, ok := bs.(*imap.BodyStructureSinglePart)
	if !ok {
		t.Fatalf("convertBS returned %T, want *imap.BodyStructureSinglePart", bs)
	}
	if sp.Type != "message" || sp.Subtype != "rfc822" {
		t.Errorf("media type = %s/%s, want message/rfc822", sp.Type, sp.Subtype)
	}
	if sp.MessageRFC822 == nil {
		t.Fatal("MessageRFC822 is nil; the nested structure was dropped")
	}
	if sp.MessageRFC822.NumLines != 120 {
		t.Errorf("NumLines = %d, want 120", sp.MessageRFC822.NumLines)
	}
	nested, ok := sp.MessageRFC822.BodyStructure.(*imap.BodyStructureSinglePart)
	if !ok {
		t.Fatalf("nested = %T, want *imap.BodyStructureSinglePart", sp.MessageRFC822.BodyStructure)
	}
	if nested.Type != "text" || nested.Subtype != "plain" {
		t.Errorf("nested media type = %s/%s, want text/plain", nested.Type, nested.Subtype)
	}

	// And on the wire. This also checks that a nil Envelope does not panic
	// the library's writer.
	line := serializeBodyStructure(t, bs)
	t.Logf("wire: %s", line)
	if !strings.Contains(strings.ToUpper(line), `"MESSAGE" "RFC822"`) {
		t.Errorf(`wire response lacks "MESSAGE" "RFC822": %s`, line)
	}
}

// TestUnparseableEmbeddedMessageHasNoNestedStructure covers an edge case: a
// message/rfc822 node with zero children (ingest degraded it to an opaque
// attachment) must serialize as a plain single-part, not as a synthesized
// empty child.
func TestUnparseableEmbeddedMessageHasNoNestedStructure(t *testing.T) {
	var p persistedBS
	if err := json.Unmarshal([]byte(
		`{"type":"message","subtype":"rfc822","encoding":"base64","size":900}`), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	sp, ok := convertBS(p).(*imap.BodyStructureSinglePart)
	if !ok {
		t.Fatalf("want *imap.BodyStructureSinglePart")
	}
	if sp.MessageRFC822 != nil {
		t.Error("synthesized a nested structure for a childless message/rfc822 node")
	}
	if sp.Size != 900 {
		t.Errorf("Size = %d, want 900", sp.Size)
	}
	line := serializeBodyStructure(t, sp)
	t.Logf("wire: %s", line)
	if !strings.Contains(strings.ToUpper(line), `"MESSAGE" "RFC822"`) {
		t.Errorf(`wire response lacks "MESSAGE" "RFC822": %s`, line)
	}
}

// TestRealMultipartUnchanged pins the "what must NOT change" clause: genuine
// multipart nodes keep today's serialization exactly.
func TestRealMultipartUnchanged(t *testing.T) {
	var p persistedBS
	if err := json.Unmarshal([]byte(`{
	  "type":"multipart","subtype":"mixed",
	  "params":{"boundary":"xyz"},
	  "parts":[
	    {"type":"text","subtype":"plain","encoding":"7bit","size":100,"lines":5},
	    {"type":"application","subtype":"pdf","encoding":"base64","size":2048}
	  ]}`), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	mp, ok := convertBS(p).(*imap.BodyStructureMultiPart)
	if !ok {
		t.Fatalf("convertBS returned %T, want *imap.BodyStructureMultiPart", convertBS(p))
	}
	if mp.Subtype != "mixed" {
		t.Errorf("Subtype = %q, want mixed", mp.Subtype)
	}
	if len(mp.Children) != 2 {
		t.Fatalf("Children = %d, want 2", len(mp.Children))
	}
	line := serializeBodyStructure(t, mp)
	t.Logf("wire: %s", line)
	if !strings.Contains(strings.ToUpper(line), `"MIXED"`) {
		t.Errorf("wire response lost the multipart subtype: %s", line)
	}
}

// TestNestedForwardInsideMultipart is the realistic shape: an email with a
// text part plus a forwarded message attached.
func TestNestedForwardInsideMultipart(t *testing.T) {
	var p persistedBS
	if err := json.Unmarshal([]byte(`{
	  "type":"multipart","subtype":"mixed",
	  "parts":[
	    {"type":"text","subtype":"plain","encoding":"7bit","size":50,"lines":3},
	    {"type":"message","subtype":"rfc822","encoding":"7bit","size":800,"lines":40,
	     "parts":[{"type":"text","subtype":"html","encoding":"quoted-printable","size":600,"lines":30}]}
	  ]}`), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	mp, ok := convertBS(p).(*imap.BodyStructureMultiPart)
	if !ok {
		t.Fatalf("want multipart at the root")
	}
	child, ok := mp.Children[1].(*imap.BodyStructureSinglePart)
	if !ok {
		t.Fatalf("child 2 = %T, want *imap.BodyStructureSinglePart", mp.Children[1])
	}
	if child.Type != "message" || child.Subtype != "rfc822" || child.MessageRFC822 == nil {
		t.Errorf("attached forward mis-typed: %s/%s nested=%v",
			child.Type, child.Subtype, child.MessageRFC822 != nil)
	}
	line := serializeBodyStructure(t, mp)
	t.Logf("wire: %s", line)
	if !strings.Contains(strings.ToUpper(line), `"MESSAGE" "RFC822"`) {
		t.Errorf(`wire response lacks "MESSAGE" "RFC822": %s`, line)
	}
}
