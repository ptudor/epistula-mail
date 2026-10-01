package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type LMStudioClient struct {
	base   *url.URL
	token  string
	cfg    LMStudioConfig
	client *http.Client
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model          string        `json:"model"`
	Messages       []chatMessage `json:"messages"`
	Temperature    float64       `json:"temperature"`
	MaxTokens      int           `json:"max_tokens"`
	Stream         bool          `json:"stream"`
	ResponseFormat any           `json:"response_format,omitempty"`
	// ReasoningEffort is sent only when configured (lmstudio.reasoning_effort).
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			// Reasoning models (e.g. Qwen3) leave Content empty and place the
			// schema-constrained answer here. See decodeAnnotation.
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
		// FinishReason is "length" when max_tokens truncated the answer
		// mid-JSON. Extracting from a truncated payload risks promoting a
		// complete <think> draft over the (now-incomplete) real answer, so we
		// bail out instead. See R-034.
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage,omitempty"`
}

func NewLMStudioClient(cfg LMStudioConfig) (*LMStudioClient, error) {
	base, err := parseBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	return &LMStudioClient{
		base:  base,
		token: cfg.APIToken,
		cfg:   cfg,
		client: &http.Client{
			Timeout: time.Duration(cfg.RequestTimeoutSec) * time.Second,
			// A redirect must not carry this API token off the configured
			// origin, nor replay an inference POST — whose body is the mail
			// text — there (RA6X-039).
			CheckRedirect: sameOriginRedirect,
		},
	}, nil
}

func (c *LMStudioClient) Annotate(ctx context.Context, prompt Prompt) (LLMAnnotation, *chatResponse, error) {
	chat, err := c.complete(ctx, prompt, annotationResponseFormat())
	if err != nil {
		return LLMAnnotation{}, chat, err
	}
	msg := chat.Choices[0].Message
	ann, err := decodeAnnotation(msg.Content, msg.ReasoningContent)
	if err != nil {
		return LLMAnnotation{}, chat, fmt.Errorf("decode annotation JSON: %w", err)
	}
	return ann, chat, nil
}

// complete sends one chat-completions request constrained by format and
// returns the response once it has a complete first choice. A transport or
// HTTP failure returns no response; a response with no choices, or one cut
// short by max_tokens, is returned together with its error so its usage can
// still be read.
func (c *LMStudioClient) complete(ctx context.Context, prompt Prompt, format any) (*chatResponse, error) {
	reqBody := chatRequest{
		Model:       c.cfg.Model,
		Temperature: c.cfg.Temperature,
		MaxTokens:   c.cfg.MaxTokens,
		Stream:      false,
		Messages: []chatMessage{
			{Role: "system", Content: prompt.System},
			{Role: "user", Content: prompt.User},
		},
		ResponseFormat:  format,
		ReasoningEffort: c.cfg.ReasoningEffort,
	}
	body, err := json.Marshal(&reqBody)
	if err != nil {
		return nil, err
	}
	u := c.base.ResolveReference(&url.URL{Path: strings.TrimRight(c.base.Path, "/") + "/chat/completions"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Structured, and WITHOUT the response body (RA6X-042, RA6X-043).
		//
		// Classification: a formatted string could not be told apart from a
		// per-message content failure, so a 401/429/503 from the inference
		// server let the worker churn the entire corpus during a total outage
		// and still report a clean pass.
		//
		// Content: an inference server or a proxy in front of it commonly
		// echoes the rejected prompt, and the prompt is the mail. Up to 4 MiB
		// of arbitrary upstream text used to be formatted into this error and
		// logged at WARN for every message. The body is not read at all now —
		// the status, media type and correlation id below are the diagnostic,
		// and a size cap on a log line was never redaction.
		statusErr := statusErrorFrom(ServiceLMStudio, resp, "")
		if resp.StatusCode == http.StatusBadRequest && isContextOverflow(resp.Body) {
			return nil, &ContextOverflowError{Status: statusErr}
		}
		return nil, statusErr
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, err
	}

	var chat chatResponse
	if err := json.Unmarshal(respBody, &chat); err != nil {
		return nil, fmt.Errorf("decode lmstudio response: %w", err)
	}
	if len(chat.Choices) == 0 {
		return &chat, fmt.Errorf("lmstudio response has no choices")
	}
	if chat.Choices[0].FinishReason == "length" {
		// Truncated mid-answer: do NOT extract (a complete <think> draft could
		// win over the incomplete real answer). Retryable — the worker retries
		// per-message; a persistent truncation means raising max_tokens.
		return &chat, fmt.Errorf("lmstudio truncated response (finish_reason=length); raise max_tokens")
	}
	return &chat, nil
}

// decodeAnnotation pulls the annotation JSON out of an LM Studio chat response.
// Non-reasoning models (gpt-oss, gemma, ...) return the JSON in message.content.
// Reasoning models (Qwen3.x, ...) constrained by a strict json_schema leave
// content empty and place the JSON in message.reasoning_content instead. So we
// prefer content, fall back to the reasoning channel, and in either case
// tolerate a single JSON object embedded in surrounding text (inline <think>
// blocks, markdown fences, etc.).
//
// When a candidate isn't clean JSON we enumerate every brace-balanced object
// within it and accept the LAST one that validates — a completed <think> draft
// often precedes the real answer, so a naive first-'{'-to-last-'}' span would
// wrongly capture the draft (R-034). Every accepted object must carry the
// schema-required keys (summary, tags, category); an empty {} or unrelated
// object is rejected rather than stored as a junk annotation (R-035).
func decodeAnnotation(content, reasoning string) (LLMAnnotation, error) {
	return decodeLastValid(content, reasoning, unmarshalAnnotation)
}

// decodeLastValid is decodeAnnotation's search for the model's answer, for any
// response shape: clean JSON first, in content then in the reasoning channel,
// otherwise the LAST brace-balanced object that parse accepts.
func decodeLastValid[T any](content, reasoning string, parse func(string) (T, error)) (T, error) {
	var zero T
	lastErr := fmt.Errorf("response had no content or reasoning_content")
	for _, candidate := range []string{content, reasoning} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		// Clean JSON (the strict json_schema happy path) first.
		if v, err := parse(candidate); err == nil {
			return v, nil
		} else {
			lastErr = err
		}
		// Otherwise take the last brace-balanced object that validates.
		var best *T
		for _, obj := range balancedObjects(candidate) {
			v, err := parse(obj)
			if err != nil {
				lastErr = err
				continue
			}
			best = &v
		}
		if best != nil {
			return *best, nil
		}
	}
	return zero, lastErr
}

// unmarshalAnnotation parses s into an LLMAnnotation, rejecting any object
// whose schema-required fields (summary, tags, category) are missing, of the
// wrong TYPE, or empty where the schema requires content.
//
// Key presence alone was not enough (RA6X-019). JSON null decodes into a
// zero-valued Go string or a nil slice WITHOUT an error, so
// `{"summary":null,"tags":null,"category":null}` passed every check here and
// became an empty annotation. normalizeAnnotation then substituted its
// fallbacks — "(no useful summary produced)", the fallback category, that
// category as the only tag — and the worker PUT the result under this model's
// name. Every later pass skipped the message as already annotated, so a model
// that had produced nothing usable was recorded as having done the work, and
// the failure was invisible from then on.
//
// The same check guards the salvage path that pulls objects out of model prose,
// where a half-written draft is exactly the kind of object most likely to carry
// a null.
//
// An EMPTY TAGS ARRAY is valid and distinct from null: the schema asks for a
// list, and "no tags apply" is a real answer. An empty or whitespace-only
// summary or category is not — those are the fields the annotation exists for.
func unmarshalAnnotation(s string) (LLMAnnotation, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &probe); err != nil {
		return LLMAnnotation{}, err
	}
	for _, k := range []string{"summary", "tags", "category"} {
		if _, ok := probe[k]; !ok {
			return LLMAnnotation{}, fmt.Errorf("annotation object missing required key %q", k)
		}
	}
	for _, k := range []string{"summary", "category"} {
		if err := requireJSONKind(probe[k], '"', "string", k); err != nil {
			return LLMAnnotation{}, err
		}
	}
	if err := requireJSONKind(probe["tags"], '[', "array", "tags"); err != nil {
		return LLMAnnotation{}, err
	}

	var ann LLMAnnotation
	if err := json.Unmarshal([]byte(s), &ann); err != nil {
		return LLMAnnotation{}, err
	}
	if strings.TrimSpace(ann.Summary) == "" {
		return LLMAnnotation{}, fmt.Errorf("annotation summary is empty")
	}
	if strings.ContainsRune(ann.Summary, '\x00') {
		return LLMAnnotation{}, fmt.Errorf("annotation summary contains NUL")
	}
	if strings.TrimSpace(ann.Category) == "" {
		return LLMAnnotation{}, fmt.Errorf("annotation category is empty")
	}
	for i, tag := range ann.Tags {
		if strings.TrimSpace(tag) == "" {
			return LLMAnnotation{}, fmt.Errorf("annotation tag %d is empty", i+1)
		}
	}
	return ann, nil
}

// requireJSONKind reports whether raw is the JSON kind the schema requires,
// identified by its first non-whitespace byte. json.Unmarshal into a Go string
// or slice silently accepts null and would accept a number for a string field
// only with an error message that names the Go type rather than the schema
// field, so the check is made here where the field name is known.
func requireJSONKind(raw json.RawMessage, want byte, kind, field string) error {
	v := bytes.TrimSpace(raw)
	if len(v) == 0 {
		return fmt.Errorf("annotation field %q is empty", field)
	}
	if v[0] != want {
		got := "a value"
		switch {
		case bytes.Equal(v, []byte("null")):
			got = "null"
		case v[0] == '"':
			got = "a string"
		case v[0] == '[':
			got = "an array"
		case v[0] == '{':
			got = "an object"
		case v[0] == 't' || v[0] == 'f':
			got = "a boolean"
		case v[0] == '-' || (v[0] >= '0' && v[0] <= '9'):
			got = "a number"
		}
		return fmt.Errorf("annotation field %q is %s, want %s", field, got, kind)
	}
	return nil
}

// balancedObjects returns every top-level brace-balanced {...} substring of s,
// honoring JSON string and escape rules so a brace inside a string literal
// doesn't skew the depth count. A truncated object with no closing brace is
// not returned (there is nothing balanced to return) — that case is handled
// upstream by the finish_reason=length check.
func balancedObjects(s string) []string {
	var objs []string
	depth, start := 0, -1
	inStr, escaped := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					objs = append(objs, s[start:i+1])
					start = -1
				}
			}
		}
	}
	return objs
}

func annotationResponseFormat() map[string]any {
	return map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{
			"name":   "mail_annotation",
			"strict": true,
			"schema": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"summary": map[string]any{"type": "string"},
					"tags": map[string]any{
						"type":  "array",
						"items": map[string]any{"type": "string"},
					},
					"category": map[string]any{"type": "string"},
				},
				"required": []string{"summary", "tags", "category"},
			},
		},
	}
}

// ContextOverflowError is an LM Studio refusal because the prompt does not fit
// the loaded model's context window: llama.cpp's "the number of tokens to keep
// from the initial prompt is greater than the context length". It is
// deterministic, so the same request is never retried. The caller retries with
// less of the message body instead. It is a property of one message, not an
// outage, so it never counts toward the infrastructure abort.
type ContextOverflowError struct {
	Status *HTTPStatusError
}

func (e *ContextOverflowError) Error() string {
	return "lmstudio: the prompt does not fit the model's context window (" + e.Status.Error() + ")"
}

func (e *ContextOverflowError) Unwrap() error { return e.Status }

// contextOverflowMarkers identify a context-window refusal in an error body.
var contextOverflowMarkers = []string{
	"context length", "context_length", "context window", "n_ctx", "n_keep", "maximum context",
}

// isContextOverflow reads at most 8 KiB of an LM Studio error body to decide
// whether it is a context-window refusal. The body is only matched against
// fixed markers; it is never kept, logged or put into an error, because an
// inference server may echo the prompt, which is the mail (RA6X-043).
func isContextOverflow(body io.Reader) bool {
	b, err := io.ReadAll(io.LimitReader(body, 8*1024))
	if err != nil && len(b) == 0 {
		return false
	}
	lower := strings.ToLower(string(b))
	for _, m := range contextOverflowMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}
