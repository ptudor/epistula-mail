package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// overflowBody is what llama.cpp behind LM Studio answers for a prompt longer
// than the context window. The marker stands for echoed mail text, which must
// never reach an error or a log.
const overflowBody = `{"error":"The number of tokens to keep from the initial prompt is greater than the context length (n_keep: 18893 >= n_ctx: 16384). SECRET-MAIL-TEXT"}`

func TestBodyBudgets(t *testing.T) {
	for start, want := range map[int]string{
		24000: "[24000 12000 6000 3000 2000]",
		6000:  "[6000 3000 2000]",
		2000:  "[2000]",
		1500:  "[1500]",
	} {
		if got := fmt.Sprint(bodyBudgets(start)); got != want {
			t.Errorf("bodyBudgets(%d) = %s, want %s", start, got, want)
		}
	}
}

func TestContextOverflowIsRecognisedWithoutItsBody(t *testing.T) {
	status := http.StatusBadRequest
	body := overflowBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	defer srv.Close()
	cfg := DefaultConfig().LMStudio
	cfg.BaseURL = srv.URL + "/v1"
	client, err := NewLMStudioClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = client.Annotate(context.Background(), Prompt{System: "s", User: "u"})
	var overflow *ContextOverflowError
	if !errors.As(err, &overflow) {
		t.Fatalf("err = %v; want a ContextOverflowError", err)
	}
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "n_keep") {
		t.Fatalf("the error quotes the upstream body: %v", err)
	}
	if isRetryable(err) || isInfraFailure(err) {
		t.Fatal("a context overflow is retried identically or counted as an outage")
	}

	// Any other 400 is not an overflow.
	body = `{"error":"invalid json schema"}`
	_, _, err = client.Annotate(context.Background(), Prompt{System: "s", User: "u"})
	if errors.As(err, &overflow) {
		t.Fatalf("an unrelated 400 was taken for an overflow: %v", err)
	}
}

// TestProcessMessageShrinksBodyOnOverflow: a message too long for the context
// window is annotated and classified with less of its body instead of failing
// on every pass.
func TestProcessMessageShrinksBodyOnOverflow(t *testing.T) {
	s := newFakeStore()
	api := httptest.NewServer(s.mailAPI(t))
	defer api.Close()
	var mu sync.Mutex
	var userLens []int
	lm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		n := len(req.Messages[1].Content)
		mu.Lock()
		userLens = append(userLens, n)
		mu.Unlock()
		if n > 5000 {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, overflowBody)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"content": `{"summary":"s","tags":[],"category":"other",` +
				`"archive_category":"travel","archive_confidence":0.75}`},
			"finish_reason": "stop",
		}}})
	}))
	defer lm.Close()

	cfg := DefaultConfig()
	cfg.MailAPI.BaseURL = api.URL
	cfg.MailAPI.Token = "mapi_test"
	cfg.LMStudio.BaseURL = lm.URL + "/v1"
	cfg.Worker.AnnotationModel = "lmstudio:test"
	cfg.Worker.RetryAttempts = 2
	cfg.Classify.Enabled = true
	w, err := NewWorker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("https://tracking.example.test/x?id=1 ", 800) // ~30K characters
	msg := Message{ID: 1, Mailbox: "jdoe", Folder: "INBOX", Flags: []string{}, TextBody: &long}
	before := metricContextOverflows.Load()
	classified, err := w.processMessage(context.Background(), msg, testVocab)
	if err != nil || !classified {
		t.Fatalf("processMessage = %v, %v; want success after shrinking", classified, err)
	}
	// 24000, 12000 and 6000 characters overflow; 3000 fits. Each size is sent
	// once: an overflow is not retried as it stands.
	if len(userLens) != 4 {
		t.Fatalf("sent %d requests (user lengths %v); want 4", len(userLens), userLens)
	}
	for i := 1; i < len(userLens); i++ {
		if userLens[i] >= userLens[i-1] {
			t.Fatalf("request %d was not smaller: %v", i, userLens)
		}
	}
	if got := metricContextOverflows.Load() - before; got != 3 {
		t.Fatalf("context overflow metric rose by %d, want 3", got)
	}
	if !s.annotated[1] || s.classified[1] != "travel" {
		t.Fatalf("annotated=%v classified=%q", s.annotated[1], s.classified[1])
	}
}

func TestReasoningEffortIsSentOnlyWhenSet(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		got = append(got, req)
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"message":       map[string]any{"content": `{"summary":"s","tags":[],"category":"other"}`},
			"finish_reason": "stop",
		}}})
	}))
	defer srv.Close()
	for _, effort := range []string{"", "none"} {
		cfg := DefaultConfig().LMStudio
		cfg.BaseURL = srv.URL + "/v1"
		cfg.ReasoningEffort = effort
		client, err := NewLMStudioClient(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := client.Annotate(context.Background(), Prompt{System: "s", User: "u"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, present := got[0]["reasoning_effort"]; present {
		t.Fatal("reasoning_effort was sent although not configured")
	}
	if got[1]["reasoning_effort"] != "none" {
		t.Fatalf("reasoning_effort = %v, want none", got[1]["reasoning_effort"])
	}

	c := DefaultConfig()
	c.MailAPI.Token = "x"
	c.Worker.AnnotationModel = "m"
	c.LMStudio.ReasoningEffort = "off"
	if err := c.Validate(); err == nil {
		t.Fatal("an unknown reasoning_effort was accepted")
	}
}

// TestCombinedAnswerInReasoningChannel: Qwen3 under a strict schema puts its
// whole answer in reasoning_content and leaves content empty; the combined
// annotate-and-classify answer is accepted from there, as the annotation-only
// one always was.
func TestCombinedAnswerInReasoningChannel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"content": "", "reasoning_content": `{"summary":"a statement","tags":["bank"],` +
				`"category":"finance","archive_category":"finance/banking","archive_confidence":0.9}`},
			"finish_reason": "stop",
		}}})
	}))
	defer srv.Close()
	cfg := DefaultConfig().LMStudio
	cfg.BaseURL = srv.URL + "/v1"
	client, err := NewLMStudioClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ann, _, err := client.AnnotateAndClassify(context.Background(), Prompt{System: "s", User: "u"}, testVocab)
	if err != nil || ann.ArchiveCategory != "finance/banking" {
		t.Fatalf("answer in reasoning_content: %+v, %v", ann, err)
	}
}
