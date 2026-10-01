package main

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLMStudioIntegration exercises the real Annotate path (BuildPrompt ->
// LM Studio chat/completions with the strict json_schema -> decodeAnnotation ->
// normalizeAnnotation) against a live LM Studio server. It is skipped unless
// LMSTUDIO_IT_URL is set, so the normal unit suite stays hermetic:
//
//	LMSTUDIO_IT_URL=http://127.0.0.1:1234/v1 \
//	LMSTUDIO_IT_MODEL=qwen/qwen3.6-35b-a3b \
//	go test -run TestLMStudioIntegration -v
//
// This is the end-to-end check that proves reasoning models (content empty,
// JSON in reasoning_content) are handled by the real client, not just by mocks.
func TestLMStudioIntegration(t *testing.T) {
	base := os.Getenv("LMSTUDIO_IT_URL")
	if base == "" {
		t.Skip("set LMSTUDIO_IT_URL to run the live LM Studio integration test")
	}
	cfg := DefaultConfig()
	cfg.LMStudio.BaseURL = base
	if m := os.Getenv("LMSTUDIO_IT_MODEL"); m != "" {
		cfg.LMStudio.Model = m
		cfg.Worker.AnnotationModel = "lmstudio:" + m
	}
	cfg.LMStudio.RequestTimeoutSec = 120

	client, err := NewLMStudioClient(cfg.LMStudio)
	if err != nil {
		t.Fatal(err)
	}

	sent := time.Date(2026, 6, 11, 18, 4, 20, 0, time.UTC)
	subject := "Your Acme Shop order #114-2938472 has shipped"
	from := "ship-confirm@acmeshop.example"
	body := "Your package with 1 item is on the way. Order #114-2938472. " +
		"Order total $21.63 charged to Visa ending 4242. Track it in Your Orders."
	msg := Message{
		ID:           90001,
		Mailbox:      "alice@mx.example.invalid",
		Folder:       "INBOX",
		InternalDate: sent,
		SentDate:     &sent,
		Subject:      &subject,
		From:         &from,
		To:           []string{"alice@mx.example.invalid"},
		Size:         18244,
		TextBody:     &body,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	ann, resp, err := client.Annotate(ctx, BuildPrompt(cfg, msg))
	if err != nil {
		t.Fatalf("Annotate() = %v", err)
	}
	if ann.Summary == "" || ann.Category == "" {
		t.Fatalf("empty annotation from live model: %#v", ann)
	}

	norm := normalizeAnnotation(cfg, ann)
	if !containsLabel(cfg.Worker.Categories, *norm.Category) {
		t.Fatalf("normalized category %q not in configured set", *norm.Category)
	}
	t.Logf("category=%s tags=%v", *norm.Category, norm.Tags)
	t.Logf("summary=%q", *norm.Summary)
	if resp.Usage != nil {
		t.Logf("tokens in=%d out=%d", resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	}
}
