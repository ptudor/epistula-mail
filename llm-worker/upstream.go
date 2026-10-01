package main

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Service names an upstream this worker depends on. It rides on every status
// error so a classification decision never has to guess which side failed.
type Service string

const (
	ServiceLMStudio Service = "lmstudio"
	ServiceMailAPI  Service = "epistula-api"
)

// HTTPStatusError is the structured form of a non-success HTTP response from
// either upstream (RA6X-042).
//
// LM Studio status failures used to be an ordinary formatted error, so
// isInfraFailure — which only ever examined *url.Error and two codes of this
// type — could not see them. A 401, a 429 or a 503 from the inference server
// therefore looked exactly like "this one message could not be annotated": the
// worker ran inference against the WHOLE CORPUS during a total outage, reset
// its consecutive-failure counter on every one of them, and finished the pass
// reporting no error while having annotated nothing.
//
// Body is deliberately NOT part of Error() (RA6X-043). See safeDetail.
type HTTPStatusError struct {
	Service    Service
	StatusCode int
	// RetryAfter is the parsed Retry-After header, zero when absent.
	RetryAfter time.Duration
	// CorrelationID is whatever request identifier the upstream returned, so a
	// failure here can be matched against that server's own logs without
	// copying its response body into ours.
	CorrelationID string
	// ContentType is the response's declared media type, which distinguishes
	// "the server sent a structured error" from "a proxy sent an HTML page"
	// without quoting either.
	ContentType string
	// Detail is a short, safe description. For epistula-api it is the RFC 7807
	// problem detail this project itself produced; for LM Studio it is always
	// empty. Never an arbitrary upstream body.
	Detail string
}

func (e *HTTPStatusError) Error() string {
	var b strings.Builder
	b.WriteString(string(e.Service))
	b.WriteString(" status ")
	b.WriteString(strconv.Itoa(e.StatusCode))
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	if e.ContentType != "" {
		b.WriteString(" (content-type ")
		b.WriteString(e.ContentType)
		b.WriteString(")")
	}
	if e.CorrelationID != "" {
		b.WriteString(" [request ")
		b.WriteString(e.CorrelationID)
		b.WriteString("]")
	}
	if e.RetryAfter > 0 {
		b.WriteString(" retry-after ")
		b.WriteString(e.RetryAfter.String())
	}
	return b.String()
}

// statusErrorFrom builds the structured error for a response, reading only
// headers — never the body (RA6X-043).
//
// The caller decides whether a Detail is safe to attach. It is safe for
// epistula-api, whose non-2xx bodies are RFC 7807 documents this project writes
// itself and which carry no message content. It is NOT safe for LM Studio: an
// inference server or a proxy in front of it commonly echoes the rejected
// prompt, and the prompt is the mail. Up to 4 MiB of arbitrary upstream text
// used to be formatted into an error that RunOnce logged at WARN, which put
// whole emails — and any credential the server chose to quote back — into
// routine logs, against this project's content-free logging policy.
//
// If payload-level diagnostics are ever needed for LM Studio, they belong in a
// separate, explicitly enabled facility with its own retention and access
// rules; a size cap on a log line is not redaction. This function does not
// provide one, and the status, media type and correlation identity below are
// what a normal deployment gets.
func statusErrorFrom(svc Service, resp *http.Response, detail string) *HTTPStatusError {
	return &HTTPStatusError{
		Service:       svc,
		StatusCode:    resp.StatusCode,
		RetryAfter:    parseRetryAfter(resp.Header.Get("Retry-After")),
		CorrelationID: correlationID(resp.Header),
		ContentType:   mediaTypeOf(resp.Header.Get("Content-Type")),
		Detail:        detail,
	}
}

// correlationHeaders are the request-identifier headers this worker will quote,
// in preference order. They are server-generated identifiers, not content.
var correlationHeaders = []string{"X-Request-Id", "X-Correlation-Id", "Request-Id", "X-Amzn-Trace-Id"}

func correlationID(h http.Header) string {
	for _, name := range correlationHeaders {
		if v := strings.TrimSpace(h.Get(name)); v != "" {
			return boundedToken(v)
		}
	}
	return ""
}

// boundedToken keeps an identifier printable and short. A header is upstream
// input, so it is treated as such even though it is not message content.
func boundedToken(s string) string {
	const maxTokenBytes = 96
	if len(s) > maxTokenBytes {
		s = s[:maxTokenBytes]
	}
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			b.WriteByte('?')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// mediaTypeOf strips parameters from a Content-Type, leaving only the type.
func mediaTypeOf(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return boundedToken(strings.TrimSpace(strings.ToLower(ct)))
}

// maxRetryAfter caps how long a Retry-After may pause this worker. A server
// under load is free to ask for an hour; a batch annotator holding a epistula-api
// export stream open that long is not a useful way to wait.
const maxRetryAfter = 60 * time.Second

// parseRetryAfter reads both RFC 9110 forms — delta-seconds and an HTTP-date —
// and clamps the result. An unparseable or past value is zero, meaning "use the
// configured backoff".
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		// Clamp seconds before converting: an int-sized delta can overflow
		// time.Duration during multiplication and turn a delay negative.
		return time.Duration(min(secs, int(maxRetryAfter/time.Second))) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d <= 0 {
			return 0
		}
		return min(d, maxRetryAfter)
	}
	return 0
}

// isInfraFailure reports whether err is an infrastructure failure that will
// recur for every message this pass (RA6X-042).
//
// Transport errors (no HTTP response at all: connection refused, timeout, DNS)
// and systemic HTTP statuses count. The systemic set is authentication
// (401/403 — a revoked or wrong-scope token rejects every request),
// throttling (429 — the whole worker is being rate-limited) and service
// failure (408 and every 5xx — the server, not this message, is the problem).
//
// Everything else stays a per-message content error, including epistula-api's 404
// for a message that was expunged between the export row and the annotation
// PUT, and LM Studio's 400 for one prompt it would not accept. Those reset the
// consecutive counter and the pass continues, which is the behaviour R-037
// established and this change preserves.
func isInfraFailure(err error) bool {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		switch statusErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden,
			http.StatusRequestTimeout, http.StatusTooManyRequests:
			return true
		}
		return statusErr.StatusCode >= 500
	}
	return false
}

// isRetryable reports whether a failure is worth another bounded attempt
// against the same upstream. Throttling and service failures may clear on
// their own; an authentication rejection will not, so retrying it only burns
// the budget and delays the pass abort that is the correct response.
func isRetryable(err error) bool {
	// The same prompt overflows the same context every time; the caller
	// shrinks the body instead (ContextOverflowError).
	var overflow *ContextOverflowError
	if errors.As(err, &overflow) {
		return false
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		switch statusErr.StatusCode {
		case http.StatusRequestTimeout, http.StatusTooManyRequests:
			return true
		}
		return statusErr.StatusCode >= 500
	}
	// A per-message content failure (a malformed model answer, a truncated
	// response) is retryable too: that is what RetryAttempts has always been
	// for.
	return true
}

// retryDelay is how long to wait before the next attempt: the server's own
// Retry-After when it sent one, otherwise the configured backoff.
func retryDelay(err error, backoff time.Duration) time.Duration {
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) && statusErr.RetryAfter > 0 {
		return statusErr.RetryAfter
	}
	return backoff
}
