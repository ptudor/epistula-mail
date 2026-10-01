package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// bodyString reads and returns the response body.
func bodyString(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// --- R-065: annotation PUT racing an expunge → 404, not 500 ---------------

// TestAnnotationUpsertMissingMessageIsFKViolation documents the exact error
// class handleAnnotationPut now maps to 404 (R-065): the message_annotations FK
// only fails when the message row is gone, which raises SQLSTATE 23503. The
// handler branch is inherently race-only (resolve passes, then epistula-imap
// expunges, then the upsert FK-fails), so this asserts the trigger directly.
func TestAnnotationUpsertMissingMessageIsFKViolation(t *testing.T) {
	f := newAPIFixture(t)
	_, err := f.pool.Exec(context.Background(),
		`INSERT INTO message_annotations (message_id, model, tags) VALUES ($1, 'm', '{}')`,
		int64(999_999_999))
	var pgErr *pgconn.PgError
	if err == nil {
		t.Fatal("upsert for an absent message succeeded; expected a FK violation")
	}
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("upsert error = %v, want SQLSTATE 23503", err)
	}
}

// TestAnnotationPutGoneMessageReturns404 covers the client-visible contract: a
// PUT for a message that no longer exists returns 404 problem+json, never 500.
func TestAnnotationPutGoneMessageReturns404(t *testing.T) {
	f := newAPIFixture(t)
	id := f.aliceMsgIDs[0]
	if _, err := f.pool.Exec(context.Background(), `DELETE FROM messages WHERE id = $1`, id); err != nil {
		t.Fatalf("delete message: %v", err)
	}
	resp := f.do(http.MethodPut, fmt.Sprintf("/v1/messages/%d/annotation", id),
		f.classifierToken, strings.NewReader(`{"model":"m","tags":["x"]}`))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("PUT for a gone message = %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want problem+json", ct)
	}
	resp.Body.Close()
}

// --- R-066: annotations requested but sidecar absent → 503 ----------------

func TestAnnotationRequestedButSidecarAbsent503(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.annotationsAvailable = false

	// List with fields=annotation → 503.
	resp := f.do(http.MethodGet, "/v1/mailboxes/alice/folders/INBOX/messages?fields=annotation",
		f.classifierToken, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("list fields=annotation = %d, want 503", resp.StatusCode)
	}
	resp.Body.Close()

	// Same list WITHOUT the param → 200 (annotations simply not requested).
	resp = f.do(http.MethodGet, "/v1/mailboxes/alice/folders/INBOX/messages",
		f.classifierToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("list without fields=annotation = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// Single message always includes annotations → 503 when the sidecar is absent.
	resp = f.do(http.MethodGet, fmt.Sprintf("/v1/messages/%d", f.aliceMsgIDs[0]),
		f.classifierToken, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET message = %d, want 503 when sidecar absent", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- R-067: priority 0 distinguishable from registry-absent ---------------

func TestPriorityZeroWireDistinctFromUnranked(t *testing.T) {
	f := newAPIFixture(t)
	id := f.aliceMsgIDs[0]
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO message_annotations (message_id, model, tags) VALUES ($1, 'unregistered', '{a}')`,
		id); err != nil {
		t.Fatalf("annotate: %v", err)
	}

	// Registry present: an unregistered model ranks at priority 0, which must
	// appear on the wire as "priority":0 (not omitted).
	f.srv.modelPriorityAvailable = true
	body := bodyString(t, f.do(http.MethodGet, fmt.Sprintf("/v1/messages/%d", id), f.classifierToken, nil))
	if !strings.Contains(body, `"priority":0`) {
		t.Errorf("ranked body missing \"priority\":0 for an unregistered model: %s", body)
	}

	// Registry absent: annotations are unranked and omit priority entirely.
	f.srv.modelPriorityAvailable = false
	body = bodyString(t, f.do(http.MethodGet, fmt.Sprintf("/v1/messages/%d", id), f.classifierToken, nil))
	if strings.Contains(body, `"priority"`) {
		t.Errorf("unranked body should omit priority, got: %s", body)
	}
}

// --- R-068: 405 problem+json + Allow; catch-all through auth --------------

func TestMethodMismatchIs405Problem(t *testing.T) {
	f := newAPIFixture(t)
	resp := f.do(http.MethodPost, "/v1/search", f.classifierToken, strings.NewReader("{}"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/search = %d, want 405", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want problem+json", ct)
	}
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("Allow = %q, want it to include GET", allow)
	}
}

func TestUnknownPathRequiresAuth(t *testing.T) {
	f := newAPIFixture(t)
	// No token → 401 for an unknown path too, closing the 401-vs-404
	// path-enumeration oracle (R-068).
	resp := f.do(http.MethodGet, "/v1/definitely-not-a-route", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated unknown path = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Authenticated → 404 problem+json.
	resp = f.do(http.MethodGet, "/v1/definitely-not-a-route", f.classifierToken, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("authenticated unknown path = %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want problem+json", ct)
	}
	resp.Body.Close()
}

// --- R-069: per-IP auth-failure map is globally pruned --------------------

func TestAuthFailMapGlobalSweep(t *testing.T) {
	f := newAPIFixture(t)
	a := f.srv.auth
	window := f.srv.cfg.AuthFailWindowDuration()

	// Plant many stale single-hit IPs that never return (the leak).
	old := time.Now().Add(-2 * window)
	a.mu.Lock()
	for i := 0; i < 10_000; i++ {
		a.fails[fmt.Sprintf("10.%d.%d.%d", i/65536, (i/256)%256, i%256)] = []time.Time{old}
	}
	a.mu.Unlock()

	// Drive failsSweepInterval records for one active IP to trigger the sweep.
	for i := 0; i < failsSweepInterval; i++ {
		a.recordFailure("1.2.3.4")
	}

	a.mu.Lock()
	n := len(a.fails)
	a.mu.Unlock()
	if n > 2 {
		t.Fatalf("fails map holds %d entries after the sweep, want ~1 (stale IPs not pruned)", n)
	}
}
