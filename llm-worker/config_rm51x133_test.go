package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestExportFiltersAndURLValidation(t *testing.T) {
	clearEnv(t)
	for name, mutate := range map[string]func(*Config){
		"folder without mailbox": func(c *Config) { c.MailAPI.Folder = "INBOX" },
		"bad since":              func(c *Config) { c.MailAPI.Since = "yesterday" },
		"bad before":             func(c *Config) { c.MailAPI.Before = "2026-13-01" },
		"mail URL userinfo":      func(c *Config) { c.MailAPI.BaseURL = "https://user:pw@mail.example.invalid" },
		"model URL userinfo":     func(c *Config) { c.LMStudio.BaseURL = "https://user:pw@lm.example.invalid/v1" },
	} {
		t.Run(name, func(t *testing.T) {
			c := validatableDefault()
			c.MailAPI.Token = "mapi_test"
			mutate(c)
			if err := c.Validate(); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
	c := validatableDefault()
	c.MailAPI.Token, c.MailAPI.Since, c.MailAPI.Before = "mapi_test", "2026-01-01", "2026-10-13T12:34:56Z"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestTrimmedTokenAuthorization(t *testing.T) {
	clearEnv(t)
	t.Setenv("MAIL_LLM_MAIL_API_TOKEN", " mapi_test \n")
	t.Setenv("MAIL_LLM_LMSTUDIO_TOKEN", " model-token ")
	c, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if c.MailAPI.Token != "mapi_test" || c.LMStudio.APIToken != "model-token" {
		t.Fatal("tokens were not trimmed")
	}
	client, err := NewMailAPIClient(c.MailAPI)
	if err != nil {
		t.Fatal(err)
	}
	client.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Authorization"); got != "Bearer mapi_test" {
			t.Fatalf("Authorization=%q", got)
		}
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	if err := client.PutAnnotation(context.Background(), 1, AnnotationPut{Model: "test"}); err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"MAIL_LLM_MAIL_API_TOKEN", "MAIL_LLM_LMSTUDIO_TOKEN"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(env, "secret\rcontrol")
			if _, err := LoadConfig(""); err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("control-token error: %v", err)
			}
		})
	}
}
