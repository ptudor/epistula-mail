package main

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

// TestFlagFilterRejectsMalformedValue is the RO5X-013 validation half.
//
// epistula-api accepted any `flag=`/`not_flag=` string and bound it into a
// byte-exact comparison, so a consumer passing `seen` or `Seen` silently got
// an empty page instead of an error.
func TestFlagFilterRejectsMalformedValue(t *testing.T) {
	f := newAPIFixture(t)

	for _, tc := range []struct {
		name  string
		query string
	}{
		{"empty-ish whitespace", "flag=%20"},
		{"embedded space", "flag=has%20space"},
		{"not_flag whitespace", "not_flag=has%20space"},
		{"control character", "flag=bad%00flag"},
		{"over 64 bytes", "flag=" + longFlag(65)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := f.do(http.MethodGet,
				"/v1/mailboxes/alice/folders/INBOX/messages?"+tc.query,
				f.classifierToken, nil)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Errorf("status = %d, want 422 for %s", resp.StatusCode, tc.query)
			}
		})
	}
}

// TestFlagFilterAcceptsValidValues keeps legitimate filters working, including
// keyword flags.
func TestFlagFilterAcceptsValidValues(t *testing.T) {
	f := newAPIFixture(t)

	for _, q := range []string{
		`flag=\Seen`, `flag=\SEEN`, `not_flag=\Seen`,
		`flag=$Forwarded`, `flag=NonJunk`, `flag=seen`,
	} {
		resp := f.do(http.MethodGet,
			"/v1/mailboxes/alice/folders/INBOX/messages?"+q, f.classifierToken, nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d for %s, want 200", resp.StatusCode, q)
		}
		resp.Body.Close()
	}
}

// TestFlagFilterCanonicalizesCase is the correctness half: a legacy row
// holding `\SEEN` is canonicalized by migration 011, and a caller passing any
// case of the system flag then finds it — so epistula-api, the MCP, and the IMAP
// client all agree about the same message.
func TestFlagFilterCanonicalizesCase(t *testing.T) {
	f := newAPIFixture(t)
	ctx := context.Background()

	// Mark one alice message \Seen (canonical, as every writer now does).
	id := f.aliceMsgIDs[0]
	if _, err := f.pool.Exec(ctx,
		`UPDATE messages SET flags = ARRAY['\Seen']::text[] WHERE id = $1`, id); err != nil {
		t.Fatalf("set flags: %v", err)
	}

	// Every spelling of the filter finds it.
	for _, q := range []string{`\Seen`, `\SEEN`, `\seen`} {
		var page listResp
		f.getJSON(fmt.Sprintf("/v1/mailboxes/alice/folders/INBOX/messages?flag=%s", urlEscape(q)),
			f.classifierToken, http.StatusOK, &page)
		var found bool
		for _, it := range page.Messages {
			if it.ID == id {
				found = true
			}
		}
		if !found {
			t.Errorf("flag=%s did not return the \\Seen message", q)
		}
	}
}

func longFlag(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

func urlEscape(s string) string {
	out := ""
	for _, c := range []byte(s) {
		if c == '\\' {
			out += "%5C"
		} else {
			out += string(c)
		}
	}
	return out
}
