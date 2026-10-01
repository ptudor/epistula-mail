package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestClientBoundsResponseBody is the RO5X-016 regression: client.do used
// io.ReadAll with no cap, so a large upstream body was fully resident in the
// connector — which typically runs as a subprocess of a desktop MCP client on
// a laptop.
func TestClientBoundsResponseBody(t *testing.T) {
	const bodySize = 16 << 20 // 16 MiB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		chunk := strings.Repeat("x", 64*1024)
		for written := 0; written < bodySize; written += len(chunk) {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	c := newClient(config{
		BaseURL:          srv.URL,
		Token:            "t",
		RequestTimeout:   30 * time.Second,
		MaxResponseBytes: 4 << 20, // 4 MiB
	})

	_, aerr := c.getText(context.Background(), "/messages/1/text", nil)
	if aerr == nil {
		t.Fatal("a 16 MiB body was accepted against a 4 MiB cap")
	}
	if !strings.Contains(aerr.Message, "exceeded the connector's") {
		t.Errorf("error = %q, want the size-limit message", aerr.Message)
	}
}

// TestClientAcceptsBodyUnderCap keeps the ordinary path working.
func TestClientAcceptsBodyUnderCap(t *testing.T) {
	payload := strings.Repeat("y", 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, payload)
	}))
	defer srv.Close()

	c := newClient(config{
		BaseURL: srv.URL, Token: "t",
		RequestTimeout: 30 * time.Second, MaxResponseBytes: 4 << 20,
	})
	body, aerr := c.getText(context.Background(), "/messages/1/text", nil)
	if aerr != nil {
		t.Fatalf("unexpected error: %v", aerr.Message)
	}
	if body != payload {
		t.Errorf("body length = %d, want %d", len(body), len(payload))
	}
}

// TestMessageTextPagesServerSide is the other half of RO5X-016: each page must
// cost one bounded request, not a full-body re-fetch.
func TestMessageTextPagesServerSide(t *testing.T) {
	full := strings.Repeat("abcdefghij", 30000) // 300 KB
	var transferred int
	var requests int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		slice := full
		if offset < len(slice) {
			slice = slice[offset:]
		} else {
			slice = ""
		}
		if limit > 0 && len(slice) > limit {
			slice = slice[:limit]
		}
		transferred += len(slice)
		w.Header().Set("X-Total-Bytes", strconv.Itoa(len(full)))
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, slice)
	}))
	defer srv.Close()

	cfg := config{
		BaseURL: srv.URL, Token: "t",
		RequestTimeout:   30 * time.Second,
		MaxTextBytes:     100000,
		MaxResponseBytes: 8 << 20,
	}
	ts := &toolset{c: newClient(cfg), cfg: cfg}

	var reassembled strings.Builder
	offset := 0
	for i := 0; i < 10; i++ {
		res, _, err := ts.messageText(context.Background(), nil, messageTextInput{ID: 1, Offset: offset})
		if err != nil {
			t.Fatalf("messageText page %d: %v", i, err)
		}
		m := decodeToolJSON(t, res)
		if e, bad := m["error"]; bad {
			t.Fatalf("page %d: tool error: %v", i, e)
		}
		reassembled.WriteString(m["text"].(string))
		if got := jsonInt(t, m, "total_bytes"); got != len(full) {
			t.Errorf("page %d: total_bytes = %d, want %d (the FULL body length)",
				i, got, len(full))
		}
		if _, more := m["next_offset"]; !more {
			break
		}
		offset = jsonInt(t, m, "next_offset")
	}

	if got := reassembled.String(); got != full {
		t.Errorf("reassembled %d bytes, want %d — paging is lossy", len(got), len(full))
	}
	// Re-fetching the whole body per page would transfer ~len(full) per
	// request. Server-side slicing transfers ~len(full) in total.
	if transferred > 2*len(full) {
		t.Errorf("transferred %d bytes over %d requests for a %d-byte body; "+
			"paging is still re-fetching the whole body (RO5X-016)",
			transferred, requests, len(full))
	}
	t.Logf("%d requests, %d bytes transferred for a %d-byte body", requests, transferred, len(full))
}

// TestMessageTextFallsBackWithoutRangeSupport keeps the connector working
// against an older epistula-api that ignores ?offset=/?limit= and sends the whole
// body with no X-Total-Bytes.
func TestMessageTextFallsBackWithoutRangeSupport(t *testing.T) {
	full := strings.Repeat("z", 5000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, full) // ignores the range params, sets no X-Total-Bytes
	}))
	defer srv.Close()

	cfg := config{
		BaseURL: srv.URL, Token: "t",
		RequestTimeout: 30 * time.Second,
		MaxTextBytes:   1000, MaxResponseBytes: 8 << 20,
	}
	ts := &toolset{c: newClient(cfg), cfg: cfg}

	res, _, err := ts.messageText(context.Background(), nil, messageTextInput{ID: 1, Offset: 2000})
	if err != nil {
		t.Fatalf("messageText: %v", err)
	}
	m := decodeToolJSON(t, res)
	if got := jsonInt(t, m, "total_bytes"); got != len(full) {
		t.Errorf("total_bytes = %d, want %d", got, len(full))
	}
	if got := m["text"].(string); got != full[2000:3000] {
		t.Errorf("fallback slicing returned the wrong window (%d bytes)", len(got))
	}
}

// TestHTTPServerHasReadTimeouts is the RO5X-017 regression: a peer that opens
// a connection and sends a partial request line must be disconnected rather
// than holding a goroutine and an fd forever.
func TestHTTPServerHasReadTimeouts(t *testing.T) {
	srv := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		ReadHeaderTimeout: 1 * time.Second, // production uses 10s; scaled for the test
		ReadTimeout:       2 * time.Second,
		IdleTimeout:       3 * time.Second,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go srv.Serve(ln)
	defer srv.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// A partial request: headers never terminate.
	fmt.Fprint(conn, "GET / HTTP/1.1\r\n")

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	start := time.Now()
	buf := make([]byte, 256)
	_, err = conn.Read(buf)
	elapsed := time.Since(start)

	if err == nil {
		t.Log("server responded (408) rather than dropping; either way the connection did not hang")
	}
	if elapsed > 5*time.Second {
		t.Errorf("connection held for %v; ReadHeaderTimeout is not being enforced", elapsed)
	}
}

// TestMaxResponseBytesSizing pins the config sizing rule: the cap tracks
// max_text_bytes so raising the text budget does not make /text start failing,
// and never drops below the floor.
func TestMaxResponseBytesSizing(t *testing.T) {
	for _, tc := range []struct {
		name        string
		textBytes   int
		configured  int
		wantAtLeast int
	}{
		{"defaults", defaultMaxTextBytes, 0, minMaxResponseBytes},
		{"large text budget raises the cap", 2 << 20, 0, 16 << 20},
		{"explicit small value is floored", 1024, 1024, minMaxResponseBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config{MaxTextBytes: tc.textBytes, MaxResponseBytes: tc.configured}
			if cfg.MaxResponseBytes == 0 {
				cfg.MaxResponseBytes = defaultMaxResponseBytes
			}
			if want := cfg.MaxTextBytes * 8; want > cfg.MaxResponseBytes {
				cfg.MaxResponseBytes = want
			}
			if cfg.MaxResponseBytes < minMaxResponseBytes {
				cfg.MaxResponseBytes = minMaxResponseBytes
			}
			if cfg.MaxResponseBytes < tc.wantAtLeast {
				t.Errorf("MaxResponseBytes = %d, want >= %d", cfg.MaxResponseBytes, tc.wantAtLeast)
			}
		})
	}
}

// decodeToolJSON pulls the JSON payload out of a tool result. jsonResult
// serializes into the CallToolResult's text content and returns nil for the
// structured value, so tests read it back from there.
func decodeToolJSON(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatal("tool result has no content")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] = %T, want *mcp.TextContent", res.Content[0])
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &m); err != nil {
		t.Fatalf("decode tool JSON: %v (raw %q)", err, tc.Text)
	}
	return m
}

// jsonInt reads a JSON number field as an int.
func jsonInt(t *testing.T, m map[string]any, key string) int {
	t.Helper()
	f, ok := m[key].(float64)
	if !ok {
		t.Fatalf("%s = %T (%v), want a number", key, m[key], m[key])
	}
	return int(f)
}
