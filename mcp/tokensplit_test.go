// Read/write credential split.
//
// Every message body is attacker-controlled input: anyone who can send mail can
// put text in it aimed at the model that reads it. So the session exposed to
// that content must not be the session that can write.
//
// Two independent properties are tested here, because either alone is weaker
// than it looks:
//
//  1. With no annotate_token, the `annotate` tool is not registered — a real MCP
//     client never sees it, so a hijacked model cannot call it at all. A 403
//     from epistula-api would also block the write, but the tool would still be
//     reachable and the attempt would still be made.
//  2. With an annotate_token, the write carries THAT token and reads carry the
//     read token. Mint the read token without write_annotation and epistula-api
//     enforces the split in SQL, independently of this proxy behaving.
package main

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// listToolNames connects a real MCP client over the in-memory transport pair and
// asks for the tool list, so the assertion is about what a client actually sees
// rather than about our own bookkeeping.
func listToolNames(t *testing.T, ts *toolset) map[string]bool {
	t.Helper()
	ctx := context.Background()

	server := mcp.NewServer(&mcp.Implementation{Name: "mail", Version: "test"}, nil)
	ts.register(server)

	clientT, serverT := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer ss.Close()

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).
		Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := make(map[string]bool, len(res.Tools))
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	return names
}

const readOnlyToolCount = 6 // mailboxes, folders, messages, message, message_text, search

func TestReadOnlyByDefaultOmitsAnnotateTool(t *testing.T) {
	m := newMockAPI(t)
	cfg := config{BaseURL: m.server.URL, Token: "read-token", RequestTimeout: defaultRequestTimeout}
	// w deliberately nil: this is the default posture with no annotate_token.
	names := listToolNames(t, &toolset{c: newClient(cfg), cfg: cfg})

	if names["annotate"] {
		t.Error("annotate tool is advertised with no annotate_token configured; " +
			"a model reading hostile mail must not be able to reach a write tool")
	}
	if len(names) != readOnlyToolCount {
		t.Errorf("tool count = %d (%v), want %d read tools", len(names), names, readOnlyToolCount)
	}
	for _, want := range []string{"mailboxes", "folders", "messages", "message", "message_text", "search"} {
		if !names[want] {
			t.Errorf("read tool %q missing — the split must not cost read capability", want)
		}
	}
}

func TestAnnotateTokenRegistersAnnotateTool(t *testing.T) {
	m := newMockAPI(t)
	names := listToolNames(t, newTestToolset(t, m))

	if !names["annotate"] {
		t.Error("annotate tool missing even though an annotate_token was configured")
	}
	if len(names) != readOnlyToolCount+1 {
		t.Errorf("tool count = %d (%v), want %d", len(names), names, readOnlyToolCount+1)
	}
}

// TestAnnotateUsesWriteTokenNotReadToken is the point of the whole change: the
// write must not ride the credential that read the hostile content.
func TestAnnotateUsesWriteTokenNotReadToken(t *testing.T) {
	m := newMockAPI(t)
	ts := newTestToolset(t, m)
	ctx := context.Background()

	// A read carries the read token.
	if _, _, err := ts.mailboxes(ctx, nil, mailboxesInput{}); err != nil {
		t.Fatalf("mailboxes: %v", err)
	}
	if got, want := m.lastAuth, "Bearer test-token"; got != want {
		t.Errorf("read Authorization = %q, want %q", got, want)
	}

	// The write carries the annotate token.
	res, _, err := ts.annotate(ctx, nil, annotateInput{
		ID: 42, Model: "claude-opus-4-8", Tags: []string{"receipts"},
	})
	if out := decodeResult(t, res, err); out["ok"] != true {
		t.Fatalf("annotate result = %+v, want ok:true", out)
	}
	if got, want := m.lastAuth, "Bearer test-annotate-token"; got != want {
		t.Errorf("annotate Authorization = %q, want %q — the write must use the "+
			"annotate token, not the token that read message content", got, want)
	}
}

// TestAnnotateWithoutWriteClientIsCorrectableError covers the in-handler guard.
// register() makes this unreachable today; the guard exists so that a later
// change registering the tool unconditionally yields a tool error rather than a
// nil dereference in the one path that writes.
func TestAnnotateWithoutWriteClientIsCorrectableError(t *testing.T) {
	m := newMockAPI(t)
	cfg := config{BaseURL: m.server.URL, Token: "read-token", RequestTimeout: defaultRequestTimeout}
	ts := &toolset{c: newClient(cfg), cfg: cfg} // no w

	res, _, err := ts.annotate(context.Background(), nil,
		annotateInput{ID: 42, Model: "m", Tags: []string{"x"}})
	out := decodeResult(t, res, err)
	if _, ok := out["error"]; !ok {
		t.Errorf("annotate with no write client = %+v, want a correctable error", out)
	}
	if m.lastPath == "/v1/messages/42/annotation" {
		t.Error("annotate reached epistula-api with no write credential configured")
	}
}

func TestConfigRejectsIdenticalTokens(t *testing.T) {
	path := writeTempConfig(t, `
[mailapi]
base_url       = "http://127.0.0.1:8784"
token          = "same-secret-value"
annotate_token = "same-secret-value"
`)
	_, err := loadConfig(path)
	if err == nil {
		t.Fatal("identical token and annotate_token accepted; the config would claim a " +
			"read/write split while the read tools still hold write scope")
	}
	if strings.Contains(err.Error(), "same-secret-value") {
		t.Errorf("error leaks the secret: %v", err)
	}
}

func TestConfigAnnotateTokenFromFileAndEnv(t *testing.T) {
	path := writeTempConfig(t, `
[mailapi]
base_url       = "http://127.0.0.1:8784"
token          = "read-secret"
annotate_token = "file-write-secret"
`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.AnnotateToken != "file-write-secret" {
		t.Errorf("AnnotateToken from file = %q", cfg.AnnotateToken)
	}

	t.Setenv("MAIL_MCP_ANNOTATE_TOKEN", "env-write-secret")
	cfg, err = loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig with env: %v", err)
	}
	if cfg.AnnotateToken != "env-write-secret" {
		t.Errorf("env override = %q, want env-write-secret", cfg.AnnotateToken)
	}
	if cfg.Token != "read-secret" {
		t.Errorf("read token disturbed by the annotate env override = %q", cfg.Token)
	}
}

// TestConfigNoAnnotateTokenIsValid pins the default: omitting the second
// credential is a supported, read-only configuration, not an error.
func TestConfigNoAnnotateTokenIsValid(t *testing.T) {
	path := writeTempConfig(t, `
[mailapi]
base_url = "http://127.0.0.1:8784"
token    = "read-secret"
`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.AnnotateToken != "" {
		t.Errorf("AnnotateToken = %q, want empty", cfg.AnnotateToken)
	}
}
