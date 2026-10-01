package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// apiRoutePattern is epistula-api's real folder-messages route, copied verbatim
// from api/serve.go. The defect this file pins lives in the
// interaction between the connector's URL construction and Go's ServeMux, so
// the test has to route through the same mux with the same pattern rather than
// a hand-written handler that would not clean anything.
const apiRoutePattern = "GET /v1/mailboxes/{mailbox}/folders/{rest...}"

// folderEchoServer stands in for epistula-api: it reconstructs the folder exactly
// as handlers.go does and echoes it back, recording whether the request that
// reached the handler was the one that was sent.
type folderEchoServer struct {
	*httptest.Server
	redirects []string // Location headers the mux emitted, in order
}

func newFolderEchoServer(t *testing.T) *folderEchoServer {
	t.Helper()
	f := &folderEchoServer{}
	mux := http.NewServeMux()
	mux.HandleFunc(apiRoutePattern, func(w http.ResponseWriter, r *http.Request) {
		folder, ok := strings.CutSuffix(r.PathValue("rest"), "/messages")
		if !ok || folder == "" {
			http.Error(w, `{"title":"not found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mailbox":  r.PathValue("mailbox"),
			"folder":   folder,
			"messages": []any{},
		})
	})
	// Record any redirect the mux produces on the way out. A 3xx here IS the
	// defect: it means routing decided the requested path was not canonical.
	rec := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		mux.ServeHTTP(sw, r)
		if sw.status >= 300 && sw.status < 400 {
			f.redirects = append(f.redirects, sw.location)
		}
	})
	f.Server = httptest.NewServer(rec)
	t.Cleanup(f.Close)
	return f
}

type statusWriter struct {
	http.ResponseWriter
	status   int
	location string
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.location = w.Header().Get("Location")
	w.ResponseWriter.WriteHeader(code)
}

// TestFolderURLSurvivesRouting is the RA6X-057 regression.
//
// Folder names are database strings; the connector used to lay their slashes
// into the URL as real path separators and escape each piece with
// url.PathEscape, which leaves "." alone. Go's ServeMux cleans the request path
// and 301s to the cleaned form, so `A/../B` reached the handler as `B` and
// `A//B` as `A/B` — and the connector followed the redirect, because it is
// same-origin and RA6X-039's policy permits that, returning another folder's
// messages with nothing to show a substitution had happened.
func TestFolderURLSurvivesRouting(t *testing.T) {
	folders := []string{
		".",
		"..",
		"A/../B",
		"A//B",
		"/Leading",
		"Trailing/",
		"%2e",
		"%2F",
		"Ünïcødé/受信箱",
		"Archive/2026",
		"INBOX",
		"dot.in.name",
		"...",
		"a/./b",
	}

	for _, folder := range folders {
		t.Run(folder, func(t *testing.T) {
			srv := newFolderEchoServer(t)
			ts := &toolset{
				cfg: config{MaxLimit: 100},
				c: newClientWithToken(config{
					BaseURL:          srv.URL,
					RequestTimeout:   requestTimeoutForTest,
					MaxResponseBytes: 1 << 20,
				}, "test-token"),
			}

			res, _, err := ts.messages(context.Background(), nil, messagesInput{
				Mailbox: "alice",
				Folder:  folder,
			})
			out := decodeResult(t, res, err)
			if e := toolErrorText(out); e != "" {
				t.Fatalf("messages(%q) returned a tool error: %s", folder, e)
			}
			if len(srv.redirects) != 0 {
				t.Errorf("routing normalized the request for %q and redirected to %v",
					folder, srv.redirects)
			}
			got, _ := out["folder"].(string)
			if got != folder {
				t.Errorf("handler read folder %q, want %q", got, folder)
			}
		})
	}
}

// TestFolderEchoMismatchIsRefused pins the second half: if something between
// the connector and the database does rewrite the path, the connector must say
// so rather than present another folder's mail as the requested folder's.
func TestFolderEchoMismatchIsRefused(t *testing.T) {
	// A server that always answers for "Elsewhere", whatever was asked.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mailbox": "alice", "folder": "Elsewhere", "messages": []any{},
		})
	}))
	defer srv.Close()

	ts := &toolset{
		cfg: config{MaxLimit: 100},
		c: newClientWithToken(config{
			BaseURL:          srv.URL,
			RequestTimeout:   requestTimeoutForTest,
			MaxResponseBytes: 1 << 20,
		}, "test-token"),
	}
	res, _, err := ts.messages(context.Background(), nil, messagesInput{
		Mailbox: "alice", Folder: "Archive/2026",
	})
	out := decodeResult(t, res, err)
	text := toolErrorText(out)
	if text == "" {
		t.Fatal("a response for a different folder was presented as the requested folder's contents")
	}
	if !strings.Contains(text, "Elsewhere") || !strings.Contains(text, "Archive/2026") {
		t.Errorf("the error does not name both folders: %s", text)
	}

	// A response with no folder at all is refused too — the check cannot be
	// skipped just because the upstream omitted the field.
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{}})
	}))
	defer silent.Close()
	ts.c = newClientWithToken(config{
		BaseURL:          silent.URL,
		RequestTimeout:   requestTimeoutForTest,
		MaxResponseBytes: 1 << 20,
	}, "test-token")
	res, _, err = ts.messages(context.Background(), nil, messagesInput{
		Mailbox: "alice", Folder: "Archive/2026",
	})
	out = decodeResult(t, res, err)
	if toolErrorText(out) == "" {
		t.Error("a response that names no folder was accepted as folder contents")
	}
}

// TestEscapePathDataLeavesNoStructure pins the encoder itself: no output may
// contain a path separator or a bare dot, so no output can become a path
// operator or an empty segment.
func TestEscapePathDataLeavesNoStructure(t *testing.T) {
	for _, in := range []string{".", "..", "A/../B", "A//B", "/x", "x/", "%2e", "a.b", "", "~ok-1_2"} {
		got := escapePathData(in)
		if strings.ContainsAny(got, "/.") {
			t.Errorf("escapePathData(%q) = %q; it still carries path structure", in, got)
		}
	}
	// Ordinary names stay recognisable, which is the only reason not to
	// hex-encode everything.
	if got := escapePathData("Archive2026"); got != "Archive2026" {
		t.Errorf("escapePathData mangled an ordinary name: %q", got)
	}
}

// requestTimeoutForTest is the client deadline these tests run under: long
// enough that a loopback round trip never races it, short enough that a wedged
// server fails the test rather than hanging the package.
const requestTimeoutForTest = 15 * time.Second

// toolErrorText returns the `error` field of a tool result payload, or "" when
// the call succeeded. Tool errors are data, not protocol faults, so this is how
// a test asks "did the connector refuse".
func toolErrorText(out map[string]any) string {
	switch e := out["error"].(type) {
	case string:
		return e
	case *apiError:
		return e.Error()
	case map[string]any:
		b, _ := json.Marshal(e)
		return string(b)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", e)
	}
}
