package main

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMailAPIRedirectDoesNotDiscloseToken is the RA6X-039 reproduction for the
// worker's epistula-api client: an HTTPS endpoint redirecting to plaintext on the
// same hostname. Go's default policy considers that hop trusted and forwards
// Authorization.
func TestMailAPIRedirectDoesNotDiscloseToken(t *testing.T) {
	const secret = "ra6x039-worker-token-must-not-egress"
	const summary = "ra6x039-synthetic-mail-summary"

	var mu sync.Mutex
	var seen []string
	plaintext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization")+"|"+string(buf[:n]))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer plaintext.Close()

	// 308 preserves the method and replays the body, so this covers the
	// credential and the mail-derived payload in one shot.
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plaintext.URL+r.URL.Path, http.StatusPermanentRedirect)
	}))
	defer secure.Close()

	c, err := NewMailAPIClient(MailAPIConfig{
		BaseURL:           secure.URL,
		Token:             secret,
		RequestTimeoutSec: 5,
	})
	if err != nil {
		t.Fatalf("NewMailAPIClient: %v", err)
	}
	trustTestServer(c.client, secure)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := summary
	err = c.PutAnnotation(ctx, 42, AnnotationPut{Model: "test-model", Summary: &s})
	if err == nil {
		t.Fatal("the downgrade redirect was followed; it must be refused")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, got := range seen {
		if strings.Contains(got, secret) {
			t.Fatalf("bearer token reached the plaintext endpoint: %q", got)
		}
		if strings.Contains(got, summary) {
			t.Fatalf("annotation body was replayed to the plaintext endpoint: %q", got)
		}
	}
	if len(seen) != 0 {
		t.Fatalf("the plaintext endpoint was contacted %d time(s)", len(seen))
	}
}

// TestLMStudioRedirectDoesNotDiscloseMail covers the inference POST, whose body
// is the mail text itself. A redirect off the configured origin would hand a
// third party both the API token and the message.
func TestLMStudioRedirectDoesNotDiscloseMail(t *testing.T) {
	const secret = "ra6x039-lm-token-must-not-egress"
	const mailMarker = "ra6x039-synthetic-mail-body-marker"

	var mu sync.Mutex
	var seen []string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 65536)
		n, _ := r.Body.Read(buf)
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization")+"|"+string(buf[:n]))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, collector.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	c, err := NewLMStudioClient(LMStudioConfig{
		BaseURL:           origin.URL,
		APIToken:          secret,
		Model:             "test-model",
		MaxTokens:         64,
		RequestTimeoutSec: 5,
	})
	if err != nil {
		t.Fatalf("NewLMStudioClient: %v", err)
	}
	trustTestServer(c.client, origin)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err = c.Annotate(ctx, Prompt{System: "summarize", User: mailMarker})
	if err == nil {
		t.Fatal("the cross-origin redirect was followed; it must be refused")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, got := range seen {
		if strings.Contains(got, secret) {
			t.Fatalf("LM API token reached the other origin: %q", got)
		}
		if strings.Contains(got, mailMarker) {
			t.Fatalf("mail content reached the other origin: %q", got)
		}
	}
	if len(seen) != 0 {
		t.Fatalf("the other origin was contacted %d time(s)", len(seen))
	}
}

// trustTestServer points client at srv's certificate while leaving the
// redirect policy under test untouched.
func trustTestServer(client *http.Client, srv *httptest.Server) {
	client.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs,
			MinVersion: tls.VersionTLS12,
		},
	}
}
