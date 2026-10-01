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
	"strings"
	"time"
)

type MailAPIClient struct {
	base  *url.URL
	token string
	// client carries the total-deadline http.Client.Timeout and is used for
	// the short, bounded PutAnnotation request.
	client *http.Client
	// exportClient has NO total deadline (Timeout: 0) because /v1/export is a
	// legitimately long-lived stream — the worker does per-message LLM work
	// (up to minutes) between reads of the still-open body, so a total timeout
	// would abort virtually every real pass. Liveness is instead bounded by
	// the transport (dial/TLS/response-header) and an idle-read watchdog in
	// Export. See R-009.
	exportClient   *http.Client
	requestTimeout time.Duration
}

func NewMailAPIClient(cfg MailAPIConfig) (*MailAPIClient, error) {
	base, err := parseBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	reqTimeout := time.Duration(cfg.RequestTimeoutSec) * time.Second
	return &MailAPIClient{
		base:  base,
		token: cfg.Token,
		client: &http.Client{
			Timeout: reqTimeout,
			// A redirect must not carry this bearer token off the configured
			// origin, nor replay an annotation PUT's body there (RA6X-039).
			CheckRedirect: sameOriginRedirect,
		},
		exportClient: &http.Client{
			Timeout:       0, // no total deadline; Export uses an idle-read watchdog
			CheckRedirect: sameOriginRedirect,
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: reqTimeout, // bound the wait for response headers
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
		requestTimeout: reqTimeout,
	}, nil
}

// Export streams /v1/export. notAnnotatedBy, when non-empty, asks epistula-api to
// skip messages this model has already annotated, so each GPU box pulls only
// its undone work instead of streaming the whole corpus every pass. Older
// epistula-api builds that don't know the filter ignore the param, and the
// client-side per-model skip in the worker still applies.
func (c *MailAPIClient) Export(ctx context.Context, cfg MailAPIConfig, notAnnotatedBy string, handle func(Message) error) error {
	return c.ExportFiltered(ctx, cfg, exportFilter{NotAnnotatedBy: notAnnotatedBy}, handle)
}

// exportFilter is what a pass asks /v1/export for on top of the configured
// [mail_api] filters.
type exportFilter struct {
	// NotAnnotatedBy skips messages this annotation model already annotated.
	NotAnnotatedBy string
	// NotClassified selects messages with no classification into an active
	// category of their mailbox: the classification pass's undone work.
	NotClassified bool
	// Mailbox overrides mail_api.mailbox for this stream.
	Mailbox string
	// PassRequired restricts the stream to messages queued for the
	// annotation pipeline (epistula-database migration 022): the fast round.
	PassRequired bool
}

// ExportFiltered is Export with the full filter set.
func (c *MailAPIClient) ExportFiltered(ctx context.Context, cfg MailAPIConfig, f exportFilter, handle func(Message) error) error {
	u := c.base.ResolveReference(&url.URL{Path: strings.TrimRight(c.base.Path, "/") + "/v1/export"})
	q := u.Query()
	mailbox := cfg.Mailbox
	if f.Mailbox != "" {
		mailbox = f.Mailbox
	}
	if mailbox != "" {
		q.Set("mailbox", mailbox)
	}
	if f.NotAnnotatedBy != "" {
		q.Set("not_annotated_by", f.NotAnnotatedBy)
	}
	if f.NotClassified {
		q.Set("not_classified", "true")
	}
	if f.PassRequired {
		q.Set("pass_required", "true")
	}
	if cfg.Folder != "" {
		q.Set("folder", cfg.Folder)
	}
	if cfg.Since != "" {
		q.Set("since", cfg.Since)
	}
	if cfg.Before != "" {
		q.Set("before", cfg.Before)
	}
	if cfg.Query != "" {
		q.Set("q", cfg.Query)
	}
	if cfg.Tag != "" {
		q.Set("tag", cfg.Tag)
	}
	if cfg.Category != "" {
		q.Set("category", cfg.Category)
	}
	u.RawQuery = q.Encode()

	// A dedicated child context lets the idle-read watchdog abort a stalled
	// stream without disturbing the parent (SIGTERM) context — and the
	// request still honors the parent, so shutdown mid-stream works.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	c.authorize(req)

	resp, err := c.exportClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		// Structured, so a 401/429/503 on the export itself is classified as
		// the systemic failure it is rather than an opaque formatted string
		// (RA6X-042).
		return statusErrorFrom(ServiceMailAPI, resp, problemDetail(body))
	}

	// Idle-read watchdog: bound the gap between INPUT, not the work done with
	// it. If no bytes arrive within requestTimeout (server hung mid-stream),
	// cancel streamCtx so the blocked read unwinds. The cumulative work of a
	// full pass is deliberately NOT bounded here (that was the R-009 bug), and
	// there is deliberately no per-row processing cap either: truncating a row
	// would silently drop mail, and a large row is a legitimate message, not a
	// stall.
	//
	// The budget covers exactly one blocked read (RA6X-014). Everything else —
	// decoding, the two unmarshal passes, and the caller's LLM inference — is
	// outside it by construction, so nothing has to remember to pause the
	// watch around slow work. That also fixes RO5X-015 permanently: one budget
	// no longer has to cover [LLM inference + retries + backoff + the next
	// decode], which with the shipped defaults is 3 x 300s + 2 x 2s = 904s
	// against a 300s watchdog, killing healthy passes with an error that
	// blamed epistula-api for the worker's own latency.
	watch := newIdleWatch(c.requestTimeout, cancel)
	defer watch.Stop()
	idleErr := func() error {
		return fmt.Errorf("export stream idle for %s with no data; aborting pass", c.requestTimeout)
	}

	// A json.Decoder reads the NDJSON stream as concatenated JSON values, so
	// there is no per-line length cap (a text-heavy message near the 50 MiB
	// ingest ceiling serializes to one row larger than any fixed buffer — the
	// R-032 bug that stalled every future pass at that row).
	dec := json.NewDecoder(&watchedReader{r: resp.Body, w: watch})
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			// Cancellation is checked BEFORE EOF (RA6X-014). Cancelling the
			// request closes the body, and the read that unwinds may surface
			// as io.EOF — indistinguishable, at this point, from the server
			// having finished. Accepting it as completion reported a
			// successful pass over a stream that had been torn down.
			if watch.Fired() {
				return idleErr()
			}
			if ctx.Err() != nil {
				return ctx.Err() // parent canceled (SIGTERM) — propagate for clean shutdown
			}
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("read export stream: %w", err)
		}

		// Structural in-band error detection (R-010): only a decoded object
		// with a non-empty "error" field AND no positive "id" is the abort
		// sentinel. A substring match falsely fired on a legitimate row whose
		// tag/subject/flag was literally "error", permanently stalling other
		// models at that row. Never echo the raw row in the error — it carries
		// subject/addresses/body (R-036); include only the server's short
		// error string.
		var probe struct {
			Error string `json:"error"`
			ID    int64  `json:"id"`
		}
		if err := json.Unmarshal(raw, &probe); err == nil && probe.Error != "" && probe.ID <= 0 {
			return fmt.Errorf("epistula-api export error: %s", probe.Error)
		}

		var msg Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			// Surface size + err class, not the payload (R-036).
			return fmt.Errorf("decode export row (%d bytes): %w", len(raw), err)
		}
		// The watch is not armed here: it covers reads only, and the caller's
		// LLM inference is not the stream being idle.
		if err := handle(msg); err != nil {
			return err
		}
	}
	return nil
}

func (c *MailAPIClient) PutAnnotation(ctx context.Context, messageID int64, put AnnotationPut) error {
	body, err := json.Marshal(&put)
	if err != nil {
		return err
	}
	u := c.base.ResolveReference(&url.URL{Path: fmt.Sprintf("%s/v1/messages/%d/annotation", strings.TrimRight(c.base.Path, "/"), messageID)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.authorize(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		// epistula-api's non-2xx bodies are RFC 7807 problem documents this project
		// writes itself — a title and a detail about scope, permission or
		// validation — so a bounded detail is a safe and useful diagnostic
		// here. The LM Studio path deliberately attaches none; see
		// statusErrorFrom (RA6X-043).
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return statusErrorFrom(ServiceMailAPI, resp, problemDetail(body))
	}
	return nil
}

// PutClassification stores a message's archive classification. A 422 means
// the category is no longer active for the mailbox (the list changed since
// this pass fetched it); a 404, that the message is gone.
func (c *MailAPIClient) PutClassification(ctx context.Context, messageID int64, put ClassificationPut) error {
	body, err := json.Marshal(&put)
	if err != nil {
		return err
	}
	u := c.base.ResolveReference(&url.URL{Path: fmt.Sprintf("%s/v1/messages/%d/classification", strings.TrimRight(c.base.Path, "/"), messageID)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return statusErrorFrom(ServiceMailAPI, resp, problemDetail(body))
	}
	return nil
}

// PassPrune asks epistula-api to clear the queue markers of messages this model
// has finished (POST /v1/pass-required/prune).
type PassPrune struct {
	Model string `json:"model"`
	// Mailbox limits the prune to one mailbox of the token's scope.
	Mailbox string `json:"mailbox,omitempty"`
	// RequireClassification keeps a message queued until it is also
	// classified into an active category of its mailbox (a mailbox with no
	// categories needs none).
	RequireClassification bool `json:"require_classification"`
}

// PassPruneResult is how many markers a prune cleared and how many remain
// queued in its scope.
type PassPruneResult struct {
	Pruned    int64 `json:"pruned"`
	Remaining int64 `json:"remaining"`
}

// PrunePassQueue clears the markers of finished messages. A epistula-api without
// the queue answers 503 (see isPassQueueUnavailable).
func (c *MailAPIClient) PrunePassQueue(ctx context.Context, p PassPrune) (PassPruneResult, error) {
	var out PassPruneResult
	body, err := json.Marshal(&p)
	if err != nil {
		return out, err
	}
	u := c.base.ResolveReference(&url.URL{Path: strings.TrimRight(c.base.Path, "/") + "/v1/pass-required/prune"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	c.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return out, statusErrorFrom(ServiceMailAPI, resp, problemDetail(detail))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&out); err != nil {
		return out, fmt.Errorf("decode prune result: %w", err)
	}
	return out, nil
}

// isPassQueueUnavailable reports whether err is epistula-api itself answering 503
// for the annotation pass queue: a epistula-api or database without migration 022.
// A 503 that is not epistula-api's own problem document (a proxy with the backend
// down) is an outage, not a missing feature.
func isPassQueueUnavailable(err error) bool {
	var statusErr *HTTPStatusError
	return errors.As(err, &statusErr) &&
		statusErr.Service == ServiceMailAPI &&
		statusErr.StatusCode == http.StatusServiceUnavailable &&
		strings.HasPrefix(statusErr.ContentType, "application/problem+json")
}

// ArchiveCategories returns the mailbox's active archive categories. An empty
// list means archive sorting is not set up for the mailbox.
func (c *MailAPIClient) ArchiveCategories(ctx context.Context, mailbox string) ([]ArchiveCategory, error) {
	var out struct {
		Categories []ArchiveCategory `json:"categories"`
	}
	path := fmt.Sprintf("%s/v1/mailboxes/%s/archive-categories", strings.TrimRight(c.base.Path, "/"), url.PathEscape(mailbox))
	if err := c.getJSON(ctx, path, &out); err != nil {
		return nil, err
	}
	return out.Categories, nil
}

// Mailboxes returns the names of the mailboxes the token may read.
func (c *MailAPIClient) Mailboxes(ctx context.Context) ([]string, error) {
	var out struct {
		Mailboxes []struct {
			Name string `json:"name"`
		} `json:"mailboxes"`
	}
	if err := c.getJSON(ctx, strings.TrimRight(c.base.Path, "/")+"/v1/mailboxes", &out); err != nil {
		return nil, err
	}
	names := make([]string, len(out.Mailboxes))
	for i, m := range out.Mailboxes {
		names[i] = m.Name
	}
	return names, nil
}

// getJSON performs a bounded GET and decodes a JSON response of at most 1 MiB.
func (c *MailAPIClient) getJSON(ctx context.Context, path string, v any) error {
	u := c.base.ResolveReference(&url.URL{Path: path})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	c.authorize(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return statusErrorFrom(ServiceMailAPI, resp, problemDetail(body))
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func (c *MailAPIClient) authorize(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
}

// problemDetail extracts the human-readable part of an RFC 7807 document, or
// falls back to a bounded snippet of a non-JSON body. epistula-api is the only
// caller: its error bodies are this project's own problem documents and carry
// no message content.
func problemDetail(body []byte) string {
	const maxDetail = 300
	var p struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(body, &p); err == nil && (p.Title != "" || p.Detail != "") {
		out := strings.TrimSpace(p.Detail)
		if out == "" {
			out = strings.TrimSpace(p.Title)
		}
		return boundedToken(out)
	}
	out := strings.TrimSpace(string(body))
	if len(out) > maxDetail {
		out = out[:maxDetail]
	}
	return boundedToken(out)
}
