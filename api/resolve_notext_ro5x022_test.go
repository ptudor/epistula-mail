package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestRawAndAnnotationQueriesOmitTextBody is the RO5X-022 regression, asserted
// on the query constants themselves with a simple string check.
//
// resolveMessage unconditionally selected m.text_body, but only
// handleMessageText reads it. handleMessageRaw then streams the blob from disk
// (so the text was pure waste on top of a file read), and handleAnnotationPut —
// the daemon's single WRITE path — pulled the full decoded body of the message
// it was about to annotate purely to learn the owning mailbox name.
func TestRawAndAnnotationQueriesOmitTextBody(t *testing.T) {
	if strings.Contains(messageRefSelect, "text_body") {
		t.Errorf("messageRefSelect still selects text_body:\n%s", messageRefSelect)
	}
	// The text form must still fetch it, or /text would break.
	if !strings.Contains(messageRefWithTextSelect, "text_body") {
		t.Errorf("messageRefWithTextSelect does not select text_body:\n%s", messageRefWithTextSelect)
	}
	// Both must resolve the same scope facts, so the 403-vs-404 contract is
	// identical across forms.
	for _, col := range []string{"mb.name", "f.name", "raw_sha256", "raw_blob_date", "raw_size"} {
		if !strings.Contains(messageRefSelect, col) {
			t.Errorf("messageRefSelect is missing %s", col)
		}
		if !strings.Contains(messageRefWithTextSelect, col) {
			t.Errorf("messageRefWithTextSelect is missing %s", col)
		}
	}
}

// TestAnnotationPutUnaffectedByLargeBody is the behavioural check beside it:
// the write path still works, and still 204s, for a message with a large body.
func TestAnnotationPutUnaffectedByLargeBody(t *testing.T) {
	f := newAPIFixture(t)
	id := f.aliceMsgIDs[0]

	// 1 MiB body — the resolver must not be pulling this.
	big := strings.Repeat("x", 1<<20)
	if _, err := f.pool.Exec(context.Background(), `UPDATE messages SET text_body = $1 WHERE id = $2`, big, id); err != nil {
		t.Fatalf("set large body: %v", err)
	}

	body := strings.NewReader(`{"model":"m1","tags":["t"],"category":"c"}`)
	resp := f.do(http.MethodPut, fmt.Sprintf("/v1/messages/%d/annotation", id), f.classifierToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("annotation PUT status = %d, want 204", resp.StatusCode)
	}

	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM message_annotations WHERE message_id = $1 AND model = 'm1'`, id,
	).Scan(&n); err != nil {
		t.Fatalf("count annotations: %v", err)
	}
	if n != 1 {
		t.Errorf("annotation rows = %d, want 1", n)
	}

	// /text still serves the body.
	textResp := f.do(http.MethodGet, fmt.Sprintf("/v1/messages/%d/text", id), f.classifierToken, nil)
	defer textResp.Body.Close()
	if textResp.StatusCode != http.StatusOK {
		t.Errorf("/text status = %d, want 200", textResp.StatusCode)
	}
}
