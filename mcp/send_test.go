// The send tool: a third credential, the client-facing hints that make an MCP
// client ask before calling it, and byte-exact forwarding of the message.
//
// Sending is the one capability whose effect leaves the host and cannot be
// undone, so it gets the annotate tool's guarantees and more: the tool does
// not exist without send_token, the request rides that token and no other,
// and a config that reuses a read or annotate token for it is refused.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newSendToolset(t *testing.T, m *mockAPI) *toolset {
	t.Helper()
	ts := newTestToolset(t, m)
	ts.cfg.SendToken = "test-send-token"
	ts.s = newClientWithToken(ts.cfg, ts.cfg.SendToken)
	return ts
}

// listTools returns the tools a real MCP client sees, keyed by name.
func listTools(t *testing.T, ts *toolset) map[string]*mcp.Tool {
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
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	out := make(map[string]*mcp.Tool, len(res.Tools))
	for _, tool := range res.Tools {
		out[tool.Name] = tool
	}
	return out
}

func TestSendToolAbsentWithoutSendToken(t *testing.T) {
	m := newMockAPI(t)
	tools := listTools(t, newTestToolset(t, m)) // read + annotate tokens, no send token
	if _, ok := tools["send"]; ok {
		t.Fatal("send is advertised with no send_token configured")
	}
	ts := newTestToolset(t, m)
	res, _, err := ts.send(context.Background(), nil, sendInput{Mailbox: "jdoe", Message: "x"})
	if out := decodeResult(t, res, err); out["error"] == nil {
		t.Errorf("send with no send client = %+v, want a correctable error", out)
	}
	if m.lastPath != "" {
		t.Errorf("send reached epistula-api (%s) with no send credential", m.lastPath)
	}
}

func TestSendToolRegisteredWithHints(t *testing.T) {
	m := newMockAPI(t)
	tools := listTools(t, newSendToolset(t, m))
	tool, ok := tools["send"]
	if !ok {
		t.Fatal("send tool missing with a send_token configured")
	}
	if len(tools) != readOnlyToolCount+2 {
		t.Errorf("tool count = %d, want %d", len(tools), readOnlyToolCount+2)
	}
	a := tool.Annotations
	if a == nil || a.ReadOnlyHint || a.IdempotentHint || a.DestructiveHint == nil || !*a.DestructiveHint ||
		a.OpenWorldHint == nil || !*a.OpenWorldHint {
		t.Errorf("send annotations = %+v, want destructive, open-world, not read-only, not idempotent", a)
	}
	if !strings.Contains(tool.Description, "SEND A REAL EMAIL") || !strings.Contains(tool.Description, "do NOT send it again") {
		t.Errorf("send description does not warn the model: %q", tool.Description)
	}
}

func TestSendForwardsExactBytesOnSendToken(t *testing.T) {
	m := newMockAPI(t)
	ts := newSendToolset(t, m)
	msg := "From: jdoe@x.invalid\nTo: carol@elsewhere.invalid\nDate: Sat, 24 Oct 2026 12:00:00 +0000\n\n.dot line\n"
	res, _, err := ts.send(context.Background(), nil, sendInput{
		Mailbox: "jdoe", Message: msg,
		EnvelopeFrom: " bounces@x.invalid ", Recipients: []string{"carol@elsewhere.invalid", "blind@elsewhere.invalid"},
	})
	out := decodeResult(t, res, err)
	if out["queue_id"] != "4F2D61A3B7" {
		t.Fatalf("send result = %+v", out)
	}
	if m.lastAuth != "Bearer test-send-token" {
		t.Errorf("send Authorization = %q, want the send token and only the send token", m.lastAuth)
	}
	if m.lastPath != "/v1/mailboxes/jdoe/submissions" {
		t.Errorf("path = %q", m.lastPath)
	}
	got, err := base64.StdEncoding.DecodeString(m.lastBody["message_base64"].(string))
	if err != nil || string(got) != msg {
		t.Errorf("forwarded message = %q (%v), want the input unchanged", got, err)
	}
	if m.lastBody["envelope_from"] != "bounces@x.invalid" {
		t.Errorf("envelope_from = %v", m.lastBody["envelope_from"])
	}
	if r := m.lastBody["recipients"].([]any); len(r) != 2 || r[1] != "blind@elsewhere.invalid" {
		t.Errorf("recipients = %v", r)
	}

	// Octets that are not UTF-8 survive as message_base64, including when
	// the model wraps the base64 across lines.
	latin1 := []byte("From: jdoe@x.invalid\r\nDate: Sat, 24 Oct 2026 12:00:00 +0000\r\n\r\ncaf\xe9\r\n")
	enc := base64.StdEncoding.EncodeToString(latin1)
	wrapped := enc[:20] + "\n" + enc[20:]
	res, _, err = ts.send(context.Background(), nil, sendInput{Mailbox: "jdoe", MessageBase64: wrapped})
	if out := decodeResult(t, res, err); out["queue_id"] == nil {
		t.Fatalf("send result = %+v", out)
	}
	got, _ = base64.StdEncoding.DecodeString(m.lastBody["message_base64"].(string))
	if !bytes.Equal(got, latin1) {
		t.Errorf("forwarded octets = %q, want %q", got, latin1)
	}
	if _, ok := m.lastBody["recipients"]; ok {
		t.Error("absent recipients were sent as an empty list instead of being left to the API's default")
	}
}

func TestSendRefusesBadInputLocally(t *testing.T) {
	m := newMockAPI(t)
	ts := newSendToolset(t, m)
	for _, in := range []sendInput{
		{Message: "x"},
		{Mailbox: "jdoe"},
		{Mailbox: "jdoe", Message: "x", MessageBase64: "eA=="},
		{Mailbox: "jdoe", MessageBase64: "not base64!"},
	} {
		res, _, err := ts.send(context.Background(), nil, in)
		if out := decodeResult(t, res, err); out["error"] == nil {
			t.Errorf("send(%+v) = %+v, want a correctable error", in, out)
		}
	}
	if m.lastPath != "" {
		t.Errorf("an invalid send reached epistula-api: %s", m.lastPath)
	}
}

func TestSendReportsPossiblySent(t *testing.T) {
	m := newMockAPI(t)
	ts := newSendToolset(t, m)
	res, _, err := ts.send(context.Background(), nil, sendInput{Mailbox: "unknown", Message: "x"})
	out := decodeResult(t, res, err)
	e, _ := out["error"].(map[string]any)
	if e == nil || !strings.Contains(e["detail"].(string), "may have been sent") {
		t.Errorf("outcome-unknown result = %+v", out)
	}

	// A client deadline that expires before epistula-api answers is just as
	// ambiguous, and must say so.
	cfg := ts.cfg
	cfg.RequestTimeout = 100 * time.Millisecond
	ts.s = newClientWithToken(cfg, cfg.SendToken)
	res, _, err = ts.send(context.Background(), nil, sendInput{Mailbox: "slow", Message: "x"})
	out = decodeResult(t, res, err)
	e, _ = out["error"].(map[string]any)
	if e == nil || !strings.Contains(e["message"].(string), "MAY have been sent") {
		t.Errorf("timed-out send = %+v, want it to say the message may have been sent", out)
	}
}

func TestSendPathEscapesMailbox(t *testing.T) {
	m := newMockAPI(t)
	ts := newSendToolset(t, m)
	res, _, err := ts.send(context.Background(), nil, sendInput{Mailbox: "../jdoe", Message: "x"})
	decodeResult(t, res, err)
	if strings.Contains(m.lastRawPath, "/../") || m.lastPath == "/v1/mailboxes/jdoe/submissions" {
		t.Errorf("mailbox reached the server as path structure: %q", m.lastRawPath)
	}
}

func TestConfigSendTokenSeparation(t *testing.T) {
	for name, body := range map[string]string{
		"same as token": `
[mailapi]
base_url   = "http://127.0.0.1:8784"
token      = "same-secret-value"
send_token = "same-secret-value"
`,
		"same as annotate_token": `
[mailapi]
base_url       = "http://127.0.0.1:8784"
token          = "read-secret"
annotate_token = "same-secret-value"
send_token     = "same-secret-value"
`,
	} {
		t.Run(name, func(t *testing.T) {
			clearEnv(t)
			_, err := loadConfig(writeTempConfig(t, body))
			if err == nil {
				t.Fatal("a send_token shared with another credential was accepted")
			}
			if strings.Contains(err.Error(), "same-secret-value") {
				t.Errorf("error leaks the secret: %v", err)
			}
		})
	}

	clearEnv(t)
	path := writeTempConfig(t, `
[mailapi]
base_url   = "http://127.0.0.1:8784"
token      = "read-secret"
send_token = "file-send-secret"
`)
	cfg, err := loadConfig(path)
	if err != nil || cfg.SendToken != "file-send-secret" || cfg.AnnotateToken != "" {
		t.Fatalf("send_token from file: cfg=%+v err=%v", cfg.SendToken, err)
	}
	t.Setenv("MAIL_MCP_SEND_TOKEN", "env-send-secret")
	if cfg, err = loadConfig(path); err != nil || cfg.SendToken != "env-send-secret" || cfg.Token != "read-secret" {
		t.Fatalf("env override: send=%q read=%q err=%v", cfg.SendToken, cfg.Token, err)
	}
}
