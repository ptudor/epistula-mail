package imapsess

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/ptudor/epistula-mail/database/ingest"
)

// OPS-004: BODY[section] numbering through forwards of forwards.
//
// A message/rfc822 part whose enclosed message's body is directly another
// message/rfc822 was resolved by entering both enclosed messages at once, so
// BODY[N.1] returned the innermost body rather than the middle message. The
// numbering checked here is RFC 9051 §6.4.5's (rfcPartNumbers), derived from
// the text of the RFC and not from either implementation.

// forwardChain is a synthetic forward of a forward. levels[0] is the top-level
// message; every other level is the message the level above it encloses as
// message/rfc822. A wrapped level encloses the next inside a multipart/mixed,
// after a short text part, as mail clients forward. A direct level encloses it
// as its own body (Content-Type: message/rfc822), with no multipart around it.
// The innermost message is text/plain, or multipart/mixed holding a text part
// and a base64 attachment.
type forwardChain struct {
	name       string
	raw        []byte
	attachment bool
	levels     []chainLevel
}

type chainLevel struct {
	// header is the message's header section, empty line included, and body
	// is everything after it.
	header, body string
	// wrapped says how this level encloses the next; unused on the innermost.
	wrapped bool
	// prefix is the part number this message's parts are numbered under:
	// none for the top-level message, the enclosing message/rfc822 part's
	// number for the rest.
	prefix []int
}

var chainAttachment = []byte("OPS-004 attachment \x00\x01\x02\xff")

// forwardPart is the number of the message/rfc822 part that encloses the next
// level: the second child of a wrapped level, and for a direct level its body,
// which is the only part of a message that is not multipart.
func (l chainLevel) forwardPart() []int {
	if l.wrapped {
		return appendPath(l.prefix, 2)
	}
	return appendPath(l.prefix, 1)
}

// forwardMIME is the MIME header of forwardPart: the part's own header inside
// the multipart, or for a direct level the message's header.
func (l chainLevel) forwardMIME() string {
	if l.wrapped {
		return "Content-Type: message/rfc822\r\n\r\n"
	}
	return l.header
}

func appendPath(p []int, n ...int) []int {
	return append(append([]int(nil), p...), n...)
}

func buildForwardChain(wrapped []bool, attachment bool) forwardChain {
	depth := len(wrapped)
	levels := make([]chainLevel, depth+1)
	subject := func(k int) string {
		return fmt.Sprintf("From: level%d@ops004.invalid\r\nSubject: level %d\r\n", k, k)
	}

	inner := &levels[depth]
	inner.header = subject(depth)
	if attachment {
		inner.header += "Content-Type: multipart/mixed; boundary=\"att\"\r\n\r\n"
		inner.body = "--att\r\nContent-Type: text/plain\r\n\r\ninnermost text\r\n" +
			"--att\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n" +
			"Content-Disposition: attachment; filename=\"ops004.bin\"\r\n\r\n" +
			base64.StdEncoding.EncodeToString(chainAttachment) + "\r\n--att--\r\n"
	} else {
		inner.header += "Content-Type: text/plain\r\n\r\n"
		inner.body = "innermost text\r\n"
	}

	for k := depth - 1; k >= 0; k-- {
		next := levels[k+1].header + levels[k+1].body
		l := &levels[k]
		l.wrapped = wrapped[k]
		l.header = subject(k)
		if l.wrapped {
			b := fmt.Sprintf("w%d", k)
			l.header += "Content-Type: multipart/mixed; boundary=\"" + b + "\"\r\n\r\n"
			l.body = "--" + b + "\r\nContent-Type: text/plain\r\n\r\n" + fmt.Sprintf("forwarding level %d", k+1) + "\r\n" +
				"--" + b + "\r\nContent-Type: message/rfc822\r\n\r\n" + next + "\r\n--" + b + "--\r\n"
		} else {
			l.header += "Content-Type: message/rfc822\r\n\r\n"
			l.body = next
		}
	}

	var prefix []int
	for k := range levels {
		levels[k].prefix = prefix
		if k < depth {
			prefix = levels[k].forwardPart()
		}
	}

	shape := make([]string, depth)
	for k, w := range wrapped {
		shape[k] = map[bool]string{true: "wrapped", false: "direct"}[w]
	}
	name := strings.Join(shape, "+") + map[bool]string{true: "/attachment", false: "/text"}[attachment]
	return forwardChain{name: name, raw: []byte(levels[0].header + levels[0].body), attachment: attachment, levels: levels}
}

// forwardChains returns every chain 1, 2 and 3 levels deep, each level wrapped
// or direct, with each innermost shape.
func forwardChains() []forwardChain {
	var out []forwardChain
	for depth := 1; depth <= 3; depth++ {
		for mask := 0; mask < 1<<depth; mask++ {
			wrapped := make([]bool, depth)
			for k := range wrapped {
				wrapped[k] = mask&(1<<k) != 0
			}
			for _, attachment := range []bool{false, true} {
				out = append(out, buildForwardChain(wrapped, attachment))
			}
		}
	}
	return out
}

// partPaths is every part number the chain has, in RFC 9051 numbering.
func (c forwardChain) partPaths() []string {
	var out []string
	depth := len(c.levels) - 1
	for _, l := range c.levels[:depth] {
		if l.wrapped {
			out = append(out, partIntPath(appendPath(l.prefix, 1)))
		}
		out = append(out, partIntPath(l.forwardPart()))
	}
	inner := c.levels[depth].prefix
	out = append(out, partIntPath(appendPath(inner, 1)))
	if c.attachment {
		out = append(out, partIntPath(appendPath(inner, 2)))
	}
	sort.Strings(out)
	return out
}

// sectionWant is one BODY[...] section of a chain and the bytes it must be.
// absent marks a part that does not exist.
type sectionWant struct {
	sec    *imap.FetchItemBodySection
	want   string
	absent bool
}

// sections lists, for every level, BODY[N], N.HEADER, N.TEXT, N.MIME and
// N.HEADER.FIELDS of the part enclosing the next level, the cover text of a
// wrapped level, the innermost parts, the parts that must not exist, and the
// top-level TEXT and HEADER.
func (c forwardChain) sections() []sectionWant {
	sec := func(part []int, spec imap.PartSpecifier) *imap.FetchItemBodySection {
		return &imap.FetchItemBodySection{Part: part, Specifier: spec, Peek: true}
	}
	var out []sectionWant
	add := func(part []int, spec imap.PartSpecifier, want string) {
		out = append(out, sectionWant{sec: sec(part, spec), want: want})
	}
	absent := func(part []int) {
		out = append(out, sectionWant{sec: sec(part, imap.PartSpecifierNone), absent: true})
	}

	depth := len(c.levels) - 1
	for k, l := range c.levels[:depth] {
		next := c.levels[k+1]
		fwd := l.forwardPart()
		add(fwd, imap.PartSpecifierNone, next.header+next.body)
		add(fwd, imap.PartSpecifierHeader, next.header)
		add(fwd, imap.PartSpecifierText, next.body)
		add(fwd, imap.PartSpecifierMIME, l.forwardMIME())
		fields := sec(fwd, imap.PartSpecifierHeader)
		fields.HeaderFields = []string{"Subject"}
		out = append(out, sectionWant{sec: fields, want: fmt.Sprintf("Subject: level %d\r\n\r\n", k+1)})
		if l.wrapped {
			cover := appendPath(l.prefix, 1)
			add(cover, imap.PartSpecifierNone, fmt.Sprintf("forwarding level %d", k+1))
			add(cover, imap.PartSpecifierMIME, "Content-Type: text/plain\r\n\r\n")
			absent(appendPath(l.prefix, 3))
		} else {
			absent(appendPath(l.prefix, 2))
		}
	}

	inner := c.levels[depth]
	if c.attachment {
		add(appendPath(inner.prefix, 1), imap.PartSpecifierNone, "innermost text")
		add(appendPath(inner.prefix, 2), imap.PartSpecifierNone, base64.StdEncoding.EncodeToString(chainAttachment))
		absent(appendPath(inner.prefix, 3))
	} else {
		add(appendPath(inner.prefix, 1), imap.PartSpecifierNone, "innermost text\r\n")
		add(appendPath(inner.prefix, 1), imap.PartSpecifierMIME, inner.header)
		absent(appendPath(inner.prefix, 2))
	}
	absent(appendPath(inner.prefix, 1, 1))

	add(nil, imap.PartSpecifierText, c.levels[0].body)
	add(nil, imap.PartSpecifierHeader, c.levels[0].header)
	return out
}

func sectionName(sec *imap.FetchItemBodySection) string {
	name := partIntPath(sec.Part)
	if sec.Specifier != imap.PartSpecifierNone {
		if name != "" {
			name += "."
		}
		name += string(sec.Specifier)
	}
	if len(sec.HeaderFields) > 0 {
		name += ".FIELDS (" + strings.Join(sec.HeaderFields, " ") + ")"
	}
	return "BODY[" + name + "]"
}

// fetchSection answers one BODY[...] section from the raw message the way
// writeBodySection answers every section it does not stream from the blob.
func fetchSection(raw []byte, sec *imap.FetchItemBodySection) ([]byte, error) {
	e, err := resolveRawPart(raw, sec.Part)
	if err != nil {
		return nil, err
	}
	payload, ok := selectEntityPayload(sec, e)
	if !ok {
		return nil, fmt.Errorf("%s: no such section", sectionName(sec))
	}
	return payload, nil
}

// numberedPart is one part of a BODYSTRUCTURE with the number a client gives it.
type numberedPart struct {
	path []int
	part imap.BodyStructure
}

// rfcPartNumbers numbers every part of a BODYSTRUCTURE as a client does before
// it asks for BODY[n], following RFC 9051 §6.4.5:
//
//   - "Every message has at least one part number. Messages that do not use
//     MIME, and MIME messages that are not multipart and have no encapsulated
//     message within them, only have a part 1."
//   - "Multipart messages are assigned consecutive part numbers, as they occur
//     in the message. If a particular part is of type message or multipart,
//     its parts MUST be indicated by a period followed by the part number
//     within that nested multipart part."
//   - "A part of type MESSAGE/RFC822 or MESSAGE/GLOBAL also has nested part
//     numbers, referring to parts of the MIME body of that message part."
//
// So a message's parts are its multipart body's children, or else its body as
// part 1, and that applies again to the message a message/rfc822 part encloses,
// with that part's number as the prefix.
func rfcPartNumbers(bs imap.BodyStructure) []numberedPart {
	var out []numberedPart
	var message func(body imap.BodyStructure, prefix []int)
	var part func(p imap.BodyStructure, path []int)
	message = func(body imap.BodyStructure, prefix []int) {
		if mp, ok := body.(*imap.BodyStructureMultiPart); ok {
			for i, child := range mp.Children {
				part(child, appendPath(prefix, i+1))
			}
			return
		}
		part(body, appendPath(prefix, 1))
	}
	part = func(p imap.BodyStructure, path []int) {
		out = append(out, numberedPart{path: path, part: p})
		switch p := p.(type) {
		case *imap.BodyStructureMultiPart:
			for i, child := range p.Children {
				part(child, appendPath(path, i+1))
			}
		case *imap.BodyStructureSinglePart:
			if p.MessageRFC822 != nil {
				message(p.MessageRFC822.BodyStructure, path)
			}
		}
	}
	message(bs, nil)
	return out
}

func numberedPaths(parts []numberedPart) []string {
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = partIntPath(p.path)
	}
	sort.Strings(out)
	return out
}

// checkNumberedPart compares what BODYSTRUCTURE advertises for one part with
// the sections FETCH returns for it. body, mimeHeader, header and text are
// BODY[n], BODY[n.MIME], BODY[n.HEADER] and BODY[n.TEXT].
func checkNumberedPart(t *testing.T, p numberedPart, body, mimeHeader, header, text []byte) {
	t.Helper()
	name := partIntPath(p.path)
	mediaType := strings.ToLower(p.part.MediaType())
	if !strings.Contains(strings.ToLower(string(mimeHeader)), "content-type: "+mediaType) {
		t.Errorf("BODY[%s.MIME] = %q, BODYSTRUCTURE says the part is %s", name, mimeHeader, mediaType)
	}
	sp, ok := p.part.(*imap.BodyStructureSinglePart)
	if !ok {
		if len(body) == 0 {
			t.Errorf("BODY[%s] is empty, BODYSTRUCTURE says it is a %s part", name, mediaType)
		}
		return
	}
	if int64(len(body)) != int64(sp.Size) {
		t.Errorf("BODY[%s] is %d bytes, BODYSTRUCTURE says %d", name, len(body), sp.Size)
	}
	if sp.MessageRFC822 == nil {
		return
	}
	if string(header)+string(text) != string(body) {
		t.Errorf("BODY[%s.HEADER] + BODY[%s.TEXT] is not BODY[%s]:\n header %q\n text %q\n body %q",
			name, name, name, header, text, body)
	}
	if env := sp.MessageRFC822.Envelope; env == nil {
		t.Errorf("BODYSTRUCTURE of %s has no envelope", name)
	} else if want := "Subject: " + env.Subject + "\r\n"; !strings.Contains(string(header), want) {
		t.Errorf("BODY[%s.HEADER] = %q, but BODYSTRUCTURE's envelope has subject %q", name, header, env.Subject)
	}
}

// TestForwardChainSections pins every section of every chain shape against the
// bytes the chain was built from.
func TestForwardChainSections(t *testing.T) {
	for _, c := range forwardChains() {
		t.Run(c.name, func(t *testing.T) {
			for _, w := range c.sections() {
				got, err := fetchSection(c.raw, w.sec)
				switch {
				case w.absent:
					if !errors.Is(err, errPartNotFound) {
						t.Errorf("%s = %q, %v; want errPartNotFound", sectionName(w.sec), got, err)
					}
				case err != nil:
					t.Errorf("%s: %v", sectionName(w.sec), err)
				case string(got) != w.want:
					t.Errorf("%s\n got %q\nwant %q", sectionName(w.sec), got, w.want)
				}
			}
		})
	}
}

// TestForwardChainStructureMatchesSections is the BODYSTRUCTURE and FETCH
// round trip without the network. Ingest derives the structure, the reader
// converts it as FETCH BODYSTRUCTURE does, and every part a client numbers
// from it must resolve to what it advertises. Each attachment row ingest
// derives must also name the part holding its bytes.
func TestForwardChainStructureMatchesSections(t *testing.T) {
	parser := ingest.New(ingest.DefaultLimits())
	for _, c := range forwardChains() {
		t.Run(c.name, func(t *testing.T) {
			msg, err := parser.Parse(c.raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			stored, err := json.Marshal(msg.BodyStructure)
			if err != nil {
				t.Fatal(err)
			}
			bs, err := decodeBodyStructure(stored)
			if err != nil {
				t.Fatalf("decodeBodyStructure: %v", err)
			}
			parts := rfcPartNumbers(bs)
			if got, want := numberedPaths(parts), c.partPaths(); strings.Join(got, " ") != strings.Join(want, " ") {
				t.Fatalf("BODYSTRUCTURE numbers the parts %v, the chain has %v", got, want)
			}
			byPath := map[string]numberedPart{}
			for _, p := range parts {
				byPath[partIntPath(p.path)] = p
				get := func(spec imap.PartSpecifier) []byte {
					b, err := fetchSection(c.raw, &imap.FetchItemBodySection{Part: p.path, Specifier: spec})
					if err != nil {
						t.Errorf("BODY[%s] %s: %v", partIntPath(p.path), spec, err)
					}
					return b
				}
				checkNumberedPart(t, p, get(imap.PartSpecifierNone), get(imap.PartSpecifierMIME),
					get(imap.PartSpecifierHeader), get(imap.PartSpecifierText))
			}

			if c.attachment != (len(msg.Attachments) == 1) {
				t.Fatalf("ingest derived %d attachment(s)", len(msg.Attachments))
			}
			for _, a := range msg.Attachments {
				p, ok := byPath[a.PartNumber]
				if !ok {
					t.Errorf("attachment part number %q is not a part of the message (%v)", a.PartNumber, c.partPaths())
					continue
				}
				body, err := fetchSection(c.raw, &imap.FetchItemBodySection{Part: p.path})
				if err != nil {
					t.Fatalf("BODY[%s]: %v", a.PartNumber, err)
				}
				if decoded, err := base64.StdEncoding.DecodeString(string(body)); err != nil || string(decoded) != string(a.Data) {
					t.Errorf("BODY[%s] does not hold the attachment's bytes: %q", a.PartNumber, body)
				}
			}
		})
	}
}

// TestTopLevelTextIsTheMessageBody: with no part number, TEXT is the
// top-level message's body even when its Content-Type is message/rfc822. The
// reader used to enter the enclosed message and return that message's body.
func TestTopLevelTextIsTheMessageBody(t *testing.T) {
	c := buildForwardChain([]bool{false}, false)
	got, err := fetchSection(c.raw, &imap.FetchItemBodySection{Specifier: imap.PartSpecifierText})
	if err != nil {
		t.Fatal(err)
	}
	if want := c.levels[0].body; string(got) != want {
		t.Errorf("BODY[TEXT] = %q, want the top-level body %q", got, want)
	}
	if _, err := fetchSection(c.raw, &imap.FetchItemBodySection{Specifier: imap.PartSpecifierMIME}); err == nil {
		t.Error("BODY[MIME] without a part number was answered")
	}
}

// TestBareMultipartTypeIsSplitByBothSides: a Content-Type of "multipart" with
// no subtype is invalid, but ingest treated it as multipart and advertised its
// children while the reader treated it as a leaf. Both now ask
// ingest.IsMultipart.
func TestBareMultipartTypeIsSplitByBothSides(t *testing.T) {
	raw := []byte("From: a@ops004.invalid\r\nContent-Type: multipart/mixed; boundary=o\r\n\r\n" +
		"--o\r\nContent-Type: multipart; boundary=i\r\n\r\n" +
		"--i\r\nContent-Type: text/plain\r\n\r\nfirst\r\n--i\r\nContent-Type: text/plain\r\n\r\nsecond\r\n--i--\r\n" +
		"--o--\r\n")
	msg, err := ingest.New(ingest.DefaultLimits()).Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := json.Marshal(msg.BodyStructure)
	bs, err := decodeBodyStructure(stored)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range rfcPartNumbers(bs) {
		body, err := fetchSection(raw, &imap.FetchItemBodySection{Part: p.path})
		if err != nil {
			t.Errorf("BODY[%s] advertised but unresolvable: %v", partIntPath(p.path), err)
			continue
		}
		if sp, ok := p.part.(*imap.BodyStructureSinglePart); ok && len(body) != int(sp.Size) {
			t.Errorf("BODY[%s] is %d bytes, BODYSTRUCTURE says %d", partIntPath(p.path), len(body), sp.Size)
		}
	}
	got, err := fetchSection(raw, &imap.FetchItemBodySection{Part: []int{1, 2}})
	if err != nil || string(got) != "second" {
		t.Errorf("BODY[1.2] = %q, %v; want the bare multipart's second child", got, err)
	}
}

// chainWireClient serves fixture's mailbox through the real IMAP server on a
// loopback socket and returns a go-imap client connected to it, so FETCH
// responses are parsed by a client rather than by the test.
func chainWireClient(t *testing.T, fixture *Session) *imapclient.Client {
	t.Helper()
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			s := fixture.be.NewSession()
			s.mailboxID, s.mailboxName, s.tenant = fixture.mailboxID, fixture.mailboxName, fixture.tenant
			return s, &imapserver.GreetingData{PreAuth: true}, nil
		},
		Caps: imap.CapSet{imap.CapIMAP4rev1: {}},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	go srv.Serve(ln)
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.SetDeadline(time.Now().Add(60 * time.Second)); err != nil {
		t.Fatal(err)
	}
	client := imapclient.New(conn, nil)
	t.Cleanup(func() { client.Close() })
	return client
}

// TestForwardChainsOverTheWire is the end-to-end round trip. Every chain is
// APPENDed, so ingest stores its BODYSTRUCTURE and attachment rows. A go-imap
// client then reads BODYSTRUCTURE off the wire, numbers the parts per RFC 9051,
// and FETCHes BODY[n], n.MIME, n.HEADER and n.TEXT for each. Each must be what
// BODYSTRUCTURE advertises, every pinned section must be exact, and every
// stored attachment row's part number must FETCH the attachment's bytes.
func TestForwardChainsOverTheWire(t *testing.T) {
	sess, _ := appendFixture(t)
	mustCreateFolder(t, sess, "Chains")
	chains := forwardChains()
	for _, c := range chains {
		if _, err := sess.Append("Chains", newLiteral(c.raw), nil); err != nil {
			t.Fatalf("APPEND %s: %v", c.name, err)
		}
	}

	client := chainWireClient(t, sess)
	if _, err := client.Select("Chains", nil).Wait(); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	var all imap.SeqSet
	all.AddRange(1, uint32(len(chains)))
	listed, err := client.Fetch(all, &imap.FetchOptions{
		UID:           true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
	}).Collect()
	if err != nil {
		t.Fatalf("FETCH BODYSTRUCTURE: %v", err)
	}
	if len(listed) != len(chains) {
		t.Fatalf("FETCH returned %d messages, want %d", len(listed), len(chains))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, m := range listed {
		c := chains[m.SeqNum-1]
		t.Run(c.name, func(t *testing.T) {
			parts := rfcPartNumbers(m.BodyStructure)
			if got, want := numberedPaths(parts), c.partPaths(); strings.Join(got, " ") != strings.Join(want, " ") {
				t.Fatalf("BODYSTRUCTURE numbers the parts %v, the chain has %v", got, want)
			}

			var sections []*imap.FetchItemBodySection
			perPart := func(p numberedPart, spec imap.PartSpecifier) *imap.FetchItemBodySection {
				return &imap.FetchItemBodySection{Part: p.path, Specifier: spec, Peek: true}
			}
			for _, p := range parts {
				for _, spec := range []imap.PartSpecifier{imap.PartSpecifierNone, imap.PartSpecifierMIME,
					imap.PartSpecifierHeader, imap.PartSpecifierText} {
					sections = append(sections, perPart(p, spec))
				}
			}
			pinned := c.sections()
			for _, w := range pinned {
				sections = append(sections, w.sec)
			}
			fetched, err := client.Fetch(imap.UIDSetNum(m.UID), &imap.FetchOptions{BodySection: sections}).Collect()
			if err != nil || len(fetched) != 1 {
				t.Fatalf("FETCH sections: %d message(s), %v", len(fetched), err)
			}
			got := fetched[0]

			for _, p := range parts {
				checkNumberedPart(t, p,
					got.FindBodySection(perPart(p, imap.PartSpecifierNone)),
					got.FindBodySection(perPart(p, imap.PartSpecifierMIME)),
					got.FindBodySection(perPart(p, imap.PartSpecifierHeader)),
					got.FindBodySection(perPart(p, imap.PartSpecifierText)))
			}
			for _, w := range pinned {
				b := got.FindBodySection(w.sec)
				switch {
				case w.absent:
					if len(b) != 0 {
						t.Errorf("%s = %q, want an empty section for a part that does not exist", sectionName(w.sec), b)
					}
				case string(b) != w.want:
					t.Errorf("%s\n got %q\nwant %q", sectionName(w.sec), b, w.want)
				}
			}

			rows, err := sess.be.Pool.Query(ctx, `
				SELECT a.part_number, encode(a.sha256, 'hex')
				  FROM attachments a
				  JOIN messages m ON m.id = a.message_id
				  JOIN folders f ON f.id = m.folder_id
				 WHERE f.mailbox_id = $1 AND f.name = 'Chains' AND m.uid = $2`,
				sess.mailboxID, int64(m.UID))
			if err != nil {
				t.Fatal(err)
			}
			stored := map[string]string{}
			for rows.Next() {
				var partNumber, sha string
				if err := rows.Scan(&partNumber, &sha); err != nil {
					t.Fatal(err)
				}
				stored[partNumber] = sha
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if c.attachment != (len(stored) == 1) {
				t.Fatalf("attachment rows %v", stored)
			}
			for partNumber, sha := range stored {
				var path []int
				for _, p := range parts {
					if partIntPath(p.path) == partNumber {
						path = p.path
					}
				}
				if path == nil {
					t.Errorf("stored attachment part number %q is not a part of the message (%v)", partNumber, c.partPaths())
					continue
				}
				decoded, err := base64.StdEncoding.DecodeString(string(got.FindBodySection(perPart(numberedPart{path: path}, imap.PartSpecifierNone))))
				sum := sha256.Sum256(decoded)
				if err != nil || hex.EncodeToString(sum[:]) != sha {
					t.Errorf("BODY[%s] does not hold the stored attachment's bytes (%v)", partNumber, err)
				}
			}
		})
	}
}
