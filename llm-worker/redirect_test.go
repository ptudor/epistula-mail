package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestSameOriginRedirect is the RA6X-039 truth table. It is duplicated verbatim
// in epistula-llm-worker and epistula-mcp alongside redirect.go, so the two copies of
// the policy cannot drift apart silently.
func TestSameOriginRedirect(t *testing.T) {
	cases := []struct {
		name    string
		from    string
		to      string
		allowed bool
	}{
		{
			name:    "same origin, canonicalising base path",
			from:    "https://api.mail.invalid/v1/messages",
			to:      "https://api.mail.invalid/v1/messages/",
			allowed: true,
		},
		{
			name:    "same origin with the default port written out",
			from:    "https://api.mail.invalid/v1/x",
			to:      "https://api.mail.invalid:443/v1/x",
			allowed: true,
		},
		{
			// The reported disclosure: same hostname, plaintext scheme. Go's
			// default policy forwards Authorization here.
			name:    "scheme downgrade to plaintext on the same host",
			from:    "https://api.mail.invalid/v1/messages",
			to:      "http://api.mail.invalid/v1/messages",
			allowed: false,
		},
		{
			name:    "scheme upgrade is still a different origin",
			from:    "http://api.mail.invalid/v1/messages",
			to:      "https://api.mail.invalid/v1/messages",
			allowed: false,
		},
		{
			name:    "different host",
			from:    "https://api.mail.invalid/v1/messages",
			to:      "https://collector.evil.invalid/v1/messages",
			allowed: false,
		},
		{
			name:    "sibling subdomain",
			from:    "https://api.mail.invalid/v1/messages",
			to:      "https://logs.mail.invalid/v1/messages",
			allowed: false,
		},
		{
			name:    "different port on the same host",
			from:    "https://api.mail.invalid/v1/messages",
			to:      "https://api.mail.invalid:8443/v1/messages",
			allowed: false,
		},
		{
			name:    "host case is insensitive",
			from:    "https://API.Mail.Invalid/v1/x",
			to:      "https://api.mail.invalid/v1/x",
			allowed: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from := mustURL(t, tc.from)
			to := mustURL(t, tc.to)
			err := sameOriginRedirect(
				&http.Request{URL: to},
				[]*http.Request{{URL: from}},
			)
			if tc.allowed && err != nil {
				t.Fatalf("redirect %s -> %s should be allowed, got %v", tc.from, tc.to, err)
			}
			if !tc.allowed && err == nil {
				t.Fatalf("redirect %s -> %s must be refused", tc.from, tc.to)
			}
			if err != nil {
				// Origins only: no path, no query, no userinfo.
				if strings.Contains(err.Error(), "/v1/") {
					t.Fatalf("error leaks the request path: %v", err)
				}
			}
		})
	}
}

// TestSameOriginRedirectBoundsHops pins that a same-origin redirect loop stops
// well before Go's 10-hop default, so a misconfigured proxy cannot make the
// client re-present its credential ten times.
func TestSameOriginRedirectBoundsHops(t *testing.T) {
	u := mustURL(t, "https://api.mail.invalid/v1/x")
	via := make([]*http.Request, 0, maxRedirectHops+1)
	for i := 0; i <= maxRedirectHops; i++ {
		via = append(via, &http.Request{URL: u})
		err := sameOriginRedirect(&http.Request{URL: u}, via)
		if i < maxRedirectHops && err != nil {
			t.Fatalf("hop %d should be allowed, got %v", i, err)
		}
		if i == maxRedirectHops && err == nil {
			t.Fatalf("hop %d must be refused", i)
		}
	}
}

// TestSameOriginRedirectFirstRequest pins that the very first request (no
// history) is never treated as a redirect.
func TestSameOriginRedirectFirstRequest(t *testing.T) {
	u := mustURL(t, "https://api.mail.invalid/v1/x")
	if err := sameOriginRedirect(&http.Request{URL: u}, nil); err != nil {
		t.Fatalf("initial request must not be refused: %v", err)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}
