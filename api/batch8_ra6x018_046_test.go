package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// setMessageText replaces one fixture message's text_body.
func (f *apiFixture) setMessageText(ctx context.Context, id int64, body string) {
	f.t.Helper()
	if _, err := f.pool.Exec(ctx, `UPDATE messages SET text_body = $2 WHERE id = $1`, id, body); err != nil {
		f.t.Fatalf("set text_body: %v", err)
	}
}

// TestTextProjectionReportsItsEffectiveStart is the RA6X-018 regression on the
// API side.
//
// An offset landing inside a multibyte rune was advanced to the next boundary
// and the client was never told, so it computed its next cursor from the offset
// it had SENT. And when the next rune was wider than the whole limit, the
// back-off produced an EMPTY slice: the client saw zero bytes, computed the
// same offset again, and paged forever without moving.
func TestTextProjectionReportsItsEffectiveStart(t *testing.T) {
	f := newAPIFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	body := "abé€\U0001F600cdé\U0001F600ef"
	id := f.aliceMsgIDs[0]
	f.setMessageText(ctx, id, body)

	get := func(offset, limit int) (int, int, string) {
		t.Helper()
		path := fmt.Sprintf("/v1/messages/%d/text?offset=%d&limit=%d", id, offset, limit)
		resp := f.do(http.MethodGet, path, f.aliceContentToken, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("GET %s = %d (%s)", path, resp.StatusCode, b)
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		start, err := strconv.Atoi(resp.Header.Get("X-Content-Offset"))
		if err != nil {
			t.Fatalf("X-Content-Offset = %q: %v", resp.Header.Get("X-Content-Offset"), err)
		}
		total, err := strconv.Atoi(resp.Header.Get("X-Total-Bytes"))
		if err != nil {
			t.Fatalf("X-Total-Bytes = %q: %v", resp.Header.Get("X-Total-Bytes"), err)
		}
		return start, total, string(data)
	}

	for limit := 1; limit <= 5; limit++ {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			// Walk from zero: every page advances, and the concatenation is the
			// original body byte for byte.
			var sb strings.Builder
			offset := 0
			for step := 0; offset < len(body); step++ {
				if step > 4*len(body) {
					t.Fatalf("paging did not terminate; stuck at offset %d", offset)
				}
				start, total, page := get(offset, limit)
				if total != len(body) {
					t.Fatalf("X-Total-Bytes = %d, want %d", total, len(body))
				}
				if page == "" {
					t.Fatalf("offset %d returned an empty page while %d bytes remain", offset, len(body)-offset)
				}
				if !utf8.ValidString(page) {
					t.Fatalf("offset %d returned a split rune: %q", offset, page)
				}
				sb.WriteString(page)
				next := start + len(page)
				if next <= offset {
					t.Fatalf("offset %d produced next offset %d; the cursor does not advance", offset, next)
				}
				offset = next
			}
			if sb.String() != body {
				t.Errorf("concatenated pages = %q, want the body", sb.String())
			}

			// Every byte offset, interior ones included, reports a rune-aligned
			// effective start at or after what was asked for.
			for want := 0; want <= len(body)+2; want++ {
				start, _, page := get(want, limit)
				if start < want && start != len(body) {
					t.Errorf("offset %d reported effective start %d, which is backwards", want, start)
				}
				if start < len(body) && !utf8.RuneStart(body[start]) {
					t.Errorf("effective start %d is inside a rune", start)
				}
				if start >= len(body) {
					if page != "" {
						t.Errorf("offset %d past EOF returned %q", want, page)
					}
					continue
				}
				if got := body[start : start+len(page)]; got != page {
					t.Errorf("offset %d: page %q is not the body slice at %d", want, page, start)
				}
			}
		})
	}
}

// TestMessageDocumentProjection is the RA6X-046 regression.
//
// The single-message document inlines BOTH bodies, so a message with more text
// and HTML than a consumer's response budget failed the whole request even when
// only its metadata, attachments or a short preview were wanted. The projection
// is additive: without the parameters the document is byte-for-byte what it
// always was.
func TestMessageDocumentProjection(t *testing.T) {
	f := newAPIFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	id := f.aliceMsgIDs[0]
	text := strings.Repeat("t", 4096) + "é"
	if _, err := f.pool.Exec(ctx,
		`UPDATE messages SET text_body = $2, html_body = $3 WHERE id = $1`,
		id, text, strings.Repeat("<p>h</p>", 512)); err != nil {
		t.Fatalf("seed bodies: %v", err)
	}

	// Default: unchanged. Both bodies present in full, no projection fields.
	var full map[string]any
	f.getJSON(fmt.Sprintf("/v1/messages/%d", id), f.classifierToken, http.StatusOK, &full)
	if got, _ := full["text_body"].(string); got != text {
		t.Errorf("default document text_body is %d bytes, want the full %d", len(got), len(text))
	}
	if _, ok := full["html_body"].(string); !ok {
		t.Error("default document lost html_body")
	}
	for _, k := range []string{"text_bytes", "text_truncated"} {
		if _, present := full[k]; present {
			t.Errorf("default document carries %q; the projection must be additive", k)
		}
	}

	// Projected: bounded text, no HTML, and the omitted length reported.
	var bounded map[string]any
	f.getJSON(fmt.Sprintf("/v1/messages/%d?html=false&text_limit=100", id),
		f.classifierToken, http.StatusOK, &bounded)
	got, _ := bounded["text_body"].(string)
	if len(got) > 100 {
		t.Errorf("text_body is %d bytes, over the 100-byte limit", len(got))
	}
	if !utf8.ValidString(got) {
		t.Error("the bounded preview split a rune")
	}
	if _, present := bounded["html_body"]; present {
		t.Error("html=false still returned html_body")
	}
	if bounded["text_truncated"] != true {
		t.Error("a bounded preview was not flagged truncated")
	}
	if n, ok := bounded["text_bytes"].(float64); !ok || int(n) != len(text) {
		t.Errorf("text_bytes = %v, want the full body length %d", bounded["text_bytes"], len(text))
	}
	// The metadata a caller actually wanted is all there.
	for _, k := range []string{"id", "uid", "mailbox", "folder", "subject", "flags", "attachments", "headers"} {
		if _, present := bounded[k]; !present {
			t.Errorf("the bounded document is missing %q", k)
		}
	}

	// A limit that lands mid-rune trims back rather than splitting.
	var midRune map[string]any
	f.getJSON(fmt.Sprintf("/v1/messages/%d?text_limit=%d", id, len(text)-1),
		f.classifierToken, http.StatusOK, &midRune)
	if got, _ := midRune["text_body"].(string); !utf8.ValidString(got) {
		t.Error("a mid-rune limit split a character")
	}

	// A limit at or past the body length is not truncation.
	var whole map[string]any
	f.getJSON(fmt.Sprintf("/v1/messages/%d?text_limit=%d", id, len(text)),
		f.classifierToken, http.StatusOK, &whole)
	if whole["text_truncated"] == true {
		t.Error("a limit equal to the body length reported truncation")
	}

	// Malformed parameters are client errors, not silent defaults.
	for _, q := range []string{"text_limit=-1", "text_limit=abc", "html=maybe"} {
		resp := f.do(http.MethodGet, fmt.Sprintf("/v1/messages/%d?%s", id, q), f.classifierToken, nil)
		code := resp.StatusCode
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if code < 400 || code >= 500 {
			t.Errorf("?%s = %d, want a 4xx", q, code)
		}
	}
}
