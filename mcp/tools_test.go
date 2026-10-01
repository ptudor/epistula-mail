package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNormTimeAbsolutePassthrough(t *testing.T) {
	for _, in := range []string{"2026-07-01T00:00:00Z", "2026-07-01", "2026-12-31T23:59:59+02:00"} {
		got, err := normTime(in)
		if err != nil {
			t.Fatalf("normTime(%q) errored: %v", in, err)
		}
		if got != in {
			t.Errorf("normTime(%q) = %q, want passthrough", in, got)
		}
	}
}

func TestNormTimeEmpty(t *testing.T) {
	got, err := normTime("")
	if err != nil || got != "" {
		t.Errorf("normTime(\"\") = %q, %v; want \"\", nil", got, err)
	}
}

func TestNormTimeRelative(t *testing.T) {
	cases := map[string]time.Duration{
		"30m": 30 * time.Minute,
		"6h":  6 * time.Hour,
		"2d":  48 * time.Hour,
		"1w":  7 * 24 * time.Hour,
	}
	for in, want := range cases {
		got, err := normTime(in)
		if err != nil {
			t.Fatalf("normTime(%q) errored: %v", in, err)
		}
		parsed, err := time.Parse(time.RFC3339, got)
		if err != nil {
			t.Fatalf("normTime(%q) = %q, not RFC3339: %v", in, got, err)
		}
		delta := time.Since(parsed) - want
		if delta < -2*time.Second || delta > 2*time.Second {
			t.Errorf("normTime(%q) = %q, lookback off by %v", in, got, delta)
		}
	}
}

func TestNormTimeInvalid(t *testing.T) {
	for _, in := range []string{"yesterday", "5x", "-3d", "0h", "d"} {
		if _, err := normTime(in); err == nil {
			t.Errorf("normTime(%q) should have errored", in)
		}
	}
}

// TestClampLimit pins the resolution of a caller-supplied limit.
//
// The two non-positive cases previously expected "" — send no limit at all and
// let epistula-api apply its own default. That is the behaviour RA6X-055 removes:
// it was the one path on which the operator's max_limit had no effect, so a
// connector configured with max_limit=10 answered a default list with the
// API's 50. The expectations below are the reversal, not a drift.
func TestClampLimit(t *testing.T) {
	ts := &toolset{cfg: config{MaxLimit: 200}}
	cases := map[int]string{
		0:    "default:200", // unset -> the API's default, explicitly
		-5:   "default:200", // non-positive is the same absence
		50:   "50",          // under cap
		200:  "200",         // at cap
		1000: "200",         // over cap -> clamped
	}
	for in, want := range cases {
		if got := ts.clampLimit(in); got != want {
			t.Errorf("clampLimit(%d) = %q, want %q", in, got, want)
		}
	}

	// A cap below the API's default binds the default path too, which is the
	// whole point of the change.
	small := &toolset{cfg: config{MaxLimit: 10}}
	for _, in := range []int{0, -1, 50, 1000} {
		want := "10"
		if in <= 0 {
			want = "default:10"
		}
		if got := small.clampLimit(in); got != want {
			t.Errorf("with max_limit=10, clampLimit(%d) = %q, want \"10\"", in, got)
		}
	}
	// A cap above the default leaves the default alone.
	big := &toolset{cfg: config{MaxLimit: 500}}
	if got := big.clampLimit(0); got != "default:500" {
		t.Errorf("with max_limit=500, an omitted limit = %q, want the API default 50", got)
	}
}

// TestEscapePathData pins the encoding of a name that goes into a URL path.
//
// This test previously asserted that a folder's own slashes stayed REAL path
// separators — `Archive/2026` → `Archive/2026`. That is the behaviour RA6X-057
// removes: it left the name's structure at the mercy of Go's ServeMux path
// cleaning, which rewrote `A/../B` to `B` before the handler ever saw it. The
// expectations below are the reversal, not a drift.
func TestEscapePathData(t *testing.T) {
	cases := map[string]string{
		"INBOX":         "INBOX",
		"Archive/2026":  "Archive%2F2026",
		"a b":           "a%20b",
		"weird/na me":   "weird%2Fna%20me",
		"a+b":           "a+b", // '+' is not escaped in a path segment
		"Sent Messages": "Sent%20Messages",
		".":             "%2E",
		"..":            "%2E%2E",
		"A/../B":        "A%2F%2E%2E%2FB",
		"A//B":          "A%2F%2FB",
		"dot.in.name":   "dot%2Ein%2Ename",
		"%2e":           "%252e", // a literal percent sign survives as data
	}
	for in, want := range cases {
		if got := escapePathData(in); got != want {
			t.Errorf("escapePathData(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCapTextShort(t *testing.T) {
	s := "hello world"
	got, truncated := capText(s, 0, 1024)
	if got != s || truncated {
		t.Errorf("capText short = %q, %v; want full, false", got, truncated)
	}
}

func TestCapTextTruncates(t *testing.T) {
	s := strings.Repeat("a", 100)
	got, truncated := capText(s, 0, 10)
	if len(got) != 10 || !truncated {
		t.Errorf("capText(100 'a', cap 10) = len %d, truncated %v; want 10, true", len(got), truncated)
	}
}

func TestCapTextOffset(t *testing.T) {
	s := "0123456789"
	got, truncated := capText(s, 5, 100)
	if got != "56789" || truncated {
		t.Errorf("capText offset 5 = %q, %v; want \"56789\", false", got, truncated)
	}
}

func TestCapTextOffsetPastEnd(t *testing.T) {
	got, truncated := capText("short", 100, 10)
	if got != "" || truncated {
		t.Errorf("capText past end = %q, %v; want \"\", false", got, truncated)
	}
}

// TestCapTextRuneBoundary verifies a multibyte rune is never split at the cap:
// "é" is 2 bytes (0xC3 0xA9), so a cap that lands mid-rune backs off.
func TestCapTextRuneBoundary(t *testing.T) {
	s := "aaaé" // 3 ASCII + one 2-byte rune = 5 bytes
	got, truncated := capText(s, 0, 4)
	if !truncated {
		t.Fatal("expected truncation")
	}
	if got != "aaa" {
		t.Errorf("capText mid-rune = %q, want \"aaa\" (backed off the split rune)", got)
	}
	if !utf8ValidString(got) {
		t.Errorf("capText produced invalid UTF-8: %q", got)
	}
}

func utf8ValidString(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// --- problemError: RFC 7807 -> bounded apiError mapping ---

func makeResp(status int, ct, body string) (*http.Response, []byte) {
	h := http.Header{}
	if ct != "" {
		h.Set("Content-Type", ct)
	}
	return &http.Response{StatusCode: status, Header: h}, []byte(body)
}

func TestProblemErrorRFC7807(t *testing.T) {
	resp, data := makeResp(403, "application/problem+json",
		`{"type":"https://x/errors/forbidden","title":"Forbidden","status":403,"detail":"Token is not scoped to mailbox 'jdoe'."}`)
	e := problemError(resp, data)
	if e.Status != 403 {
		t.Errorf("status = %d, want 403", e.Status)
	}
	if e.Title != "Forbidden" {
		t.Errorf("title = %q, want Forbidden", e.Title)
	}
	if !strings.Contains(e.Detail, "not scoped") {
		t.Errorf("detail = %q, want the upstream detail", e.Detail)
	}
}

func TestProblemErrorBoundsSnippet(t *testing.T) {
	long := strings.Repeat("X", 5000)
	resp, data := makeResp(500, "text/html", long)
	e := problemError(resp, data)
	if e.Status != 500 {
		t.Errorf("status = %d, want 500", e.Status)
	}
	if len(e.Detail) > maxErrBody+len("…") {
		t.Errorf("detail not bounded: len %d", len(e.Detail))
	}
	if e.Title != http.StatusText(500) {
		t.Errorf("title = %q, want %q", e.Title, http.StatusText(500))
	}
}

func TestProblemErrorEmptyBody(t *testing.T) {
	resp, data := makeResp(404, "", "")
	e := problemError(resp, data)
	if e.Status != 404 || e.Detail != "" {
		t.Errorf("empty-body 404 = %+v, want status 404 and empty detail", e)
	}
}

// TestSlimMessageOmitsEmpties verifies the projection drops null/empty fields
// and never surfaces a body it wasn't given.
func TestSlimMessageOmitsEmpties(t *testing.T) {
	ts := &toolset{cfg: config{MaxTextBytes: 1024}}
	row := map[string]any{
		"id":            float64(42),
		"uid":           float64(7),
		"subject":       "Invoice",
		"from":          "billing@host.invalid",
		"to":            []any{"me@x.invalid"},
		"cc":            []any{},
		"internal_date": "2026-07-01T00:00:00Z",
		"flags":         []any{"\\Seen"},
		"size":          float64(1234),
	}
	m := ts.slimMessage(row)
	if m["id"] != float64(42) || m["subject"] != "Invoice" {
		t.Errorf("core fields missing: %+v", m)
	}
	if _, ok := m["cc"]; ok {
		t.Errorf("empty cc should be omitted, got %+v", m["cc"])
	}
	if _, ok := m["text_body"]; ok {
		t.Errorf("slimMessage must not invent a body")
	}
	if m["date"] != "2026-07-01T00:00:00Z" {
		t.Errorf("internal_date should map to date, got %+v", m["date"])
	}
}
