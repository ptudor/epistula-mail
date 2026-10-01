package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// statusResponse builds an LM Studio-shaped failure response with the given
// headers, so tests can exercise Retry-After and correlation handling.
func statusResponse(status int, body string, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	if header.Get("Content-Type") == "" {
		header.Set("Content-Type", "application/json")
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}
}

// TestSystemicHTTPStatusesAreInfrastructureFailures is the RA6X-042 unit
// regression.
//
// LM Studio status failures were an ordinary formatted error, and
// isInfraFailure only ever recognised *url.Error plus 401/403 of the
// annotation-PUT error type. A 429 or a 503 therefore counted as a per-message
// content failure, RESET the consecutive counter, and let the worker run
// inference against the whole corpus during a total outage.
func TestSystemicHTTPStatusesAreInfrastructureFailures(t *testing.T) {
	for _, svc := range []Service{ServiceLMStudio, ServiceMailAPI} {
		for status, wantInfra := range map[int]bool{
			http.StatusUnauthorized:        true,
			http.StatusForbidden:           true,
			http.StatusRequestTimeout:      true,
			http.StatusTooManyRequests:     true,
			http.StatusInternalServerError: true,
			http.StatusBadGateway:          true,
			http.StatusServiceUnavailable:  true,
			http.StatusGatewayTimeout:      true,
			// Per-message: this request, not the service.
			http.StatusBadRequest:          false,
			http.StatusNotFound:            false,
			http.StatusUnprocessableEntity: false,
			http.StatusConflict:            false,
		} {
			err := error(&HTTPStatusError{Service: svc, StatusCode: status})
			if got := isInfraFailure(err); got != wantInfra {
				t.Errorf("%s %d: isInfraFailure = %v, want %v", svc, status, got, wantInfra)
			}
		}
	}
	// An auth rejection must not be retried: waiting changes nothing, and the
	// budget is better spent reaching the pass abort.
	if isRetryable(&HTTPStatusError{Service: ServiceLMStudio, StatusCode: http.StatusUnauthorized}) {
		t.Error("a 401 is retryable; it will answer the same way forever")
	}
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusBadGateway} {
		if !isRetryable(&HTTPStatusError{Service: ServiceLMStudio, StatusCode: status}) {
			t.Errorf("%d is not retryable; it may clear on its own", status)
		}
	}
}

// TestRetryAfterIsHonouredAndBounded pins the Retry-After half.
func TestRetryAfterIsHonouredAndBounded(t *testing.T) {
	backoff := 2 * time.Second
	cases := map[string]time.Duration{
		"":                    backoff,         // absent → configured backoff
		"5":                   5 * time.Second, // delta-seconds
		"0":                   backoff,         // non-positive is meaningless
		"-3":                  backoff,
		"garbage":             backoff,
		"9223372036854775807": maxRetryAfter,
		"9999":                maxRetryAfter, // clamped: a batch worker must not idle for hours
	}
	for header, want := range cases {
		h := http.Header{}
		if header != "" {
			h.Set("Retry-After", header)
		}
		e := statusErrorFrom(ServiceLMStudio, statusResponse(429, "", h), "")
		if got := retryDelay(e, backoff); got != want {
			t.Errorf("Retry-After %q → %s, want %s", header, got, want)
		}
	}
	// The HTTP-date form works too.
	h := http.Header{}
	h.Set("Retry-After", time.Now().Add(4*time.Second).UTC().Format(http.TimeFormat))
	e := statusErrorFrom(ServiceLMStudio, statusResponse(503, "", h), "")
	if d := retryDelay(e, backoff); d <= 0 || d > 5*time.Second {
		t.Errorf("HTTP-date Retry-After → %s, want roughly 4s", d)
	}
	// A date already in the past means "now".
	h.Set("Retry-After", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat))
	e = statusErrorFrom(ServiceLMStudio, statusResponse(503, "", h), "")
	if d := retryDelay(e, backoff); d != backoff {
		t.Errorf("past Retry-After → %s, want the configured backoff", d)
	}
}

// TestRunOnceAbortsOnSystemicLMStatuses is the RA6X-042 end-to-end regression:
// an LM Studio outage expressed as an HTTP status, not a transport error, must
// bound the number of requests instead of churning the corpus.
func TestRunOnceAbortsOnSystemicLMStatuses(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized,
		http.StatusTooManyRequests,
		http.StatusServiceUnavailable,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := exportServer(t, 50)
			defer srv.Close()

			var calls atomic.Int64
			rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				return statusResponse(status, `{"error":"upstream is down"}`, nil), nil
			})
			w := newTestWorker(t, srv.URL, rt)

			stats, err := w.RunOnce(context.Background())
			if err == nil {
				t.Fatal("a total LM Studio outage reported a successful pass")
			}
			if !strings.Contains(err.Error(), "consecutive infrastructure failures") {
				t.Fatalf("err = %v, want an infra-abort error", err)
			}
			if stats.Annotated != 0 {
				t.Errorf("Annotated = %d, want 0", stats.Annotated)
			}
			if stats.Scanned > consecutiveInfraFailureAbort {
				t.Errorf("Scanned = %d; the pass churned past the abort threshold of %d",
					stats.Scanned, consecutiveInfraFailureAbort)
			}
			// RetryAttempts is 0 in the fixture, so one request per message.
			if n := calls.Load(); n > int64(consecutiveInfraFailureAbort) {
				t.Errorf("%d inference requests during an outage, want at most %d",
					n, consecutiveInfraFailureAbort)
			}
		})
	}
}

// TestInfraFailuresBelowThresholdStillFailThePass pins the reporting half: a
// pass that leaves messages unannotated because an upstream was unavailable is
// not a successful pass, even when the outage was brief enough not to trip the
// breaker.
func TestInfraFailuresBelowThresholdStillFailThePass(t *testing.T) {
	srv := exportServer(t, 6)
	defer srv.Close()

	var call atomic.Int64
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		// One 503, then success — never enough consecutive failures to abort.
		if call.Add(1) == 2 {
			return statusResponse(http.StatusServiceUnavailable, "", nil), nil
		}
		return jsonResponse(http.StatusOK,
			`{"choices":[{"message":{"content":"{\"summary\":\"s\",\"tags\":[\"x\"],\"category\":\"other\"}"}}]}`), nil
	})
	w := newTestWorker(t, srv.URL, rt)

	stats, err := w.RunOnce(context.Background())
	if err == nil {
		t.Fatal("a pass that left a message unannotated by an outage reported success")
	}
	if !strings.Contains(err.Error(), "pass incomplete") {
		t.Fatalf("err = %v, want an incomplete-pass error", err)
	}
	if stats.InfraFailed != 1 {
		t.Errorf("InfraFailed = %d, want 1", stats.InfraFailed)
	}
	if stats.Annotated != 5 {
		t.Errorf("Annotated = %d, want the other 5", stats.Annotated)
	}
	if stats.Scanned != 6 {
		t.Errorf("Scanned = %d, want the whole 6-row corpus", stats.Scanned)
	}
}

// TestContentErrorsStillResetTheBreaker interleaves a recoverable content
// error with successes: neither must count as infrastructure, and the pass
// must complete cleanly.
func TestContentErrorsStillResetTheBreaker(t *testing.T) {
	srv := exportServer(t, 8)
	defer srv.Close()

	var call atomic.Int64
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch call.Add(1) {
		case 2, 5:
			// A 400 for this prompt, and a malformed answer: both per-message.
			return statusResponse(http.StatusBadRequest, "", nil), nil
		default:
			return jsonResponse(http.StatusOK,
				`{"choices":[{"message":{"content":"{\"summary\":\"s\",\"tags\":[\"x\"],\"category\":\"other\"}"}}]}`), nil
		}
	})
	w := newTestWorker(t, srv.URL, rt)

	stats, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce err = %v; per-message failures must not fail the pass", err)
	}
	if stats.InfraFailed != 0 {
		t.Errorf("InfraFailed = %d, want 0", stats.InfraFailed)
	}
	if stats.Failed != 2 || stats.Annotated != 6 {
		t.Errorf("Failed = %d, Annotated = %d; want 2 and 6", stats.Failed, stats.Annotated)
	}
}

// TestRetryIsCancellableDuringBackoff pins that a long Retry-After does not
// hold the worker past shutdown.
func TestRetryIsCancellableDuringBackoff(t *testing.T) {
	srv := exportServer(t, 3)
	defer srv.Close()

	h := http.Header{}
	h.Set("Retry-After", "30")
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return statusResponse(http.StatusTooManyRequests, "", h), nil
	})
	w := newTestWorker(t, srv.URL, rt)
	w.cfg.Worker.RetryAttempts = 3 // so a backoff is actually entered

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if _, err := w.RunOnce(ctx); err == nil {
		t.Fatal("a cancelled pass reported success")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancellation took %s; the Retry-After wait is not cancellable", elapsed)
	}
}

// TestAnnotationPutOutageAbortsThePass covers the epistula-api side of the same
// classification: a 429 or 503 on the annotation PUT is systemic too.
func TestAnnotationPutOutageAbortsThePass(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var puts atomic.Int64
			mux := http.NewServeMux()
			mux.HandleFunc("GET /v1/export", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				for i := 1; i <= 50; i++ {
					_, _ = io.WriteString(w, ndjsonRow(int64(i)))
				}
			})
			mux.HandleFunc("PUT /v1/messages/{id}/annotation", func(w http.ResponseWriter, r *http.Request) {
				puts.Add(1)
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"title": "Too Many Requests", "detail": "Slow down.",
				})
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return jsonResponse(http.StatusOK,
					`{"choices":[{"message":{"content":"{\"summary\":\"s\",\"tags\":[\"x\"],\"category\":\"other\"}"}}]}`), nil
			})
			w := newTestWorker(t, srv.URL, rt)
			w.cfg.Worker.DryRun = false // exercise the real PUT

			stats, err := w.RunOnce(context.Background())
			if err == nil {
				t.Fatal("an annotation-store outage reported a successful pass")
			}
			if !strings.Contains(err.Error(), "consecutive infrastructure failures") {
				t.Fatalf("err = %v, want an infra-abort error", err)
			}
			if stats.Annotated != 0 {
				t.Errorf("Annotated = %d, want 0", stats.Annotated)
			}
			// One PUT per message (RetryAttempts is 0), bounded by the breaker.
			if n := puts.Load(); n > int64(consecutiveInfraFailureAbort) {
				t.Errorf("%d annotation PUTs during an outage, want at most %d",
					n, consecutiveInfraFailureAbort)
			}
		})
	}
}

// TestLMErrorBodyNeverReachesTheLog is the RA6X-043 regression.
//
// On a non-200 the whole bounded response body — up to 4 MiB of arbitrary
// upstream text — was formatted into an error that RunOnce logged at WARN. An
// inference server or a proxy that echoes the rejected prompt therefore put
// full message content, and any credential it chose to quote back, into
// routine logs.
func TestLMErrorBodyNeverReachesTheLog(t *testing.T) {
	const (
		secretBody = "Subject: Wire transfer confirmation\r\n\r\n" +
			"Account 4111-1111-1111-1111 was debited by DISTINCTIVE-MAIL-MARKER."
		secretToken = "sk-DISTINCTIVE-CREDENTIAL-MARKER"
	)

	srv := exportServer(t, 3)
	defer srv.Close()

	h := http.Header{}
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Request-Id", "req-abc-123")
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		// The server echoes the prompt it rejected, and its own auth header.
		body, _ := json.Marshal(map[string]any{
			"error": map[string]any{
				"message":       "rejected prompt: " + secretBody,
				"authorization": "Bearer " + secretToken,
			},
		})
		return statusResponse(http.StatusBadRequest, string(body), h), nil
	})
	w := newTestWorker(t, srv.URL, rt)

	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	stats, runErr := w.RunOnce(context.Background())
	if stats.Failed == 0 {
		t.Fatal("the fixture did not produce a failure to log")
	}

	haystacks := map[string]string{"log output": logBuf.String()}
	if runErr != nil {
		haystacks["returned error"] = runErr.Error()
	}
	for where, s := range haystacks {
		for _, marker := range []string{"DISTINCTIVE-MAIL-MARKER", "DISTINCTIVE-CREDENTIAL-MARKER",
			"4111-1111-1111-1111", "rejected prompt"} {
			if strings.Contains(s, marker) {
				t.Errorf("%s contains %q from the upstream error body", where, marker)
			}
		}
	}

	// The diagnostic that remains must still be actionable.
	out := logBuf.String()
	for _, want := range []string{"lmstudio", "400", "req-abc-123", "application/json"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lost a safe diagnostic: %q not in the output", want)
		}
	}
}

// TestStatusErrorCarriesNoBody pins the error value itself, independent of any
// logger: nothing constructs one that quotes an LM Studio body.
func TestStatusErrorCarriesNoBody(t *testing.T) {
	h := http.Header{}
	h.Set("X-Correlation-Id", "corr-9\r\ninjected")
	e := statusErrorFrom(ServiceLMStudio, statusResponse(500, "SECRET-BODY", h), "")
	msg := e.Error()
	if strings.Contains(msg, "SECRET-BODY") {
		t.Errorf("Error() = %q; it quotes the upstream body", msg)
	}
	if !strings.Contains(msg, "500") || !strings.Contains(msg, "lmstudio") {
		t.Errorf("Error() = %q; it lost the status or the service", msg)
	}
	// A correlation id is upstream input: it must not break a log line.
	if strings.ContainsAny(msg, "\r\n") {
		t.Errorf("Error() = %q; a header value carried a newline into it", msg)
	}

	// epistula-api's own RFC 7807 detail is kept — it is this project's text and
	// carries no message content.
	body := `{"title":"Forbidden","detail":"Token is not scoped to mailbox 'alice'."}`
	me := statusErrorFrom(ServiceMailAPI, statusResponse(403, body, nil), problemDetail([]byte(body)))
	if !strings.Contains(me.Error(), "not scoped to mailbox") {
		t.Errorf("epistula-api error lost its problem detail: %q", me.Error())
	}
	var target *HTTPStatusError
	if !errors.As(error(me), &target) || target.StatusCode != 403 {
		t.Errorf("epistula-api error is not classifiable: %#v", me)
	}
}
