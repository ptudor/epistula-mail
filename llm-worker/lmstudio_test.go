package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestLMStudioAnnotate(t *testing.T) {
	var sawSchema bool
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		sawSchema = req.ResponseFormat != nil
		return jsonResponse(http.StatusOK, `{
		  "choices":[{"message":{"content":"{\"summary\":\"Hi\",\"tags\":[\"personal\"],\"category\":\"personal\"}"}}],
		  "usage":{"prompt_tokens":11,"completion_tokens":7}
		}`), nil
	})

	cfg := DefaultConfig().LMStudio
	cfg.BaseURL = "http://lmstudio.test/v1"
	client, err := NewLMStudioClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	client.client = &http.Client{Transport: rt}
	ann, resp, err := client.Annotate(context.Background(), Prompt{System: "s", User: "u"})
	if err != nil {
		t.Fatalf("Annotate() = %v", err)
	}
	if !sawSchema {
		t.Fatal("request did not include response_format")
	}
	if ann.Summary != "Hi" || ann.Category != "personal" || len(ann.Tags) != 1 {
		t.Fatalf("annotation = %#v", ann)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 7 {
		t.Fatalf("usage = %#v", resp.Usage)
	}
}

func TestLMStudioAnnotateReasoningModel(t *testing.T) {
	// Reasoning models (Qwen3.x) leave content empty and put the
	// schema-constrained JSON in reasoning_content. The worker must recover it.
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{
		  "choices":[{"message":{
		    "content":"",
		    "reasoning_content":"{\"summary\":\"Order shipped\",\"tags\":[\"receipt\",\"finance\"],\"category\":\"receipt\"}\n"
		  }}],
		  "usage":{"prompt_tokens":203,"completion_tokens":33}
		}`), nil
	})

	cfg := DefaultConfig().LMStudio
	cfg.BaseURL = "http://lmstudio.test/v1"
	client, err := NewLMStudioClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	client.client = &http.Client{Transport: rt}
	ann, _, err := client.Annotate(context.Background(), Prompt{System: "s", User: "u"})
	if err != nil {
		t.Fatalf("Annotate() = %v", err)
	}
	if ann.Summary != "Order shipped" || ann.Category != "receipt" || len(ann.Tags) != 2 {
		t.Fatalf("annotation = %#v", ann)
	}
}

func TestDecodeAnnotation(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		reasoning string
		want      string // expected category, "" means expect error
	}{
		{"content plain", `{"summary":"a","tags":["x"],"category":"personal"}`, "", "personal"},
		{"content preferred over reasoning",
			`{"summary":"a","tags":["x"],"category":"finance"}`,
			`{"summary":"b","tags":["y"],"category":"spam"}`, "finance"},
		{"reasoning fallback when content empty", "",
			`{"summary":"b","tags":["y"],"category":"spam"}`, "spam"},
		{"inline think block in content",
			"<think>this looks like a receipt</think>{\"summary\":\"a\",\"tags\":[\"x\"],\"category\":\"receipt\"}",
			"", "receipt"},
		{"markdown fenced json", "```json\n{\"summary\":\"a\",\"tags\":[\"x\"],\"category\":\"travel\"}\n```", "", "travel"},
		{"both empty", "", "", ""},
		{"no json anywhere", "I could not classify this.", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ann, err := decodeAnnotation(tc.content, tc.reasoning)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("expected error, got %#v", ann)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeAnnotation() = %v", err)
			}
			if ann.Category != tc.want {
				t.Fatalf("category = %q, want %q", ann.Category, tc.want)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}
}
