package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---- RA6X-058: lookback arithmetic ---------------------------------------

// TestLookbackRejectsZeroTinyAndOverflow is the RA6X-058 regression.
//
// The d/w branches converted a float product straight to time.Duration and
// returned, skipping the positivity check the Go-duration path gets. `0d` and
// `0w` became a zero window — a search "since now", which quietly matches
// nothing — and an out-of-range float converted to an implementation-defined
// integer: `999999999d` landed on MaxInt64, a 292-year lookback presented as if
// it had been asked for.
func TestLookbackRejectsZeroTinyAndOverflow(t *testing.T) {
	for _, in := range []string{
		"0d", "0w", "0h", "0", "0s",
		"0.0000000000000001d", "0.0000000000000001w",
		"999999999d", "999999999w", "1e300d",
		"-1d", "-2h",
	} {
		t.Run(in, func(t *testing.T) {
			if d, err := parseLookback(in); err == nil {
				t.Fatalf("parseLookback(%q) = %s, want an argument error", in, d)
			}
		})
	}

	// Valid values keep working, fractions included.
	for in, want := range map[string]time.Duration{
		"2d":    48 * time.Hour,
		"1w":    7 * 24 * time.Hour,
		"0.5d":  12 * time.Hour,
		"1.5w":  252 * time.Hour,
		"90m":   90 * time.Minute,
		"6h":    6 * time.Hour,
		"1h30m": 90 * time.Minute,
		"1ns":   time.Nanosecond,
	} {
		got, err := parseLookback(in)
		if err != nil {
			t.Errorf("parseLookback(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseLookback(%q) = %s, want %s", in, got, want)
		}
	}

	// The largest representable day count still converts.
	maxDays := math.Floor(float64(math.MaxInt64)/float64(24*time.Hour)) - 1
	if _, err := parseLookback(strconv.FormatFloat(maxDays, 'f', 0, 64) + "d"); err != nil {
		t.Errorf("the largest representable day lookback was rejected: %v", err)
	}
	// One step past it is not.
	overDays := math.Ceil(float64(math.MaxInt64)/float64(24*time.Hour)) + 1
	if d, err := parseLookback(strconv.FormatFloat(overDays, 'f', 0, 64) + "d"); err == nil {
		t.Errorf("a day lookback past time.Duration's range was accepted as %s", d)
	}

	// normTime rejects the same values without any upstream request, and still
	// passes absolute forms through unchanged.
	if _, err := normTime("0d"); err == nil {
		t.Error("normTime accepted a zero lookback")
	}
	for _, abs := range []string{"2026-07-01T00:00:00Z", "2026-07-01"} {
		got, err := normTime(abs)
		if err != nil || got != abs {
			t.Errorf("normTime(%q) = %q, %v; want it passed through", abs, got, err)
		}
	}
	// A relative value resolves to a PAST timestamp.
	got, err := normTime("2d")
	if err != nil {
		t.Fatalf("normTime(2d): %v", err)
	}
	ts, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("normTime(2d) = %q, not RFC 3339", got)
	}
	if !ts.Before(time.Now()) {
		t.Errorf("normTime(2d) = %q, which is not in the past", got)
	}
}

// resultText returns the raw JSON text of a tool result. The RA6X-056 test
// needs it because decodeResult's own json.Unmarshal into map[string]any is
// exactly the float64 rounding under test — decoding the result the ordinary
// way would hide the very thing being asserted.
func resultText(t *testing.T, res *mcp.CallToolResult, err error) string {
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
	return tc.Text
}

// decodeExactResult decodes a tool result preserving integer precision.
func decodeExactResult(t *testing.T, res *mcp.CallToolResult, err error) map[string]any {
	t.Helper()
	v, derr := decodeExactJSON([]byte(resultText(t, res, err)))
	if derr != nil {
		t.Fatalf("tool result not JSON: %v", derr)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("tool result was %T, want an object", v)
	}
	return m
}

// ---- RA6X-056: 64-bit ids survive the connector ---------------------------

// TestLargeMessageIDsSurviveTheConnector is the RA6X-056 regression.
//
// getJSON decoded into `any`, so every number became a float64 whose 53-bit
// mantissa cannot hold every int64. messages.id is a BIGINT, so an id past 2^53
// reached the model ROUNDED — and a rounded id addresses a different message,
// which the model would then fetch or annotate.
func TestLargeMessageIDsSurviveTheConnector(t *testing.T) {
	ids := []string{"9007199254740991", "9007199254740992", "9007199254740993", "9223372036854775807"}

	var lastPath string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/mailboxes/{mailbox}/folders/{rest...}", func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		rows := make([]string, 0, len(ids))
		for _, id := range ids {
			rows = append(rows, fmt.Sprintf(
				`{"id":%s,"uid":%s,"internal_date":"2026-06-01T00:00:00Z","subject":"s",`+
					`"from":"a@b.invalid","flags":[],"size":9007199254740993,"attachment_count":1}`, id, id))
		}
		folder, _ := strings.CutSuffix(r.PathValue("rest"), "/messages")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"mailbox":"jdoe","folder":%q,"messages":[%s],"next_cursor":"CURSOR"}`,
			folder, strings.Join(rows, ","))
	})
	mux.HandleFunc("GET /v1/messages/{id}", func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		id := r.PathValue("id")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%s,"uid":%s,"mailbox":"jdoe","folder":"INBOX",`+
			`"internal_date":"2026-06-01T00:00:00Z","subject":"s","from":"a@b.invalid",`+
			`"flags":[],"size":1,"attachments":[],"text_body":"hello"}`, id, id)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ts := &toolset{
		cfg: config{MaxLimit: 100, MaxTextBytes: 4096},
		c: newClientWithToken(config{
			BaseURL: srv.URL, RequestTimeout: requestTimeoutForTest, MaxResponseBytes: 1 << 20,
		}, "test-token"),
	}

	res, _, err := ts.messages(context.Background(), nil, messagesInput{Mailbox: "jdoe", Folder: "INBOX"})
	raw := resultText(t, res, err)
	// The ids must be present in the tool result's own bytes, exactly.
	for _, id := range ids {
		if !strings.Contains(raw, id) {
			t.Errorf("tool result does not contain the exact id %s:\n%s", id, raw)
		}
	}
	out := decodeExactResult(t, res, err)
	msgs, _ := out["messages"].([]any)
	if len(msgs) != len(ids) {
		t.Fatalf("got %d messages, want %d", len(msgs), len(ids))
	}
	for i, m := range msgs {
		row, _ := m.(map[string]any)
		got := fmt.Sprint(row["id"])
		if got != ids[i] {
			t.Errorf("id %d came back as %s, want %s", i, got, ids[i])
		}
		if s := fmt.Sprint(row["size"]); s != "9007199254740993" {
			t.Errorf("size came back as %s, want it exact too", s)
		}
	}

	// The id the model was given addresses the message it thinks it does.
	for _, id := range ids {
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		res, _, err := ts.message(context.Background(), nil, messageInput{ID: n})
		doc := decodeExactResult(t, res, err)
		if got := fmt.Sprint(doc["id"]); got != id {
			t.Errorf("single message returned id %s, want %s", got, id)
		}
		if !strings.Contains(lastPath, id) {
			t.Errorf("follow-up request path %q does not name id %s", lastPath, id)
		}
	}
}

// TestZeroAttachmentCountIsStillOmitted pins that the numeric slimming check
// still works now that numbers are json.Number rather than float64.
func TestZeroAttachmentCountIsStillOmitted(t *testing.T) {
	ts := &toolset{cfg: config{MaxTextBytes: 4096}}
	zero, err := decodeExactJSON([]byte(`{"id":1,"attachment_count":0}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, present := ts.slimMessage(asMap(zero))["attachment_count"]; present {
		t.Error("a zero attachment_count was forwarded; it should be omitted")
	}
	two, err := decodeExactJSON([]byte(`{"id":1,"attachment_count":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(ts.slimMessage(asMap(two))["attachment_count"]); got != "2" {
		t.Errorf("attachment_count = %s, want 2", got)
	}
}

// ---- RA6X-018: text paging always advances --------------------------------

// TestTextPagingAlwaysAdvances is the RA6X-018 regression.
//
// capText backed off past the cut when the next rune was wider than the budget
// and returned an EMPTY page with truncated=true, so next_offset equalled the
// input offset forever. Arbitrary offsets landing inside a rune were also
// rounded by the API without telling the connector, whose next offset was
// computed from the offset it had sent.
func TestTextPagingAlwaysAdvances(t *testing.T) {
	// ASCII plus 2-, 3- and 4-byte runes, so every budget from 1 to 5 meets a
	// rune it cannot fit.
	body := "abé€\U0001F600cdé\U0001F600ef"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		start, slice := serverSliceForTest(body, offset, limit)
		w.Header().Set("X-Total-Bytes", strconv.Itoa(len(body)))
		w.Header().Set("X-Content-Offset", strconv.Itoa(start))
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(slice))
	}))
	defer srv.Close()

	for budget := 1; budget <= 5; budget++ {
		t.Run(fmt.Sprintf("budget=%d", budget), func(t *testing.T) {
			ts := &toolset{
				cfg: config{MaxLimit: 100, MaxTextBytes: budget},
				c: newClientWithToken(config{
					BaseURL: srv.URL, RequestTimeout: requestTimeoutForTest, MaxResponseBytes: 1 << 20,
				}, "test-token"),
			}

			// Walk the body from zero. Every page must advance, and the
			// concatenation must reproduce it exactly.
			var sb strings.Builder
			offset := 0
			for step := 0; ; step++ {
				if step > 4*len(body) {
					t.Fatalf("paging did not terminate; stuck at offset %d", offset)
				}
				res, _, err := ts.messageText(context.Background(), nil, messageTextInput{ID: 1, Offset: offset})
				out := decodeExactResult(t, res, err)
				if e := toolErrorText(out); e != "" {
					t.Fatalf("messageText at offset %d: %s", offset, e)
				}
				text, _ := out["text"].(string)
				sb.WriteString(text)
				if out["truncated"] != true {
					break
				}
				next := intOf(t, out["next_offset"])
				if next <= offset {
					t.Fatalf("next_offset %d did not advance past %d", next, offset)
				}
				offset = next
			}
			if sb.String() != body {
				t.Errorf("concatenated pages = %q, want the original body", sb.String())
			}
			if !utf8.ValidString(sb.String()) {
				t.Error("the concatenation is not valid UTF-8")
			}

			// Every byte offset, including interior ones, terminates and
			// reports an effective start that is a rune boundary.
			for start := 0; start <= len(body)+2; start++ {
				res, _, err := ts.messageText(context.Background(), nil, messageTextInput{ID: 1, Offset: start})
				out := decodeExactResult(t, res, err)
				if e := toolErrorText(out); e != "" {
					t.Fatalf("messageText at offset %d: %s", start, e)
				}
				eff := intOf(t, out["offset"])
				// The effective start moves FORWARD to a rune boundary, except
				// past EOF, where it clamps to the body length.
				if eff < start && eff != len(body) {
					t.Errorf("offset %d reported an effective start of %d, which is backwards", start, eff)
				}
				if eff < len(body) && !utf8.RuneStart(body[eff]) {
					t.Errorf("effective start %d is inside a rune", eff)
				}
				if out["truncated"] == true {
					if next := intOf(t, out["next_offset"]); next <= eff {
						t.Errorf("offset %d: next_offset %d did not advance past the effective start %d",
							start, next, eff)
					}
				} else if start >= len(body) {
					if text, _ := out["text"].(string); text != "" {
						t.Errorf("offset %d past EOF returned %q", start, text)
					}
				}
			}
		})
	}
}

// serverSliceForTest mirrors epistula-api's sliceTextBody, including the effective
// start and the one-complete-rune guarantee, so this test exercises the
// connector against the contract the API actually implements.
func serverSliceForTest(body string, offset, limit int) (int, string) {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(body) {
		return len(body), ""
	}
	for offset < len(body) && !utf8.RuneStart(body[offset]) {
		offset++
	}
	end := len(body)
	if limit > 0 && len(body)-offset > limit {
		end = offset + limit
		for end > offset && !utf8.RuneStart(body[end]) {
			end--
		}
		if end == offset {
			_, size := utf8.DecodeRuneInString(body[offset:])
			end = offset + size
		}
	}
	return offset, body[offset:end]
}

func intOf(t *testing.T, v any) int {
	t.Helper()
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			t.Fatalf("not an integer: %v", v)
		}
		return int(i)
	case float64:
		return int(n)
	case int:
		return n
	}
	t.Fatalf("not a number: %#v", v)
	return 0
}

// TestCapTextEmitsOneRuneRatherThanNothing pins the helper directly: no budget
// may produce an empty page while bytes remain.
func TestCapTextEmitsOneRuneRatherThanNothing(t *testing.T) {
	for _, s := range []string{"\U0001F600next", "éx", "€y", "plain"} {
		for budget := 1; budget <= 5; budget++ {
			text, truncated := capText(s, 0, budget)
			if text == "" {
				t.Errorf("capText(%q, 0, %d) returned nothing while bytes remain", s, budget)
			}
			if !utf8.ValidString(text) {
				t.Errorf("capText(%q, 0, %d) split a rune: %q", s, budget, text)
			}
			if truncated && len(text) == 0 {
				t.Errorf("capText(%q, 0, %d) reported truncation with no progress", s, budget)
			}
		}
	}
	// Past the end is empty and NOT truncated, which terminates a walk.
	if text, truncated := capText("abc", 3, 2); text != "" || truncated {
		t.Errorf("capText past EOF = %q, %v; want \"\", false", text, truncated)
	}
}

// TestConfigRefusesATextBudgetTooSmallForARune pins the configuration floor.
func TestConfigRefusesATextBudgetTooSmallForARune(t *testing.T) {
	clearEnv(t)
	load := func(n int) (config, error) {
		return loadConfig(writeTempConfig(t, fmt.Sprintf(`
[mailapi]
base_url = "http://127.0.0.1:8784"
token = "tok-abc"

[mcp]
max_text_bytes = %d
`, n)))
	}
	for _, n := range []int{1, 2, 3} {
		if _, err := load(n); err == nil {
			t.Errorf("max_text_bytes = %d was accepted; a page that cannot hold a rune cannot advance", n)
		} else if !strings.Contains(err.Error(), "max_text_bytes") {
			t.Errorf("max_text_bytes = %d: unexpected error %v", n, err)
		}
	}
	cfg, err := load(utf8.UTFMax)
	if err != nil {
		t.Fatalf("max_text_bytes = %d (one rune) was refused: %v", utf8.UTFMax, err)
	}
	if cfg.MaxTextBytes != utf8.UTFMax {
		t.Errorf("MaxTextBytes = %d, want %d", cfg.MaxTextBytes, utf8.UTFMax)
	}
}

// ---- RA6X-055: an omitted limit still respects the cap --------------------

// TestOmittedLimitRespectsTheConfiguredCap is the RA6X-055 regression: the
// default path used to send no limit at all and take whatever the API's own
// default was, which is the one request shape max_limit did not bound.
func TestOmittedLimitRespectsTheConfiguredCap(t *testing.T) {
	var lastLimit string
	mux := http.NewServeMux()
	record := func(w http.ResponseWriter, r *http.Request) {
		lastLimit = r.URL.Query().Get("limit")
		folder, _ := strings.CutSuffix(r.PathValue("rest"), "/messages")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"mailbox":"jdoe","folder":%q,"messages":[]}`, folder)
	}
	mux.HandleFunc("GET /v1/mailboxes/{mailbox}/folders/{rest...}", record)
	mux.HandleFunc("GET /v1/search", func(w http.ResponseWriter, r *http.Request) {
		lastLimit = r.URL.Query().Get("limit")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messages":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	newTS := func(maxLimit int) *toolset {
		return &toolset{
			cfg: config{MaxLimit: maxLimit, MaxTextBytes: 4096},
			c: newClientWithToken(config{
				BaseURL: srv.URL, RequestTimeout: requestTimeoutForTest, MaxResponseBytes: 1 << 20,
			}, "test-token"),
		}
	}

	// A cap BELOW the API's default binds every shape, the omitted one first.
	for name, limit := range map[string]int{"omitted": 0, "zero": 0, "negative": -7, "at cap": 10, "over cap": 1000} {
		t.Run("max_limit=10 "+name, func(t *testing.T) {
			ts := newTS(10)
			want := "10"
			if limit <= 0 {
				want = "default:10"
			}
			if _, _, err := ts.messages(context.Background(), nil,
				messagesInput{Mailbox: "jdoe", Folder: "INBOX", Limit: limit}); err != nil {
				t.Fatal(err)
			}
			if lastLimit != want {
				t.Errorf("messages sent limit=%q, want 10", lastLimit)
			}
			if _, _, err := ts.search(context.Background(), nil,
				searchInput{Q: "x", Limit: limit}); err != nil {
				t.Fatal(err)
			}
			if lastLimit != want {
				t.Errorf("search sent limit=%q, want 10", lastLimit)
			}
		})
	}

	// A cap ABOVE the API's default leaves the intended default alone.
	ts := newTS(500)
	if _, _, err := ts.messages(context.Background(), nil,
		messagesInput{Mailbox: "jdoe", Folder: "INBOX"}); err != nil {
		t.Fatal(err)
	}
	if lastLimit != "default:500" {
		t.Errorf("omitted limit sent %q, want default:500", lastLimit)
	}
	// An explicit value under both still passes through.
	if _, _, err := ts.messages(context.Background(), nil,
		messagesInput{Mailbox: "jdoe", Folder: "INBOX", Limit: 7}); err != nil {
		t.Fatal(err)
	}
	if lastLimit != "7" {
		t.Errorf("an explicit limit sent %q, want 7", lastLimit)
	}
}

// ---- RA6X-046: a huge body must not break metadata ------------------------

// TestHugeBodyStillYieldsMetadata is the RA6X-046 regression.
//
// The full document inlines both bodies, so a message with more than the
// connector's response budget of serialized body failed the message tool
// outright — even for a caller that only wanted metadata, attachments or a
// short preview. The client rejects an over-budget response before capText can
// run, so the bytes had to not be sent.
func TestHugeBodyStillYieldsMetadata(t *testing.T) {
	const huge = 12 << 20 // 12 MiB, past the connector's 8 MiB response budget

	var sawProjection bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("text_limit"))
		wantHTML := q.Get("html") != "false"
		sawProjection = limit > 0 && !wantHTML

		text := strings.Repeat("t", huge)
		html := strings.Repeat("<p>h</p>", huge/8)
		truncated := false
		if limit > 0 && limit < len(text) {
			text = text[:limit]
			truncated = true
		}
		doc := map[string]any{
			"id": 42, "uid": 7, "mailbox": "jdoe", "folder": "INBOX",
			"internal_date": "2026-06-01T00:00:00Z", "subject": "big one",
			"from": "a@b.invalid", "flags": []any{}, "size": huge,
			"attachments": []any{map[string]any{
				"filename": "invoice.pdf", "content_type": "application/pdf", "size_bytes": 1000,
			}},
			"annotations": []any{map[string]any{
				"model": "m", "tags": []any{"receipts"}, "category": "finance", "summary": "an invoice",
			}},
			"text_body": text,
		}
		if truncated {
			doc["text_truncated"] = true
			doc["text_bytes"] = huge
		}
		if wantHTML {
			doc["html_body"] = html
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}))
	defer srv.Close()

	ts := &toolset{
		cfg: config{MaxLimit: 100, MaxTextBytes: 4096},
		c: newClientWithToken(config{
			BaseURL: srv.URL, RequestTimeout: requestTimeoutForTest,
			MaxResponseBytes: defaultMaxResponseBytes,
		}, "test-token"),
	}

	res, _, err := ts.message(context.Background(), nil, messageInput{ID: 42})
	out := decodeResult(t, res, err)
	if e := toolErrorText(out); e != "" {
		t.Fatalf("a message with a %d-byte body failed the metadata tool: %s", huge, e)
	}
	if !sawProjection {
		t.Error("the connector did not ask for a bounded document")
	}
	if out["subject"] != "big one" {
		t.Errorf("subject = %v", out["subject"])
	}
	if atts, _ := out["attachments"].([]any); len(atts) != 1 {
		t.Errorf("attachments = %v, want the one part", out["attachments"])
	}
	if anns, _ := out["annotations"].([]any); len(anns) != 1 {
		t.Errorf("annotations = %v, want the one row", out["annotations"])
	}
	if body, _ := out["text_body"].(string); len(body) > ts.cfg.MaxTextBytes {
		t.Errorf("text_body is %d bytes, over the %d-byte cap", len(body), ts.cfg.MaxTextBytes)
	}
	if out["text_body_truncated"] != true {
		t.Error("a truncated preview was not flagged")
	}
	if _, present := out["html_body"]; present {
		t.Error("html_body was forwarded")
	}
}
