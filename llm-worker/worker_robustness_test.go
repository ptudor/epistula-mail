package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// ---- R-035: junk-annotation guard ---------------------------------------

func TestDecodeAnnotationRequiresSchemaKeys(t *testing.T) {
	if _, err := decodeAnnotation("{}", ""); err == nil {
		t.Error(`decodeAnnotation("{}") returned nil error; want reject (missing required keys)`)
	}
	if _, err := decodeAnnotation(`{"note":"x"}`, ""); err == nil {
		t.Error(`decodeAnnotation({"note":"x"}) returned nil error; want reject`)
	}
	// An EMPTY summary is no longer acceptable (RA6X-019). This test used to
	// assert the opposite — "present-but-empty fields are acceptable, the keys
	// are there" — and that is exactly how a model that produced nothing
	// usable got recorded as having annotated the message: normalizeAnnotation
	// substituted "(no useful summary produced)", the PUT succeeded under this
	// model's name, and every later pass skipped the message. The reversal is
	// deliberate.
	if _, err := decodeAnnotation(`{"summary":"","tags":[],"category":"other"}`, ""); err == nil {
		t.Error("an empty summary decoded; a model that said nothing must not look annotated")
	}
	// An empty TAGS array stays valid: the schema asks for a list, and "no
	// tags apply" is a real answer, distinct from null.
	ann, err := decodeAnnotation(`{"summary":"a real summary","tags":[],"category":"other"}`, "")
	if err != nil {
		t.Fatalf("an empty tag list should decode: %v", err)
	}
	if ann.Category != "other" {
		t.Errorf("category = %q, want other", ann.Category)
	}
	if len(ann.Tags) != 0 {
		t.Errorf("tags = %v, want empty", ann.Tags)
	}
}

// ---- R-034: prefer the last valid object; finish_reason=length aborts ----

func TestDecodeAnnotationPrefersLastValidObject(t *testing.T) {
	// A completed <think> draft precedes the real answer; the balanced-brace
	// scanner must accept the LAST valid object, not the first (the old
	// first-'{'-to-last-'}' span captured the draft or produced invalid JSON).
	content := `<think>{"summary":"draft","tags":["x"],"category":"personal"}</think>` +
		`{"summary":"real","tags":["y"],"category":"receipt"}`
	ann, err := decodeAnnotation(content, "")
	if err != nil {
		t.Fatalf("decodeAnnotation: %v", err)
	}
	if ann.Summary != "real" || ann.Category != "receipt" {
		t.Fatalf("got %#v, want the last (real) object", ann)
	}
}

func TestAnnotateTruncatedResponseAborts(t *testing.T) {
	// finish_reason=length + a complete <think> draft: must NOT promote the
	// draft; return a retryable truncation error instead.
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{
		  "choices":[{"finish_reason":"length","message":{
		    "content":"",
		    "reasoning_content":"{\"summary\":\"draft\",\"tags\":[\"x\"],\"category\":\"personal\"}"
		  }}]
		}`), nil
	})
	cfg := DefaultConfig().LMStudio
	cfg.BaseURL = "http://lmstudio.test/v1"
	client, err := NewLMStudioClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	client.client = &http.Client{Transport: rt}
	if _, _, err := client.Annotate(context.Background(), Prompt{System: "s", User: "u"}); err == nil {
		t.Fatal("expected an error on finish_reason=length; the draft must not be stored")
	} else if !strings.Contains(err.Error(), "finish_reason=length") {
		t.Fatalf("err = %v, want a truncation error", err)
	}
}

// ---- R-033: tag/model clamps --------------------------------------------

func TestNormalizeAnnotationClampsTagLength(t *testing.T) {
	cfg := validatableDefault()
	put := normalizeAnnotation(cfg, LLMAnnotation{
		Summary:  "s",
		Tags:     []string{strings.Repeat("a", 300)},
		Category: "other",
	})
	for _, tag := range put.Tags {
		if len(tag) > apiMaxTagBytes {
			t.Fatalf("tag length %d exceeds epistula-api contract %d", len(tag), apiMaxTagBytes)
		}
	}
}

func TestValidateRejectsContractBreakingLimits(t *testing.T) {
	base := func() *Config {
		cfg := validatableDefault()
		cfg.MailAPI.BaseURL = "https://mail.example.test"
		cfg.MailAPI.Token = "t"
		return cfg
	}
	c1 := base()
	c1.Worker.MaxTags = 100
	if err := c1.Validate(); err == nil {
		t.Error("Validate accepted max_tags=100 (> contract 64)")
	}
	c2 := base()
	c2.Worker.AnnotationModel = "lmstudio:" + strings.Repeat("x", 200)
	if err := c2.Validate(); err == nil {
		t.Error("Validate accepted a >128-byte annotation_model")
	}
}

// ---- R-037: failure-class discrimination --------------------------------

func newTestWorker(t *testing.T, exportSrvURL string, llmRT http.RoundTripper) *Worker {
	t.Helper()
	cfg := validatableDefault()
	cfg.MailAPI.BaseURL = exportSrvURL
	cfg.MailAPI.Token = "x"
	cfg.MailAPI.RequestTimeoutSec = 5
	cfg.LMStudio.BaseURL = "http://lmstudio.test/v1"
	cfg.Worker.RetryAttempts = 0 // one LLM call per message; no backoff sleeps
	cfg.Worker.SkipAnnotated = false
	cfg.Worker.DryRun = true // avoid needing a PUT mock

	mail, err := NewMailAPIClient(cfg.MailAPI)
	if err != nil {
		t.Fatal(err)
	}
	llm, err := NewLMStudioClient(cfg.LMStudio)
	if err != nil {
		t.Fatal(err)
	}
	llm.client = &http.Client{Transport: llmRT}
	return &Worker{cfg: cfg, mail: mail, llm: llm}
}

func exportServer(t *testing.T, rows int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		for i := 1; i <= rows; i++ {
			io.WriteString(w, ndjsonRow(int64(i)))
		}
	}))
}

// TestRunOnceAbortsOnConsecutiveInfraFailures: LM Studio always refusing
// (transport error) must abort the pass after the threshold rather than churn
// the whole 10-row corpus.
func TestRunOnceAbortsOnConsecutiveInfraFailures(t *testing.T) {
	srv := exportServer(t, 10)
	defer srv.Close()

	refusing := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Post", URL: r.URL.String(), Err: errors.New("connect: connection refused")}
	})
	w := newTestWorker(t, srv.URL, refusing)

	stats, err := w.RunOnce(context.Background())
	if err == nil {
		t.Fatal("expected the pass to abort on consecutive infra failures")
	}
	if !strings.Contains(err.Error(), "consecutive infrastructure failures") {
		t.Fatalf("err = %v, want an infra-abort error", err)
	}
	if stats.Annotated != 0 {
		t.Errorf("Annotated = %d, want 0", stats.Annotated)
	}
	if stats.Scanned > consecutiveInfraFailureAbort {
		t.Errorf("Scanned = %d, want <= %d (aborted early, not the whole corpus)", stats.Scanned, consecutiveInfraFailureAbort)
	}
}

// TestRunOnceContinuesOnPerMessageContentError: a single malformed LLM output
// among 10 must not abort the pass — Failed==1, the rest annotated.
func TestRunOnceContinuesOnPerMessageContentError(t *testing.T) {
	srv := exportServer(t, 10)
	defer srv.Close()

	var call int
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		call++
		if call == 5 {
			// Malformed content — a per-message decode failure, not infra.
			return jsonResponse(http.StatusOK, `{"choices":[{"message":{"content":"not json at all"}}]}`), nil
		}
		return jsonResponse(http.StatusOK, `{"choices":[{"message":{"content":"{\"summary\":\"s\",\"tags\":[\"x\"],\"category\":\"other\"}"}}]}`), nil
	})
	w := newTestWorker(t, srv.URL, rt)

	stats, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce err = %v; a single content error must not abort", err)
	}
	if stats.Failed != 1 {
		t.Errorf("Failed = %d, want 1", stats.Failed)
	}
	if stats.Annotated != 9 {
		t.Errorf("Annotated = %d, want 9", stats.Annotated)
	}
	if stats.Scanned != 10 {
		t.Errorf("Scanned = %d, want 10 (whole corpus processed)", stats.Scanned)
	}
}
