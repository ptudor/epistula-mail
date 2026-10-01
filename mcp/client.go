// HTTP client for epistula-api's /v1 surface.
//
// This connector never touches a database and never calls an LLM. Every tool
// call becomes exactly one request to {base_url}/v1/..., authenticated with the
// epistula-api bearer token in the Authorization header. There are no client-side
// retries — a 429/503 passes through honestly and the model (or the human)
// retries. There are no streaming endpoints in v1 (the MCP does not wrap
// /v1/export), so a single http.Client with a total-deadline timeout suffices.
//
// Secrets hygiene: the token rides only in the Authorization header — never in a
// URL, never in a log, never in an error. Upstream error bodies are bounded to a
// small snippet so a misconfigured server cannot spill internals into a tool
// error the model reads.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// maxErrBody bounds the snippet retained from a non-2xx body that is not
// decodable problem+json, so a misconfigured upstream can't spill internals.
const maxErrBody = 300

type client struct {
	http  *http.Client
	base  string // config.BaseURL, e.g. http://127.0.0.1:8784 (no trailing slash)
	token string // epistula-api bearer token (secret — header only)
	ua    string
	// maxResponseBytes bounds a buffered response body (RO5X-016).
	maxResponseBytes int
}

// newClient builds the READ client, authenticated with cfg.Token.
func newClient(cfg config) *client { return newClientWithToken(cfg, cfg.Token) }

// newClientWithToken builds a client bound to one specific bearer token.
//
// A client carries exactly one credential for its whole life, deliberately:
// the annotate tool gets its own *client holding its own token rather than a
// per-request token argument on the shared one. A per-call parameter would put
// the write credential within reach of every read path and make "which token
// did this request use" a question you answer by reading call sites. Separate
// clients make it a question you answer by reading types.
func newClientWithToken(cfg config, token string) *client {
	return &client{
		http: &http.Client{
			Timeout: cfg.RequestTimeout,
			// A redirect must not carry this bearer token off the configured
			// origin, nor replay an annotation PUT's body there (RA6X-039).
			CheckRedirect: sameOriginRedirect,
		},
		base:             cfg.BaseURL,
		token:            token,
		maxResponseBytes: cfg.MaxResponseBytes,
		ua:               "epistula-mcp/" + Version,
	}
}

// apiError is a bounded, model-facing error. It carries the upstream HTTP status
// and (when the body was RFC 7807 problem+json) its title/detail; for a
// transport-level failure (timeout, connection refused) only Message is set. It
// is JSON-serialised into a `{"error": …}` tool result the model can correct —
// never a protocol fault, never a full upstream body dump.
type apiError struct {
	Status  int    `json:"status,omitempty"`
	Title   string `json:"title,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Message string `json:"message,omitempty"`
}

func (e *apiError) Error() string {
	switch {
	case e.Status != 0 && e.Detail != "":
		return fmt.Sprintf("%d %s: %s", e.Status, e.Title, e.Detail)
	case e.Status != 0:
		return fmt.Sprintf("%d %s", e.Status, e.Title)
	default:
		return e.Message
	}
}

// problemBody is the subset of RFC 7807 we surface. We deliberately keep only
// title + detail (status comes from the HTTP line) and drop type/instance.
type problemBody struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

// getJSON issues GET {base}/v1{path}?params and decodes a 2xx JSON body into
// `any`. A non-2xx or transport failure is returned as *apiError.
func (c *client) getJSON(ctx context.Context, path string, params url.Values) (any, *apiError) {
	resp, body, aerr := c.do(ctx, http.MethodGet, path, params, "", nil)
	if aerr != nil {
		return nil, aerr
	}
	if len(body) == 0 {
		return nil, nil
	}
	out, err := decodeExactJSON(body)
	if err != nil {
		return nil, &apiError{Message: fmt.Sprintf("epistula-api returned an undecodable %d response", resp.StatusCode)}
	}
	return out, nil
}

// decodeExactJSON decodes into `any` WITHOUT routing integers through float64
// (RA6X-056).
//
// json.Unmarshal into an interface makes every number a float64, whose 53-bit
// mantissa cannot represent every int64. messages.id is a BIGINT and the API
// keeps it numeric, so an id past 2^53 came back to the model ROUNDED — and a
// rounded id addresses a different message, which the model would then fetch or
// annotate. A restored or externally seeded database is enough to reach that
// range; nothing about the schema or the API forbids it.
//
// UseNumber keeps each number as its original decimal text, so it re-marshals
// into the tool result byte-for-byte as it arrived. Cursors, quotas and sizes
// are preserved for the same reason, and the code that reads numeric fields
// treats them as opaque values to copy rather than arithmetic to do.
func decodeExactJSON(body []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// getText issues GET {base}/v1{path}?params and returns a 2xx text/plain body
// verbatim (used by /v1/messages/{id}/text). A non-2xx or transport failure is
// returned as *apiError.
func (c *client) getText(ctx context.Context, path string, params url.Values) (string, *apiError) {
	_, body, aerr := c.do(ctx, http.MethodGet, path, params, "", nil)
	if aerr != nil {
		return "", aerr
	}
	return string(body), nil
}

// getTextRange fetches one page of a message body using epistula-api's additive
// ?offset=&limit= projection, so paging costs O(page) rather than re-fetching
// the whole body per call (RO5X-016). It returns the page plus the FULL body
// length, which epistula-api reports out-of-band in X-Total-Bytes so the model can
// still reason about progress.
//
// A server that predates the projection would ignore the parameters and send
// the whole body with no X-Total-Bytes; the caller detects the missing header
// and falls back to slicing locally, so the connector stays compatible with an
// older epistula-api.
func (c *client) getTextRange(ctx context.Context, path string, offset, limit int) (page textPage, aerr *apiError) {
	params := url.Values{}
	params.Set("offset", strconv.Itoa(offset))
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	resp, body, aerr := c.do(ctx, http.MethodGet, path, params, "", nil)
	if aerr != nil {
		return textPage{}, aerr
	}
	page = textPage{Text: string(body), Start: offset}
	if raw := resp.Header.Get("X-Total-Bytes"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			page.Total = n
			page.HasTotal = true
		}
	}
	// The server reports where the slice ACTUALLY starts, because an offset
	// landing inside a multibyte rune is advanced to the next boundary
	// (RA6X-018). Paging from the offset we SENT rather than the one that was
	// used re-reads bytes at best and, at a small page size, produces a cursor
	// that never advances.
	if raw := resp.Header.Get("X-Content-Offset"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			page.Start = n
		}
	}
	return page, nil
}

// textPage is one slice of a message body plus everything needed to ask for
// the next one: where the server actually started, and how long the whole body
// is. HasTotal is false against a epistula-api that predates the byte-range
// projection, which sends the whole body and no headers.
type textPage struct {
	Text     string
	Start    int
	Total    int
	HasTotal bool
}

// put issues PUT {base}/v1{path} with a JSON body. epistula-api answers a successful
// annotation write with 204 No Content, so there is nothing to decode; only the
// *apiError (nil on success) matters.
func (c *client) put(ctx context.Context, path string, body any) *apiError {
	raw, err := json.Marshal(body)
	if err != nil {
		return &apiError{Message: "encode request body: " + err.Error()}
	}
	_, _, aerr := c.do(ctx, http.MethodPut, path, nil, "application/json", raw)
	return aerr
}

// do performs one request and reads the full body. On a 2xx it returns
// (resp, body, nil); on any non-2xx it decodes problem+json (or bounds a
// snippet) into *apiError; on a transport failure it returns an *apiError with
// only Message set. The token is set on the header and never appears anywhere
// else.
func (c *client) do(ctx context.Context, method, path string, params url.Values, contentType string, body []byte) (*http.Response, []byte, *apiError) {
	target := c.base + "/v1" + path
	if len(params) > 0 {
		target += "?" + params.Encode()
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, nil, &apiError{Message: "build request: " + err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.ua)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// A context deadline or a net timeout surfaces as a clean, bounded
		// error — never a hang, and never a leak of the target URL.
		var nerr net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &nerr) && nerr.Timeout()) {
			return nil, nil, &apiError{Message: fmt.Sprintf("epistula-api request timed out after %s", c.http.Timeout)}
		}
		return nil, nil, &apiError{Message: "epistula-api request failed: could not reach the API"}
	}
	defer resp.Body.Close()

	// Bound the buffered body. Reading max+1 makes an overflow detectable
	// rather than silently truncating a JSON document into something that
	// would fail to parse with a confusing error (RO5X-016).
	limit := c.maxResponseBytes
	if limit <= 0 {
		limit = defaultMaxResponseBytes
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return resp, nil, &apiError{Message: fmt.Sprintf("reading epistula-api %d response failed", resp.StatusCode)}
	}
	if len(data) > limit {
		return resp, nil, &apiError{
			Status:  resp.StatusCode,
			Message: fmt.Sprintf("epistula-api response exceeded the connector's %d-byte limit", limit),
		}
	}

	if resp.StatusCode >= 400 {
		return resp, data, problemError(resp, data)
	}
	return resp, data, nil
}

// problemError turns a non-2xx response into a bounded *apiError: an RFC 7807
// application/problem+json body yields status+title+detail; anything else yields
// status + a length-capped snippet. Never a full body dump.
func problemError(resp *http.Response, data []byte) *apiError {
	e := &apiError{Status: resp.StatusCode, Title: http.StatusText(resp.StatusCode)}
	ct := resp.Header.Get("Content-Type")
	if len(data) > 0 && (strings.Contains(ct, "application/problem+json") || strings.Contains(ct, "application/json")) {
		var p problemBody
		if json.Unmarshal(data, &p) == nil && (p.Title != "" || p.Detail != "") {
			if p.Title != "" {
				e.Title = p.Title
			}
			e.Detail = bound(p.Detail)
			return e
		}
	}
	// Not decodable as a problem document — keep a bounded snippet only.
	if len(data) > 0 {
		e.Detail = bound(string(data))
	}
	return e
}

func bound(s string) string {
	if len(s) > maxErrBody {
		return s[:maxErrBody] + "…"
	}
	return s
}
