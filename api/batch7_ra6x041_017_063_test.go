package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ---- RA6X-041: annotation PUT body validation ----------------------------

// annotationPutRaw sends a raw body to the annotation endpoint, bypassing any
// client-side marshalling, and returns the status.
func (f *apiFixture) annotationPutRaw(t *testing.T, id int64, token, body string) int {
	t.Helper()
	resp := f.do(http.MethodPut, fmt.Sprintf("/v1/messages/%d/annotation", id), token, strings.NewReader(body))
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// annotationSummary reads back the stored sidecar summary for (message, model).
func (f *apiFixture) annotationSummary(t *testing.T, id int64, model string) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var summary *string
	err := f.pool.QueryRow(ctx,
		`SELECT summary FROM message_annotations WHERE message_id = $1 AND model = $2`, id, model).Scan(&summary)
	if err != nil {
		return "", false
	}
	if summary == nil {
		return "", true
	}
	return *summary, true
}

// TestAnnotationPutRejectsUnstorableAndTrailingJSON is the RA6X-041
// regression.
//
// A JSON string containing U+0000 is syntactically valid and passed every
// length and emptiness check, then failed at the INSERT — a 500 that told the
// caller the server was broken about a request the server should have refused.
// And json.Decoder reads ONE value and stops, so `{...}{...}` committed the
// first object and silently discarded the rest, answering 204 for work that
// was half done.
func TestAnnotationPutRejectsUnstorableAndTrailingJSON(t *testing.T) {
	f := newAPIFixture(t)
	id := f.aliceMsgIDs[0]
	const model = "guard-model"

	// A good annotation first, so every rejection below can be checked against
	// an existing sidecar that must survive untouched.
	good := `{"model":"` + model + `","tags":["ok"],"category":"other","summary":"the original"}`
	if code := f.annotationPutRaw(t, id, f.classifierToken, good); code != http.StatusNoContent {
		t.Fatalf("valid PUT = %d, want 204", code)
	}

	for name, body := range map[string]string{
		"NUL in model":    `{"model":"bad\u0000model","tags":[],"category":null,"summary":null}`,
		"NUL in a tag":    `{"model":"` + model + `","tags":["fine","bad\u0000tag"],"category":null,"summary":null}`,
		"NUL in category": `{"model":"` + model + `","tags":[],"category":"bad\u0000cat","summary":null}`,
		"NUL in summary":  `{"model":"` + model + `","tags":[],"category":null,"summary":"bad\u0000summary"}`,
		"two JSON objects": `{"model":"` + model + `","tags":[],"category":null,"summary":"first"}` +
			`{"model":"` + model + `","tags":[],"category":null,"summary":"second"}`,
		"trailing garbage": `{"model":"` + model + `","tags":[],"category":null,"summary":"first"} not json`,
		"trailing array":   `{"model":"` + model + `","tags":[],"category":null,"summary":"first"}[1,2,3]`,
	} {
		t.Run(name, func(t *testing.T) {
			code := f.annotationPutRaw(t, id, f.classifierToken, body)
			if code == http.StatusNoContent {
				t.Fatalf("%s was accepted (204)", name)
			}
			if code < 400 || code >= 500 {
				t.Fatalf("%s = %d, want a 4xx client error", name, code)
			}
			// The previous sidecar is untouched.
			if got, ok := f.annotationSummary(t, id, model); !ok || got != "the original" {
				t.Errorf("%s changed the stored annotation to %q (present=%v)", name, got, ok)
			}
		})
	}

	// Trailing whitespace after one value is legal and must still be accepted.
	ws := `{"model":"` + model + `","tags":[],"category":null,"summary":"with whitespace"}` + "\n\n   \t\r\n"
	if code := f.annotationPutRaw(t, id, f.classifierToken, ws); code != http.StatusNoContent {
		t.Errorf("a body with trailing whitespace = %d, want 204", code)
	}
	if got, _ := f.annotationSummary(t, id, model); got != "with whitespace" {
		t.Errorf("summary = %q, want the whitespace-suffixed body to have been stored", got)
	}

	// The body cap applies to the WHOLE body, trailing input included: a small
	// valid object followed by a flood of junk is refused, not committed.
	huge := `{"model":"` + model + `","tags":[],"category":null,"summary":"x"}` +
		strings.Repeat(" ", int(f.srv.cfg.Limits.MaxAnnotationBytes)+1024)
	if code := f.annotationPutRaw(t, id, f.classifierToken, huge); code == http.StatusNoContent {
		t.Error("a body far past the configured cap was accepted")
	}
	if got, _ := f.annotationSummary(t, id, model); got != "with whitespace" {
		t.Errorf("an over-cap body changed the stored annotation to %q", got)
	}

	// A nullable annotation still works.
	nullable := `{"model":"nullable-model","tags":[],"category":null,"summary":null}`
	if code := f.annotationPutRaw(t, id, f.classifierToken, nullable); code != http.StatusNoContent {
		t.Errorf("a valid nullable annotation = %d, want 204", code)
	}
}

// ---- RA6X-017: annotation filters are content ----------------------------

// TestAnnotationFiltersRequireContentPermission is the RA6X-017 regression.
//
// Annotation values require read_content when RETURNED, but filtering by them
// did not. A metadata-only token could ask `?tag=sensitive-health` and read the
// answer off which messages came back — the same inference /v1/search already
// refuses, on results that are equally metadata-shaped.
func TestAnnotationFiltersRequireContentPermission(t *testing.T) {
	f := newAPIFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := f.pool.Exec(ctx, `
		INSERT INTO message_annotations (message_id, model, tags, category, summary)
		VALUES ($1, 'classifier', ARRAY['sensitive-health'], 'medical', 'a summary')`,
		f.aliceMsgIDs[0]); err != nil {
		t.Fatalf("seed annotation: %v", err)
	}

	endpoints := map[string]string{
		"folder listing": "/v1/mailboxes/alice/folders/INBOX/messages",
		"search":         "/v1/search?q=gophers&mailbox=alice",
		"export":         "/v1/export?mailbox=alice",
	}
	confidential := []string{"tag=sensitive-health", "category=medical"}
	operational := []string{"annotated_by=classifier", "not_annotated_by=classifier"}

	for name, base := range endpoints {
		join := func(q string) string {
			if strings.Contains(base, "?") {
				return base + "&" + q
			}
			return base + "?" + q
		}
		for _, q := range confidential {
			t.Run(name+" "+q, func(t *testing.T) {
				resp := f.do(http.MethodGet, join(q), f.metaToken, nil)
				defer resp.Body.Close()
				body, _ := io.ReadAll(resp.Body)
				if resp.StatusCode != http.StatusForbidden {
					t.Fatalf("metadata-only token got %d for %s, want 403 (body: %s)",
						resp.StatusCode, join(q), body)
				}
				if strings.Contains(string(body), "sensitive-health") {
					t.Error("the refusal echoed the guessed label back")
				}
			})
		}
		// The same filters work for a token that may read content — the fix
		// must not take a capability away from the consumers that have it.
		for _, q := range confidential {
			t.Run(name+" with content "+q, func(t *testing.T) {
				resp := f.do(http.MethodGet, join(q), f.aliceContentToken, nil)
				defer resp.Body.Close()
				body, _ := io.ReadAll(resp.Body)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("content token got %d for %s (body: %s)", resp.StatusCode, join(q), body)
				}
			})
		}
	}

	// Model-presence filters stay available to a metadata-only token: they
	// answer "has this pipeline run", not "what does the mail say", and the
	// worker cooperation knob depends on them.
	for _, q := range operational {
		resp := f.do(http.MethodGet, "/v1/mailboxes/alice/folders/INBOX/messages?"+q, f.metaToken, nil)
		func() {
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("metadata-only token got %d for %s (body: %s)", resp.StatusCode, q, body)
			}
		}()
	}

	// Ordinary metadata pagination is untouched.
	var page struct {
		Messages []struct{ ID int64 } `json:"messages"`
	}
	f.getJSON("/v1/mailboxes/alice/folders/INBOX/messages?limit=3", f.metaToken, http.StatusOK, &page)
	if len(page.Messages) != 3 {
		t.Errorf("metadata page returned %d messages, want 3", len(page.Messages))
	}

	// Mailbox scope still wins over permission: bob's content token cannot use
	// a content filter against alice.
	resp := f.do(http.MethodGet,
		"/v1/mailboxes/alice/folders/INBOX/messages?tag=sensitive-health", f.bobToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("bob's token got %d against alice, want 403", resp.StatusCode)
	}
}

// ---- RA6X-063: stalled export readers ------------------------------------

// rawExport opens a TCP connection, sends an authenticated /v1/export request
// and returns the connection with the status line already read. The caller
// controls exactly how much of the body is consumed, which a normal http.Client
// does not allow.
func rawExport(t *testing.T, addr, token, query string) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// A small receive buffer keeps the amount of data needed to block the
	// server's writes modest, so the fixture does not have to hold megabytes.
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4096)
	}
	req := "GET /v1/export" + query + " HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Authorization: Bearer " + token + "\r\n" +
		"Connection: close\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	br := bufio.NewReader(conn)
	if err := conn.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	return conn, br, strings.TrimSpace(status)
}

// TestStalledExportReaderReleasesItsSlot is the RA6X-063 regression, run over
// real TCP because the defect is in what happens when the socket stops
// draining.
//
// A connected, authenticated client that stops consuming blocks the handler
// inside Write once the buffers fill, and the export semaphore slot is held
// until the handler returns. Two such clients exhausted the default capacity
// and every other worker got 429 — indefinitely, because neither WriteTimeout
// (unset, and a whole-response deadline anyway) nor IdleTimeout (which bounds
// an idle keep-alive connection, not an active write) applies.
func TestStalledExportReaderReleasesItsSlot(t *testing.T) {
	f := newAPIFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Enough body bytes that the server's writes must block on a client that
	// has stopped reading.
	f.seedBulkMessages(ctx, 24, 128*1024)
	// One slot, and a short stall budget so the test does not take minutes.
	f.srv.cfg.Limits.ExportWriteStall = "2s"
	f.srv.exportSem = make(chan struct{}, 1)

	addr := f.ts.Listener.Addr().String()

	// A client that reads the headers and then stops.
	conn, _, status := rawExport(t, addr, f.aliceContentToken, "?mailbox=alice")
	defer conn.Close()
	if !strings.Contains(status, "200") {
		t.Fatalf("export status line = %q, want 200", status)
	}

	// While it holds the only slot, a second export is refused.
	deadline := time.Now().Add(15 * time.Second)
	var refused bool
	for time.Now().Before(deadline) {
		resp := f.do(http.MethodGet, "/v1/export?mailbox=alice", f.aliceContentToken, nil)
		code := resp.StatusCode
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if code == http.StatusTooManyRequests {
			refused = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !refused {
		t.Fatal("the stalled reader never held the export slot; the fixture is too small to block the writes")
	}

	// After the stall budget elapses the handler must give up and free it.
	freed := false
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp := f.do(http.MethodGet, "/v1/export?mailbox=alice", f.aliceContentToken, nil)
		code := resp.StatusCode
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if code == http.StatusOK {
			freed = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !freed {
		t.Fatal("a stalled reader held the export slot indefinitely")
	}
}

// TestSlowButProgressingExportOutlivesTheStallBudget pins the other side: a
// consumer that keeps reading, however slowly, must not be cut off. This is
// the epistula-llm-worker case — it pauses for each message's inference and then
// reads again.
func TestSlowButProgressingExportOutlivesTheStallBudget(t *testing.T) {
	f := newAPIFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	f.seedBulkMessages(ctx, 12, 64*1024)
	f.srv.cfg.Limits.ExportWriteStall = "1s"

	conn, br, status := rawExport(t, f.ts.Listener.Addr().String(), f.aliceContentToken, "?mailbox=alice")
	defer conn.Close()
	if !strings.Contains(status, "200") {
		t.Fatalf("export status line = %q, want 200", status)
	}

	// Read in small sips with pauses that are a large fraction of the budget,
	// for several times the budget in total.
	buf := make([]byte, 4096)
	total := 0
	stop := time.Now().Add(4 * time.Second)
	for time.Now().Before(stop) {
		if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatalf("deadline: %v", err)
		}
		n, err := br.Read(buf)
		total += n
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("a progressing reader was cut off after %d bytes: %v", total, err)
		}
		time.Sleep(700 * time.Millisecond)
	}
	if total == 0 {
		t.Fatal("read nothing")
	}
	// Drain the rest: the stream must still be alive and complete normally.
	if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	n, err := io.Copy(io.Discard, br)
	if err != nil {
		t.Fatalf("a progressing reader was cut off while draining after %d more bytes: %v", n, err)
	}
}
