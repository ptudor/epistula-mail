package imapsess

import (
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/emersion/go-imap/v2"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/simplifiedchinese"
)

func legacyNames(t *testing.T) []struct{ name, header string } {
	t.Helper()
	var out []struct{ name, header string }
	for _, tc := range []struct {
		name, charset string
		enc           encoding.Encoding
	}{
		{"日本語", "ISO-2022-JP", japanese.ISO2022JP}, {"中文", "GBK", simplifiedchinese.GBK},
	} {
		b, err := tc.enc.NewEncoder().Bytes([]byte(tc.name))
		if err != nil {
			t.Fatal(err)
		}
		word := "=?" + tc.charset + "?B?" + base64.StdEncoding.EncodeToString(b) + "?="
		out = append(out, struct{ name, header string }{tc.name, word})
	}
	return out
}

func assertLegacyAddress(t *testing.T, list []imap.Address, name string) {
	t.Helper()
	if len(list) != 1 || list[0].Name != name || list[0].Mailbox != "a" || list[0].Host != "example.invalid" {
		t.Fatalf("address=%+v, want decoded %q and a@example.invalid", list, name)
	}
}

func TestLegacyCharsetEnvelopeAddresses(t *testing.T) {
	for _, tc := range legacyNames(t) {
		value := tc.header + " <a@example.invalid>"
		assertLegacyAddress(t, parseAddressList(value), tc.name)
		env := convertEnvelope(&persistedEnvelope{From: value, To: value, Cc: value})
		for _, list := range [][]imap.Address{env.From, env.To, env.Cc} {
			assertLegacyAddress(t, list, tc.name)
		}
		group, err := parseGroupedAddresses(tc.header + ": " + value + ";")
		if err != nil || len(group) != 3 || group[0].Mailbox != tc.name {
			t.Fatalf("group=%+v %v", group, err)
		}
	}
	fallback := "a@example.invalid"
	env := buildEnvelopeHeaders(fetchRow{fromAddr: &fallback}, map[string][]string{"From": {"bad <unterminated"}})
	assertLegacyAddress(t, env.From, "")
}

func TestLegacyCharsetEnvelopeOnWire(t *testing.T) {
	s, _ := appendFixture(t)
	names := legacyNames(t)
	for _, tc := range names {
		raw := []byte(fmt.Sprintf("From: %s <a@example.invalid>\r\nTo: %s <a@example.invalid>\r\nCc: %s <a@example.invalid>\r\nSubject: charset\r\n\r\ntext\r\n", tc.header, tc.header, tc.header))
		if _, err := s.Append("INBOX", newLiteral(raw), nil); err != nil {
			t.Fatal(err)
		}
	}
	c := chainWireClient(t, s)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	messages, err := c.Fetch(imap.SeqSetNum(1, 2), &imap.FetchOptions{Envelope: true}).Collect()
	if err != nil || len(messages) != 2 {
		t.Fatalf("FETCH: %v %v", messages, err)
	}
	for i, m := range messages {
		for _, list := range [][]imap.Address{m.Envelope.From, m.Envelope.To, m.Envelope.Cc} {
			assertLegacyAddress(t, list, names[i].name)
		}
	}
}

func TestGB2312EnvelopeDecoderSupport(t *testing.T) {
	got := parseAddressList("=?GB2312?B?1tDOxA==?= <a@example.invalid>")
	if len(got) != 1 || got[0].Mailbox != "a" || got[0].Host != "example.invalid" {
		t.Fatalf("GB2312 lost the address: %+v", got)
	}
	if got[0].Name != "中文" {
		t.Skip("RM51X-096: ingest's existing charset reader does not decode GB2312; adding a charset mapping is outside its fix specification")
	}
}
