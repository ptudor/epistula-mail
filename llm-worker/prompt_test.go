package main

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestBuildPromptTruncatesBody(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MailAPI.Token = "x"
	cfg.Worker.MaxBodyChars = 5
	body := "abcdef🙂"
	subject := "hello"
	msg := Message{
		ID:           42,
		Mailbox:      "alice",
		Folder:       "INBOX",
		InternalDate: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
		Subject:      &subject,
		TextBody:     &body,
	}
	p := BuildPrompt(cfg, msg)
	if !strings.Contains(p.User, "[truncated]") {
		t.Fatalf("prompt was not truncated: %q", p.User)
	}
	if !utf8.ValidString(p.User) {
		t.Fatal("prompt is not valid UTF-8")
	}
}

func TestNormalizeAnnotation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MailAPI.Token = "x"
	cfg.Worker.MaxTags = 2
	cfg.Worker.AllowedTags = []string{"tax", "banking"}
	put := normalizeAnnotation(cfg, LLMAnnotation{
		Summary:  "  Pay this invoice.  ",
		Category: "Finance",
		Tags:     []string{"Tax", "banking", "ignored"},
	})
	if put.Category == nil || *put.Category != "finance" {
		t.Fatalf("category = %v", put.Category)
	}
	if got := strings.Join(put.Tags, ","); got != "tax,banking" {
		t.Fatalf("tags = %q", got)
	}
	if put.Summary == nil || *put.Summary != "Pay this invoice." {
		t.Fatalf("summary = %v", put.Summary)
	}
}
