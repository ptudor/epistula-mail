package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// These tests drive full tool-call round trips through the real epistula-api client
// against an httptest mock of epistula-api's /v1 surface. No live store, no DB — the
// project has none — but every request is shaped and every response slimmed by
// the production code path. The mock captures the last request per route so the
// tests can assert query/body forwarding (relative-time conversion, limit
// clamping, annotate body shape) as well as response projection.

type mockAPI struct {
	server   *httptest.Server
	lastAuth string
	lastPath string
	// lastRawPath is the path as it went over the wire, percent-escapes
	// intact — the only view that shows whether a name reached the server as
	// data or as path structure (RA6X-057).
	lastRawPath string
	lastQ       url.Values
	lastBody    map[string]any
}

func newMockAPI(t *testing.T) *mockAPI {
	t.Helper()
	m := &mockAPI{}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/mailboxes", func(w http.ResponseWriter, r *http.Request) {
		m.capture(r)
		writeJSON(w, map[string]any{"mailboxes": []any{
			map[string]any{"name": "jdoe", "used_bytes": 123, "quota_bytes": 0, "disabled": false},
			map[string]any{"name": "ops", "used_bytes": 9, "quota_bytes": 1000, "disabled": true},
		}})
	})

	mux.HandleFunc("GET /v1/mailboxes/jdoe/folders", func(w http.ResponseWriter, r *http.Request) {
		m.capture(r)
		writeJSON(w, map[string]any{"mailbox": "jdoe", "folders": []any{
			map[string]any{"name": "INBOX", "uidvalidity": 1, "uidnext": 10, "message_count": 5, "special_use": ""},
			map[string]any{"name": "Archive/2026", "uidvalidity": 1, "uidnext": 3, "message_count": 2, "special_use": "\\Archive"},
		}})
	})

	// Out-of-scope mailbox → 403 problem+json.
	mux.HandleFunc("GET /v1/mailboxes/secret/folders", func(w http.ResponseWriter, r *http.Request) {
		m.capture(r)
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "https://api.ptudor.invalid/errors/forbidden", "title": "Forbidden",
			"status": 403, "detail": "Token is not scoped to mailbox 'secret'.",
		})
	})

	mux.HandleFunc("GET /v1/mailboxes/jdoe/folders/{rest...}", func(w http.ResponseWriter, r *http.Request) {
		m.capture(r)
		// Echo the folder this route actually resolved, exactly as epistula-api's
		// handler does. It used to answer a fixed "INBOX" whatever was asked,
		// which is precisely the substitution RA6X-057 is about — a mock that
		// cannot express the bug cannot witness the fix.
		folder, _ := strings.CutSuffix(r.PathValue("rest"), "/messages")
		writeJSON(w, map[string]any{
			"mailbox": "jdoe", "folder": folder,
			"messages": []any{
				map[string]any{
					"id": 42, "uid": 7, "internal_date": "2026-07-01T00:00:00Z",
					"subject": "Invoice #9", "from": "billing@host.invalid",
					"to": []any{"me@x.invalid"}, "cc": []any{}, "flags": []any{"\\Seen"},
					"size": 2048, "attachment_count": 1,
				},
			},
			"next_cursor": "OPAQUE-CURSOR-1",
		})
	})

	mux.HandleFunc("GET /v1/messages/{id}", func(w http.ResponseWriter, r *http.Request) {
		m.capture(r)
		writeJSON(w, map[string]any{
			"id": 42, "uid": 7, "mailbox": "jdoe", "folder": "INBOX",
			"internal_date": "2026-07-01T00:00:00Z", "sent_date": "2026-06-30T20:00:00Z",
			"subject": "Invoice #9", "from": "billing@host.invalid", "to": []any{"me@x.invalid"},
			"message_id": "<abc@host.invalid>", "flags": []any{"\\Seen"}, "size": 2048,
			"headers": map[string]any{"X-Spam": "no"}, "bodystructure": map[string]any{"type": "text"},
			"text_body": "Please pay the invoice.",
			"html_body": "<p>Please pay the invoice.</p>",
			"attachments": []any{
				map[string]any{"part_number": "2", "filename": "invoice.pdf", "content_type": "application/pdf",
					"size_bytes": 1000, "sha256": strings.Repeat("a", 64)},
			},
			"annotations": []any{
				map[string]any{"model": "big", "tags": []any{"receipts"}, "category": "finance",
					"summary": "An invoice for $9.", "priority": 10, "primary": true,
					"tokens_in": 100, "tokens_out": 20, "created_at": "2026-07-02T00:00:00Z"},
				map[string]any{"model": "fast", "tags": []any{"bill"}, "priority": 0, "primary": false,
					"created_at": "2026-07-02T01:00:00Z"},
			},
		})
	})

	mux.HandleFunc("GET /v1/messages/{id}/text", func(w http.ResponseWriter, r *http.Request) {
		m.capture(r)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, strings.Repeat("z", 100))
	})

	mux.HandleFunc("GET /v1/search", func(w http.ResponseWriter, r *http.Request) {
		m.capture(r)
		writeJSON(w, map[string]any{
			"q": "invoice",
			"messages": []any{
				map[string]any{"id": 42, "uid": 7, "mailbox": "jdoe", "folder": "INBOX",
					"internal_date": "2026-07-01T00:00:00Z", "subject": "Invoice #9",
					"from": "billing@host.invalid", "flags": []any{}, "size": 2048},
			},
		})
	})

	mux.HandleFunc("PUT /v1/messages/{id}/annotation", func(w http.ResponseWriter, r *http.Request) {
		m.capture(r)
		body, _ := io.ReadAll(r.Body)
		m.lastBody = map[string]any{}
		_ = json.Unmarshal(body, &m.lastBody)
		w.WriteHeader(http.StatusNoContent)
	})

	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockAPI) capture(r *http.Request) {
	m.lastAuth = r.Header.Get("Authorization")
	m.lastPath = r.URL.Path
	m.lastRawPath = r.URL.EscapedPath()
	m.lastQ = r.URL.Query()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// newTestToolset wires the real client at the mock's URL. httptest binds
// 127.0.0.1, so the loopback-http rule is satisfied.
//
// It supplies BOTH credentials so the annotate tool is present, because most
// tests here exercise tool behaviour rather than the read/write split. The
// split itself — that annotate is absent without a write token, and that it
// uses the write token when present — is covered in tokensplit_test.go.
func newTestToolset(t *testing.T, m *mockAPI) *toolset {
	t.Helper()
	cfg := config{
		BaseURL:        m.server.URL,
		Token:          "test-token",
		AnnotateToken:  "test-annotate-token",
		RequestTimeout: defaultRequestTimeout,
		MaxLimit:       200,
		MaxTextBytes:   50,
	}
	return &toolset{
		c:   newClient(cfg),
		w:   newClientWithToken(cfg, cfg.AnnotateToken),
		cfg: cfg,
	}
}

// decodeResult extracts and JSON-decodes the text content of a tool result.
func decodeResult(t *testing.T, res *mcp.CallToolResult, err error) map[string]any {
	t.Helper()
	if err != nil {
		t.Fatalf("tool returned a protocol error: %v", err)
	}
	if len(res.Content) == 0 {
		t.Fatal("tool result had no content")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("tool result content was %T, want *mcp.TextContent", res.Content[0])
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &out); err != nil {
		t.Fatalf("tool result not JSON: %v (%q)", err, tc.Text)
	}
	return out
}

func TestIntegrationMailboxes(t *testing.T) {
	m := newMockAPI(t)
	ts := newTestToolset(t, m)
	res, _, err := ts.mailboxes(context.Background(), nil, mailboxesInput{})
	out := decodeResult(t, res, err)
	if m.lastAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want bearer test-token", m.lastAuth)
	}
	mbs := out["mailboxes"].([]any)
	if len(mbs) != 2 {
		t.Fatalf("got %d mailboxes, want 2", len(mbs))
	}
	if mbs[0].(map[string]any)["name"] != "jdoe" {
		t.Errorf("first mailbox = %v, want jdoe", mbs[0])
	}
}

func TestIntegrationFolders(t *testing.T) {
	m := newMockAPI(t)
	ts := newTestToolset(t, m)
	res, _, err := ts.folders(context.Background(), nil, foldersInput{Mailbox: "jdoe"})
	out := decodeResult(t, res, err)
	folders := out["folders"].([]any)
	if len(folders) != 2 {
		t.Fatalf("got %d folders, want 2", len(folders))
	}
	f0 := folders[0].(map[string]any)
	if f0["name"] != "INBOX" || f0["message_count"].(float64) != 5 {
		t.Errorf("folder projection wrong: %+v", f0)
	}
	// special_use is empty on INBOX and must be omitted, present on Archive.
	if _, ok := f0["special_use"]; ok {
		t.Errorf("empty special_use should be omitted: %+v", f0)
	}
}

func TestIntegrationForbiddenIsCorrectableError(t *testing.T) {
	m := newMockAPI(t)
	ts := newTestToolset(t, m)
	res, _, err := ts.folders(context.Background(), nil, foldersInput{Mailbox: "secret"})
	out := decodeResult(t, res, err) // must be a normal result, not a protocol error
	e, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an {\"error\": …} result, got %+v", out)
	}
	if e["status"].(float64) != 403 {
		t.Errorf("error status = %v, want 403", e["status"])
	}
	if !strings.Contains(e["detail"].(string), "not scoped") {
		t.Errorf("error detail = %v, want the upstream 403 detail", e["detail"])
	}
}

func TestIntegrationMessagesForwardsFilters(t *testing.T) {
	m := newMockAPI(t)
	ts := newTestToolset(t, m)
	res, _, err := ts.messages(context.Background(), nil, messagesInput{
		Mailbox: "jdoe", Folder: "Archive/2026",
		Since: "2026-01-01T00:00:00Z", Flag: "\\Seen", Larger: 500,
		Limit: 9999, // over the cap
	})
	out := decodeResult(t, res, err)
	// The folder is now one opaque path segment: its own slash is escaped so
	// routing cannot clean it (RA6X-057). r.URL.Path is the DECODED path, so
	// the folder's slash reappears there — the wire form is RawPath.
	if m.lastRawPath != "/v1/mailboxes/jdoe/folders/Archive%2F2026/messages" {
		t.Errorf("raw path = %q, want the folder escaped into one segment", m.lastRawPath)
	}
	if m.lastPath != "/v1/mailboxes/jdoe/folders/Archive/2026/messages" {
		t.Errorf("decoded path = %q, want the folder name back intact", m.lastPath)
	}
	if out["folder"] != "Archive/2026" {
		t.Errorf("folder = %v, want the requested folder echoed back", out["folder"])
	}
	if got := m.lastQ.Get("since"); got != "2026-01-01T00:00:00Z" {
		t.Errorf("since = %q, want absolute passthrough", got)
	}
	if got := m.lastQ.Get("flag"); got != "\\Seen" {
		t.Errorf("flag = %q", got)
	}
	if got := m.lastQ.Get("larger"); got != "500" {
		t.Errorf("larger = %q, want 500", got)
	}
	if got := m.lastQ.Get("limit"); got != "200" {
		t.Errorf("limit = %q, want clamped to 200", got)
	}
	if out["next_cursor"] != "OPAQUE-CURSOR-1" {
		t.Errorf("next_cursor = %v, want the opaque cursor forwarded", out["next_cursor"])
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
}

func TestIntegrationMessagesRelativeTimeConverted(t *testing.T) {
	m := newMockAPI(t)
	ts := newTestToolset(t, m)
	_, _, _ = ts.messages(context.Background(), nil, messagesInput{
		Mailbox: "jdoe", Folder: "INBOX", Since: "2d",
	})
	got := m.lastQ.Get("since")
	if got == "2d" || got == "" {
		t.Fatalf("since = %q, want a converted RFC 3339 timestamp", got)
	}
	if !strings.HasSuffix(got, "Z") {
		t.Errorf("converted since = %q, want a UTC RFC 3339 value", got)
	}
}

func TestIntegrationMessageSlimming(t *testing.T) {
	m := newMockAPI(t)
	ts := newTestToolset(t, m) // MaxTextBytes = 50
	res, _, err := ts.message(context.Background(), nil, messageInput{ID: 42})
	out := decodeResult(t, res, err)

	if _, ok := out["html_body"]; ok {
		t.Error("html_body must never be forwarded")
	}
	if _, ok := out["headers"]; ok {
		t.Error("raw headers blob should be dropped by the slim projection")
	}
	// text_body is short (< 50 bytes) so it should be present and not truncated.
	if out["text_body"] != "Please pay the invoice." {
		t.Errorf("text_body = %v", out["text_body"])
	}
	if _, ok := out["text_body_truncated"]; ok {
		t.Error("short body should not be flagged truncated")
	}
	// Attachments: sha256 dropped, filename/content_type/size kept.
	atts := out["attachments"].([]any)
	a0 := atts[0].(map[string]any)
	if _, ok := a0["sha256"]; ok {
		t.Error("attachment sha256 should be dropped")
	}
	if a0["filename"] != "invoice.pdf" {
		t.Errorf("attachment filename = %v", a0["filename"])
	}
	// Ranked annotations retained with priority/primary; token accounting dropped.
	anns := out["annotations"].([]any)
	if len(anns) != 2 {
		t.Fatalf("got %d annotations, want 2", len(anns))
	}
	first := anns[0].(map[string]any)
	if first["primary"] != true || first["summary"] != "An invoice for $9." {
		t.Errorf("primary annotation wrong: %+v", first)
	}
	if _, ok := first["tokens_in"]; ok {
		t.Error("token accounting should be dropped from annotations")
	}
}

func TestIntegrationMessageTextTruncation(t *testing.T) {
	m := newMockAPI(t)
	ts := newTestToolset(t, m) // MaxTextBytes = 50, body is 100 'z'
	res, _, err := ts.messageText(context.Background(), nil, messageTextInput{ID: 42})
	out := decodeResult(t, res, err)
	if out["truncated"] != true {
		t.Errorf("expected truncated=true, got %+v", out)
	}
	if len(out["text"].(string)) != 50 {
		t.Errorf("text len = %d, want 50", len(out["text"].(string)))
	}
	if out["next_offset"].(float64) != 50 {
		t.Errorf("next_offset = %v, want 50", out["next_offset"])
	}
	if out["total_bytes"].(float64) != 100 {
		t.Errorf("total_bytes = %v, want 100", out["total_bytes"])
	}

	// Continue from the offset — the remaining 50 bytes, not truncated.
	res2, _, err2 := ts.messageText(context.Background(), nil, messageTextInput{ID: 42, Offset: 50})
	out2 := decodeResult(t, res2, err2)
	if _, ok := out2["truncated"]; ok {
		t.Errorf("second slice should not be truncated: %+v", out2)
	}
	if len(out2["text"].(string)) != 50 {
		t.Errorf("second slice len = %d, want 50", len(out2["text"].(string)))
	}
}

func TestIntegrationSearch(t *testing.T) {
	m := newMockAPI(t)
	ts := newTestToolset(t, m)
	res, _, err := ts.search(context.Background(), nil, searchInput{Q: "invoice", Mailbox: "jdoe", Since: "1w"})
	out := decodeResult(t, res, err)
	if m.lastQ.Get("q") != "invoice" {
		t.Errorf("q = %q", m.lastQ.Get("q"))
	}
	if m.lastQ.Get("mailbox") != "jdoe" {
		t.Errorf("mailbox = %q", m.lastQ.Get("mailbox"))
	}
	if got := m.lastQ.Get("since"); !strings.HasSuffix(got, "Z") {
		t.Errorf("since = %q, want converted RFC 3339", got)
	}
	if len(out["messages"].([]any)) != 1 {
		t.Errorf("want 1 search hit, got %v", out["messages"])
	}
}

func TestIntegrationSearchFolderRequiresMailbox(t *testing.T) {
	m := newMockAPI(t)
	ts := newTestToolset(t, m)
	res, _, err := ts.search(context.Background(), nil, searchInput{Q: "x", Folder: "INBOX"})
	out := decodeResult(t, res, err)
	if _, ok := out["error"]; !ok {
		t.Errorf("folder without mailbox should be a correctable error, got %+v", out)
	}
}

func TestIntegrationAnnotateRoundTrip(t *testing.T) {
	m := newMockAPI(t)
	ts := newTestToolset(t, m)
	res, _, err := ts.annotate(context.Background(), nil, annotateInput{
		ID: 42, Model: "claude-opus-4-8", Tags: []string{"receipts"},
		Category: "finance", Summary: "An invoice.",
	})
	out := decodeResult(t, res, err)
	if out["ok"] != true {
		t.Errorf("annotate result = %+v, want ok:true", out)
	}
	if m.lastPath != "/v1/messages/42/annotation" {
		t.Errorf("path = %q", m.lastPath)
	}
	if m.lastBody["model"] != "claude-opus-4-8" {
		t.Errorf("body model = %v", m.lastBody["model"])
	}
	if m.lastBody["category"] != "finance" || m.lastBody["summary"] != "An invoice." {
		t.Errorf("body = %+v", m.lastBody)
	}
	tags := m.lastBody["tags"].([]any)
	if len(tags) != 1 || tags[0] != "receipts" {
		t.Errorf("body tags = %v", tags)
	}
}

func TestIntegrationAnnotateRequiresContent(t *testing.T) {
	m := newMockAPI(t)
	ts := newTestToolset(t, m)
	res, _, err := ts.annotate(context.Background(), nil, annotateInput{ID: 42, Model: "m"})
	out := decodeResult(t, res, err)
	if _, ok := out["error"]; !ok {
		t.Errorf("annotate with no tags/category/summary should error, got %+v", out)
	}
	// It must not have reached the server.
	if m.lastPath == "/v1/messages/42/annotation" {
		t.Error("annotate should have failed before calling epistula-api")
	}
}
