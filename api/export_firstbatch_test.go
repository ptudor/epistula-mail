package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestAPIExportFirstBatchFailureIsProblem5xx is the R-015 regression: when the
// first export batch fails (backend error) before any header is written, the
// response must be an RFC 7807 5xx problem+json — never 200 + an in-band error
// row that a consumer can only detect heuristically. We break only the export
// query (DROP messages) so token auth (api_tokens) still succeeds and the
// request actually reaches the export handler's first batch.
func TestAPIExportFirstBatchFailureIsProblem5xx(t *testing.T) {
	f := newAPIFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := f.pool.Exec(ctx, `DROP TABLE messages CASCADE`); err != nil {
		t.Fatalf("drop messages: %v", err)
	}

	resp := f.do(http.MethodGet, "/v1/export?mailbox=alice", f.classifierToken, nil)
	defer resp.Body.Close()

	if resp.StatusCode < 500 || resp.StatusCode >= 600 {
		t.Fatalf("export first-batch-failure status = %d, want 5xx", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}
