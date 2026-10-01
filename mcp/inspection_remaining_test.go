package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMessageInspectionContinuation(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		if q.Get("metadata") != "inspection" || q.Get("attachment_after") != "part-4" || q.Get("annotation_after") != "model-4" || q.Get("annotation_model") != "model-9" || q.Get("summary_offset") != "16384" {
			t.Error("continuation parameters", q)
		}
		json.NewEncoder(w).Encode(map[string]any{"id": 42, "attachments": []any{}, "annotations": []any{map[string]any{"model": "model-9", "summary": "continued", "summary_offset": 16384, "summary_next_offset": 32768, "summary_bytes": 90000}}, "next_attachment": "part-8", "next_annotation": "model-8"})
	}))
	defer upstream.Close()
	ts := &toolset{cfg: config{MaxTextBytes: 4096}, c: newClientWithToken(config{BaseURL: upstream.URL, RequestTimeout: requestTimeoutForTest, MaxResponseBytes: defaultMaxResponseBytes}, "test-token")}
	result, _, err := ts.message(context.Background(), nil, messageInput{ID: 42, AttachmentAfter: "part-4", AnnotationAfter: "model-4", AnnotationModel: "model-9", SummaryOffset: 16384})
	out := decodeResult(t, result, err)
	if calls != 1 || out["next_attachment"] != "part-8" || out["next_annotation"] != "model-8" || out["metadata_truncated"] != true {
		t.Fatal("lost continuation", out, calls)
	}
	anns := asSlice(out["annotations"])
	if len(anns) != 1 || asMap(anns[0])["summary_next_offset"] == nil {
		t.Fatal("lost summary cursor", out)
	}
}
