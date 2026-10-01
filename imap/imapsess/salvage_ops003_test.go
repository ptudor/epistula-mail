package imapsess

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/ptudor/epistula-mail/database/ingest"
)

// salvageFixtures are synthetic messages of every shape ingest used to refuse
// and now stores (OPS-003), keyed by the error ingest used to return.
func salvageFixtures() map[string]string {
	deep := "From: a@ops003.invalid\r\nSubject: deep\r\n"
	for i := 0; i < 12; i++ {
		deep += fmt.Sprintf("Content-Type: multipart/mixed; boundary=B%d\r\n\r\n--B%d\r\n", i, i)
	}
	deep += "Content-Type: text/plain\r\n\r\ndeepest\r\n"
	for i := 11; i >= 0; i-- {
		deep += fmt.Sprintf("--B%d--\r\n", i)
	}
	// Forwards of forwards, each inside a multipart/mixed as mail clients
	// send them. Chains with no multipart around a forward are covered in
	// rfc822chain_ops004_test.go.
	wrapped := "From: deepest@ops003.invalid\r\n\r\nbottom\r\n"
	for i := 0; i < 7; i++ {
		wrapped = fmt.Sprintf("From: wrap@ops003.invalid\r\nContent-Type: multipart/mixed; boundary=W%d\r\n\r\n"+
			"--W%d\r\nContent-Type: text/plain\r\n\r\nforwarding\r\n"+
			"--W%d\r\nContent-Type: message/rfc822\r\n\r\n%s\r\n--W%d--\r\n", i, i, i, wrapped, i)
	}
	attachment := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 cut short"))
	enclosed := base64.StdEncoding.EncodeToString([]byte("From: in@ops003.invalid\r\n\r\ninner\r\n"))
	return map[string]string{
		"read part: unexpected EOF": "From: a@ops003.invalid\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
			"--b\r\nContent-Type: text/plain\r\n\r\nfirst\r\n--b\r\nContent-Type: text/html\r\n\r\n<p>cut off",
		"mixed line endings": "From: a@ops003.invalid\nContent-Type: multipart/mixed; boundary=b\n\n" +
			"--b\r\nContent-Type: text/plain\r\n\r\nhello\n--b\nContent-Type: text/plain\n\nsecond\n--b--\n",
		"unclosed inner multipart": "From: a@ops003.invalid\nContent-Type: multipart/mixed; boundary=o\n\n" +
			"--o\nContent-Type: multipart/alternative; boundary=i\n\n--i\nContent-Type: text/plain\n\ninner\n" +
			"--o\nContent-Type: text/plain\n\nouter\n--o--\n",
		"no header/body separator": "From: a@ops003.invalid\nSubject: header only\n",
		"lf field, crlf empty line": "From: a@ops003.invalid\nSubject: s\n\r\n" + strings.Repeat("y", 20000) +
			"\r\n\r\nlater\r\n",
		"illegal base64 data": "From: a@ops003.invalid\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
			base64.StdEncoding.EncodeToString([]byte("text")) + "\r\n-- \r\nfooter\r\n",
		"transfer decode: unexpected EOF": "From: a@ops003.invalid\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
			"--b\r\nContent-Type: text/plain\r\n\r\nsee attached\r\n" +
			"--b\r\nContent-Type: application/pdf\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
			attachment[:len(attachment)-3] + "\r\n--b--\r\n",
		"undecodable enclosed message": "From: a@ops003.invalid\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
			"--b\r\nContent-Type: message/rfc822\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
			enclosed[:len(enclosed)-2] + "\r\n--b--\r\n",
		"unparseable part header": "From: a@ops003.invalid\nContent-Type: multipart/mixed; boundary=b\n\n" +
			"--b\nContent-Type: text/plain\nContent-Transf\n\nopaque\n--b\n\nread\n--b--\n",
		"MIME depth exceeds MaxMimeDepth":   deep,
		"rfc822 depth exceeds MaxMimeDepth": wrapped,
		"single header exceeds MaxHeaderBytes": "From: a@ops003.invalid\r\nReferences: " +
			strings.Repeat("<ref@list.invalid> ", 1000) + "\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
			"--b\r\nContent-Type: text/plain\r\n\r\nreply\r\n--b--\r\n",
	}
}

// TestAppendSalvagesButKeepsLimits: APPEND shares ingest's refusing parser
// with delivery (OPS-003). A message a mail client would show, such as one
// whose multipart body was cut short, is appended and logged as degraded; a
// resource limit is still refused.
func TestAppendSalvagesButKeepsLimits(t *testing.T) {
	sess, _ := appendFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	mustCreateFolder(t, sess, "Archive")
	fixtures := salvageFixtures()

	if _, err := sess.Append("Archive", newLiteral([]byte(fixtures["read part: unexpected EOF"])), nil); err != nil {
		t.Fatalf("APPEND of a truncated multipart: %v", err)
	}
	var outcome, detail string
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT outcome, coalesce(error_detail, '') FROM delivery_log ORDER BY id DESC LIMIT 1`,
	).Scan(&outcome, &detail); err != nil {
		t.Fatal(err)
	}
	if outcome != "appended:degraded" || !strings.Contains(detail, ingest.DefectMissingCloseDelimiter) {
		t.Errorf("logged %q / %q, want appended:degraded with the missing delimiter", outcome, detail)
	}

	_, err := sess.Append("Archive", newLiteral([]byte(fixtures["MIME depth exceeds MaxMimeDepth"])), nil)
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) || imapErr.Type != imap.StatusResponseTypeNo {
		t.Errorf("APPEND past MaxMimeDepth: err = %v, want NO", err)
	}
}

// TestSalvagedStructureLinesUpWithRawParts is the OPS-003 cross-check between
// the writer and this reader. A client addresses BODY[n] from the stored
// BODYSTRUCTURE, and resolveRawPart answers it from the raw bytes; ingest now
// stores messages it used to refuse, so for every part path such a structure
// advertises, the part resolved here must have exactly the advertised size.
func TestSalvagedStructureLinesUpWithRawParts(t *testing.T) {
	parser := ingest.NewSalvaging(ingest.DefaultLimits())
	for name, raw := range salvageFixtures() {
		t.Run(name, func(t *testing.T) {
			msg, err := parser.Parse([]byte(raw))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			checked := 0
			// addressable is false for a node with no part number of its own:
			// the top-level multipart, and an enclosed message's multipart
			// body, whose children share the message/rfc822 part's number.
			var visit func(n ingest.BodyStructure, path []int, addressable bool)
			visit = func(n ingest.BodyStructure, path []int, addressable bool) {
				if addressable {
					e, err := resolveRawPart([]byte(raw), path)
					if err != nil {
						t.Errorf("BODY[%s] advertised but unresolvable: %v", partIntPath(path), err)
						return
					}
					if int64(len(e.body)) != n.Size {
						t.Errorf("BODY[%s] is %d bytes, BODYSTRUCTURE says %d", partIntPath(path), len(e.body), n.Size)
					}
					checked++
				}
				if n.Type == "message" && n.Subtype == "rfc822" {
					if len(n.Parts) == 1 {
						if n.Parts[0].Type == "multipart" {
							visit(n.Parts[0], path, false)
						} else {
							visit(n.Parts[0], append(append([]int(nil), path...), 1), true)
						}
					}
					return
				}
				for i, child := range n.Parts {
					visit(child, append(append([]int(nil), path...), i+1), true)
				}
			}
			// A message that is not multipart has its body as part 1, whatever
			// its type (RFC 9051 §6.4.5, OPS-004).
			root := msg.BodyStructure
			if root.Type == "multipart" {
				visit(root, nil, false)
			} else {
				visit(root, []int{1}, true)
			}
			if checked == 0 && len(root.Parts) > 0 {
				t.Error("no part path was checked")
			}

			// BODY[HEADER] from the streamed read is the header ingest parsed.
			wantHeader, wantBody := ingest.SplitHeaderBody([]byte(raw))
			got, err := readHeaderSection(bytes.NewReader([]byte(raw)))
			if err != nil || !bytes.Equal(got, wantHeader) {
				t.Errorf("readHeaderSection = %q, %v; want %q", got, err, wantHeader)
			}
			if int64(len(wantBody)) != root.Size {
				t.Errorf("BODY[TEXT] is %d bytes, the stored body %d", len(wantBody), root.Size)
			}
			if len(msg.Defects) == 0 && name != "no header/body separator" && name != "lf field, crlf empty line" && name != "mixed line endings" {
				t.Error("a salvaged message carries no defect")
			}
		})
	}
}
