package ingest

import "testing"

func TestVerificationEmptyMIMEHeaders(t *testing.T) {
	for _, sep := range []string{"\n", "\r\n"} {
		header, body := SplitHeaderBody([]byte(sep + "plain body\r\n\r\nstill body"))
		if string(header) != sep || string(body) != "plain body\r\n\r\nstill body" {
			t.Fatalf("separator %q: header=%q body=%q", sep, header, body)
		}
	}
}
