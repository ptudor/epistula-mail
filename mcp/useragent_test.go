package main

import (
	"strings"
	"testing"
)

func TestUserAgentNamesConnectorBuildAndProject(t *testing.T) {
	ua := userAgent()
	if !strings.HasPrefix(ua, "epistula-mcp/"+Version+" ") {
		t.Errorf("userAgent() = %q, want the product token first", ua)
	}
	if !strings.HasSuffix(ua, "(+"+projectURL+")") {
		t.Errorf("userAgent() = %q, want the project URL as the comment", ua)
	}
	if strings.ContainsAny(ua, "\r\n") {
		t.Errorf("userAgent() = %q contains a line break", ua)
	}
}

func TestAddMailUserAgent(t *testing.T) {
	const ua = "epistula-mcp/test (+https://example.invalid)"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"LF message gets an LF field first",
			"From: a@x.invalid\nDate: d\n\nbody\n",
			"User-Agent: " + ua + "\nFrom: a@x.invalid\nDate: d\n\nbody\n"},
		{"CRLF message gets a CRLF field",
			"From: a@x.invalid\r\nDate: d\r\n\r\nbody\r\n",
			"User-Agent: " + ua + "\r\nFrom: a@x.invalid\r\nDate: d\r\n\r\nbody\r\n"},
		{"header without a final line ending",
			"From: a@x.invalid",
			"User-Agent: " + ua + "\nFrom: a@x.invalid"},
		{"empty input still gets the field",
			"",
			"User-Agent: " + ua + "\n"},
		{"existing User-Agent, any case, is kept",
			"from: a@x.invalid\nUSER-AGENT: Mutt\n\nbody\n",
			"from: a@x.invalid\nUSER-AGENT: Mutt\n\nbody\n"},
		{"existing X-Mailer is kept",
			"From: a@x.invalid\nX-Mailer: Client\n\nbody\n",
			"From: a@x.invalid\nX-Mailer: Client\n\nbody\n"},
		{"a DKIM-Signature forbids the addition",
			"DKIM-Signature: v=1; a=rsa-sha256; d=x.invalid;\n b=abc\nFrom: a@x.invalid\n\nbody\n",
			"DKIM-Signature: v=1; a=rsa-sha256; d=x.invalid;\n b=abc\nFrom: a@x.invalid\n\nbody\n"},
		{"a folded continuation is not a field",
			"Subject: quoting\n User-Agent: Mutt\n\nbody\n",
			"User-Agent: " + ua + "\nSubject: quoting\n User-Agent: Mutt\n\nbody\n"},
		{"a field-shaped body line is not a field",
			"From: a@x.invalid\n\nUser-Agent: Mutt\n",
			"User-Agent: " + ua + "\nFrom: a@x.invalid\n\nUser-Agent: Mutt\n"},
		{"a header-only CRLF message ending at the blank line",
			"From: a@x.invalid\r\n\r\n",
			"User-Agent: " + ua + "\r\nFrom: a@x.invalid\r\n\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(addMailUserAgent([]byte(tc.in), ua)); got != tc.want {
				t.Errorf("addMailUserAgent(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}
