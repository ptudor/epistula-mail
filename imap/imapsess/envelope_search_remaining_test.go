package imapsess

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestRemainingEnvelopeGroupsOnWire(t *testing.T) {
	s, _ := appendFixture(t)
	mustCreateFolder(t, s, "INBOX")
	inner := "Subject: forwarded\r\nFrom: Inner <inner@example.invalid>\r\nTo: Empty:;\r\n\r\nforward body"
	raw := "Subject: groups\r\nFrom: =?UTF-8?Q?Last=2C_First?= <alice@example.invalid>\r\nFrom: Second <second@example.invalid>\r\nTo: =?UTF-8?Q?Team=3A_West?=: Bob <bob@example.invalid>, Carol <carol@example.invalid>;, Empty:;\r\nBcc: Hidden <hidden@example.invalid>\r\nReply-To: Reply <reply@example.invalid>\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: message/rfc822\r\n\r\n" + inner + "\r\n--b--\r\n"
	if _, err := s.Append("INBOX", newLiteral([]byte(raw)), &imap.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	client := verificationWire(t, s)
	client("SELECT INBOX")
	got := client("FETCH 1 (ENVELOPE BODYSTRUCTURE)")
	for _, want := range []string{`ENVELOPE (NIL "groups"`, `"Last, First" NIL "alice"`, `"Second" NIL "second"`, `(NIL NIL "Team: West" NIL)`, `"Bob" NIL "bob"`, `"Carol" NIL "carol"`, `(NIL NIL "Empty" NIL) (NIL NIL NIL NIL)`, `"Reply" NIL "reply"`, `"Hidden" NIL "hidden"`, `(NIL "forwarded"`, `"Inner" NIL "inner"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in wire envelope: %s", want, got)
		}
	}
	// Sender fallback preserves both From values, independently of Reply-To.
	if strings.Count(got, `"Last, First" NIL "alice"`) != 2 {
		t.Fatalf("Sender fallback lost From values: %s", got)
	}
	invalid := "Date: not a date\r\nFrom: A <a@example.invalid>\r\nSubject: invalid date\r\n\r\nbody"
	if _, err := s.Append("INBOX", newLiteral([]byte(invalid)), &imap.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	client("NOOP")
	if got := client("FETCH 2 ENVELOPE"); !strings.Contains(got, `ENVELOPE (NIL "invalid date"`) {
		t.Fatalf("invalid Date substituted: %s", got)
	}
}

func TestRemainingAddressGroupPunctuation(t *testing.T) {
	for _, input := range []string{`"Team, West": "A; B" <a@example.invalid>;, Empty:;`, `Team (comment: x): "a:b"@example.invalid;, Empty:;`, `=?UTF-8?Q?Team=3A_West?=: A <a@example.invalid>;, Empty:;`} {
		got, err := parseGroupedAddresses(input)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 5 || !got[0].IsGroupStart() || !got[2].IsGroupEnd() || got[3].Mailbox != "Empty" || !got[4].IsGroupEnd() {
			t.Fatalf("group structure %q: %+v", input, got)
		}
	}
}

func TestRemainingTextSearchUsesHeaderText(t *testing.T) {
	s := mutationFixture(t)
	ctx := context.Background()
	headers, _ := json.Marshal(map[string][]string{"X-Quote": {`a "quoted" value`}, "X-Path": {`c:\folder\file`}, "X-Repeated": {"first", "second"}})
	if _, err := s.be.Pool.Exec(ctx, `UPDATE messages SET headers=$2,html_body='<p>HTMLmarker</p>' WHERE folder_id=$1 AND uid=1`, s.selectedFolderID, headers); err != nil {
		t.Fatal(err)
	}
	for _, term := range []string{`a "quoted" value`, `X-Quote: a "quoted" value`, `c:\folder\file`, `X-Repeated: second`, `HTMLmarker`} {
		if got := searchUIDs(t, s, &imap.SearchCriteria{Text: []string{term}}); !containsUID(got, 1) {
			t.Fatalf("TEXT missed %q: %v", term, got)
		}
	}
	for _, term := range []string{`["first", "second"]`, `\"quoted\"`, `X-Quote": [`} {
		if got := searchUIDs(t, s, &imap.SearchCriteria{Text: []string{term}}); containsUID(got, 1) {
			t.Fatalf("TEXT matched JSON artifacts %q: %v", term, got)
		}
	}
	if got := searchUIDs(t, s, &imap.SearchCriteria{Body: []string{`a "quoted" value`}}); containsUID(got, 1) {
		t.Fatal("BODY included headers")
	}
	if got := searchUIDs(t, s, &imap.SearchCriteria{Not: []imap.SearchCriteria{{Text: []string{`a "quoted" value`}}}}); containsUID(got, 1) {
		t.Fatal("NOT TEXT failed")
	}
}
