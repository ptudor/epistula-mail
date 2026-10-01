package main

import (
	"strings"
	"testing"
)

// TestExportErrorsNeverLeakBodyOrSubject is the R-036 regression: neither the
// structural in-band-error path nor the decode-failure path may echo the raw
// NDJSON row (which carries subject, addresses, and full text_body) into the
// error that serve.go logs and surfaces on /status.
func TestExportErrorsNeverLeakBodyOrSubject(t *testing.T) {
	const secretSubject = "SECRETSUBJECT_jdoe"
	const secretBody = "SECRETBODY_confidential_content"

	t.Run("structural in-band error", func(t *testing.T) {
		// A sentinel row (non-empty "error", no positive id) that ALSO carries a
		// subject/body — the error must include only the short server string.
		body := `{"error":"backend down","subject":"` + secretSubject +
			`","text_body":"` + secretBody + `"}` + "\n"
		srv := serveNDJSON(t, body)
		defer srv.Close()

		_, err := exportAll(t, srv)
		if err == nil {
			t.Fatal("expected abort on the in-band sentinel")
		}
		assertNoLeak(t, err.Error(), secretSubject, secretBody)
		if !strings.Contains(err.Error(), "backend down") {
			t.Errorf("err = %q, want the server's short error string", err)
		}
	})

	t.Run("decode failure", func(t *testing.T) {
		// Valid JSON for the probe (id > 0, no error) but the wrong type for a
		// Message field, so json.Unmarshal into Message fails — the error must
		// carry the byte length, not the payload.
		body := `{"id":5,"subject":"` + secretSubject + `","text_body":"` + secretBody +
			`","flags":"not-an-array"}` + "\n"
		srv := serveNDJSON(t, body)
		defer srv.Close()

		_, err := exportAll(t, srv)
		if err == nil {
			t.Fatal("expected a decode failure on the type-mismatched row")
		}
		assertNoLeak(t, err.Error(), secretSubject, secretBody)
		if !strings.Contains(err.Error(), "bytes") {
			t.Errorf("decode error = %q, want it to name the byte length", err)
		}
	})
}

func assertNoLeak(t *testing.T, msg, subject, body string) {
	t.Helper()
	if strings.Contains(msg, subject) {
		t.Errorf("error leaked the subject: %q", msg)
	}
	if strings.Contains(msg, body) {
		t.Errorf("error leaked the body: %q", msg)
	}
}
