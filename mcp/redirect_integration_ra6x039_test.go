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

// TestRedirectDoesNotDiscloseTokenOverPlaintext is the RA6X-039 end-to-end
// reproduction: an HTTPS endpoint redirecting to a plaintext endpoint on the
// same hostname. Go's default CheckRedirect considers that same-host hop
// trusted and forwards Authorization, handing the bearer token to a listener
// with no transport security.
func TestRedirectDoesNotDiscloseTokenOverPlaintext(t *testing.T) {
	const secret = "ra6x039-dummy-token-must-not-egress"

	var mu sync.Mutex
	var plaintextAuth []string
	plaintext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		plaintextAuth = append(plaintextAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer plaintext.Close()

	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plaintext.URL+r.URL.Path, http.StatusFound)
	}))
	defer secure.Close()

	c := newTestClient(t, secure, secret)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, aerr := c.getJSON(ctx, "/mailboxes", nil)
	if aerr == nil {
		t.Fatal("the downgrade redirect was followed; it must be refused")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, got := range plaintextAuth {
		if strings.Contains(got, secret) {
			t.Fatalf("bearer token reached the plaintext endpoint: %q", got)
		}
	}
	if len(plaintextAuth) != 0 {
		t.Fatalf("the plaintext endpoint was contacted %d time(s)", len(plaintextAuth))
	}
}

// TestRedirectDoesNotReplayAnnotationBody pins the 307/308 half: a redirect
// that preserves the method also replays the request body, so an annotation
// PUT would resend its payload to another origin even where Go strips the
// credential. Stripping Authorization alone is not a fix.
func TestRedirectDoesNotReplayAnnotationBody(t *testing.T) {
	const marker = "ra6x039-synthetic-mail-summary"

	var mu sync.Mutex
	var receivedBodies []string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		mu.Lock()
		receivedBodies = append(receivedBodies, string(buf[:n]))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer collector.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, collector.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	c := newTestClient(t, origin, "ra6x039-write-token")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	aerr := c.put(ctx, "/messages/1/annotation", map[string]any{
		"model":   "test-model",
		"summary": marker,
	})
	if aerr == nil {
		t.Fatal("the cross-origin 307 was followed; it must be refused")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, body := range receivedBodies {
		if strings.Contains(body, marker) {
			t.Fatalf("annotation body was replayed to another origin: %q", body)
		}
	}
	if len(receivedBodies) != 0 {
		t.Fatalf("the other origin was contacted %d time(s)", len(receivedBodies))
	}
}

// TestSameOriginRedirectIsFollowed pins that the policy did not break the one
// redirect real deployments produce: a reverse proxy canonicalising an
// approved base path on the same origin.
func TestSameOriginRedirectIsFollowed(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, srv.URL+r.URL.Path+"/", http.StatusMovedPermanently)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mailboxes":[]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "ra6x039-read-token")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, aerr := c.getJSON(ctx, "/mailboxes", nil)
	if aerr != nil {
		t.Fatalf("same-origin redirect must still be followed: %+v", aerr)
	}
	if out == nil {
		t.Fatal("expected a decoded body after the same-origin redirect")
	}
}

// newTestClient builds a client pointed at srv, trusting its certificate when
// it is a TLS server, with the redirect policy under test left in place.
func newTestClient(t *testing.T, srv *httptest.Server, token string) *client {
	t.Helper()
	c := newClientWithToken(config{
		BaseURL:          srv.URL,
		RequestTimeout:   5 * time.Second,
		MaxResponseBytes: defaultMaxResponseBytes,
	}, token)
	if srv.TLS != nil {
		c.http.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs,
				MinVersion: tls.VersionTLS12,
			},
		}
	}
	return c
}
