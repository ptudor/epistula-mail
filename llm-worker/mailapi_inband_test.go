package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serveNDJSON returns a test server that writes the given raw NDJSON body.
func serveNDJSON(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("server write: %v", err)
		}
	}))
}

func exportAll(t *testing.T, srv *httptest.Server) ([]int64, error) {
	t.Helper()
	client, err := NewMailAPIClient(MailAPIConfig{BaseURL: srv.URL, Token: "x", RequestTimeoutSec: 5})
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	err = client.Export(context.Background(), MailAPIConfig{}, "", func(m Message) error {
		ids = append(ids, m.ID)
		return nil
	})
	return ids, err
}

// TestExportErrorTagIsNotSentinel is the R-010 regression: a legitimate row
// whose tag/subject is literally "error" must reach the handler, not be
// mistaken for the in-band error sentinel (which permanently stalled other
// models at that row under the old substring match).
func TestExportErrorTagIsNotSentinel(t *testing.T) {
	body := `{"id":1,"uid":1,"internal_date":"2026-06-01T00:00:00Z","flags":[],"size":1,"attachment_count":0,"annotations":[{"model":"m","tags":["error"],"category":"other"}]}` + "\n" +
		`{"id":2,"uid":2,"subject":"error","internal_date":"2026-06-01T00:00:00Z","flags":[],"size":1,"attachment_count":0}` + "\n"
	srv := serveNDJSON(t, body)
	defer srv.Close()

	ids, err := exportAll(t, srv)
	if err != nil {
		t.Fatalf("Export: %v (rows with 'error' tag/subject must not abort)", err)
	}
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("ids = %v, want [1 2]", ids)
	}
}

// TestExportRealSentinelAborts confirms the structural sentinel — an object
// with a non-empty "error" and no positive id — still aborts, and that the
// error carries only the server's short string, not the row payload.
func TestExportRealSentinelAborts(t *testing.T) {
	body := `{"id":1,"uid":1,"internal_date":"2026-06-01T00:00:00Z","flags":[],"size":1,"attachment_count":0}` + "\n" +
		`{"error":"export aborted: backend failure"}` + "\n"
	srv := serveNDJSON(t, body)
	defer srv.Close()

	ids, err := exportAll(t, srv)
	if err == nil {
		t.Fatal("expected abort on the in-band error sentinel")
	}
	if !strings.Contains(err.Error(), "export aborted: backend failure") {
		t.Fatalf("err = %q, want the server's error string", err)
	}
	if len(ids) != 1 {
		t.Fatalf("processed %d rows before the sentinel, want 1", len(ids))
	}
}

// TestExportHandlesOversizedRow is the R-032 regression: a row larger than the
// old 32 MiB bufio.Scanner cap must decode (json.Decoder has no line cap).
func TestExportHandlesOversizedRow(t *testing.T) {
	// 33 MiB of body text — one byte past the old cap.
	big := strings.Repeat("a", 33*1024*1024)
	body := `{"id":7,"uid":7,"internal_date":"2026-06-01T00:00:00Z","flags":[],"size":1,"attachment_count":0,"text_body":"` + big + `"}` + "\n"
	srv := serveNDJSON(t, body)
	defer srv.Close()

	ids, err := exportAll(t, srv)
	if err != nil {
		t.Fatalf("Export of a 33 MiB row: %v (json.Decoder must not cap line length)", err)
	}
	if len(ids) != 1 || ids[0] != 7 {
		t.Fatalf("ids = %v, want [7]", ids)
	}
}
