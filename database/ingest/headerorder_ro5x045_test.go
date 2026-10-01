package ingest

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestHeaderMergeOrderIsDeterministic resolves RO5X-045.
//
// Parse iterates msg.Header — a Go map — and merge-appends into
// out.Headers[key]. When two DISTINCT wire keys sanitize to the SAME string
// (the R-053 case: "X-Bad\x00Key" and "X-BadKey"), the resulting VALUE ORDER
// depends on map-iteration order, which Go deliberately randomizes. Two parses
// of the identical bytes could then produce different JSONB — breaking
// byte-comparison workflows like import-verify and making epistula-api's headers
// passthrough non-reproducible.
//
// The check: parse the same NUL-containing fixture many times and assert the
// marshalled JSONB is byte-identical every time.
func TestHeaderMergeOrderIsDeterministic(t *testing.T) {
	// Two wire keys that collapse to one sanitized key, with distinguishable
	// values so a reordering is visible.
	// "X-K\x00ey" and "X-Key" DO collapse to one sanitized key (verified
	// empirically; note "X-Bad\x00Key" vs "X-BadKey" does NOT, because
	// textproto canonicalizes them to differently-cased keys).
	raw := []byte("From: a@b.invalid\r\n" +
		"X-Key: first-value\r\n" +
		"X-K\x00ey: second-value\r\n" +
		"Subject: s\r\n" +
		"\r\nbody\r\n")

	parser := New(DefaultLimits())
	first, err := parser.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want, err := json.Marshal(first.Headers)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Confirm the fixture actually exercises the collision, or the test would
	// pass vacuously.
	var collapsed bool
	for k, vs := range first.Headers {
		if strings.Contains(strings.ToLower(k), "x-key") && len(vs) > 1 {
			collapsed = true
		}
	}
	if !collapsed {
		t.Skipf("fixture did not produce a merged key; headers = %v", first.Headers)
	}

	const iterations = 500
	for i := 0; i < iterations; i++ {
		msg, err := parser.Parse(raw)
		if err != nil {
			t.Fatalf("Parse iteration %d: %v", i, err)
		}
		got, err := json.Marshal(msg.Headers)
		if err != nil {
			t.Fatalf("marshal iteration %d: %v", i, err)
		}
		if string(got) != string(want) {
			t.Fatalf("headers JSONB differs between parses of identical bytes "+
				"(iteration %d) — value order depends on map iteration (RO5X-045):\n"+
				"first: %s\ngot:   %s", i, want, got)
		}
	}
}

// TestHeaderOrderStableWithoutCollision is the ordinary case: no two keys
// collapse, so each key holds exactly its own values and order cannot vary.
func TestHeaderOrderStableWithoutCollision(t *testing.T) {
	raw := []byte("From: a@b.invalid\r\nTo: c@d.invalid\r\nSubject: s\r\n" +
		"Received: from one\r\nReceived: from two\r\nReceived: from three\r\n" +
		"\r\nbody\r\n")

	parser := New(DefaultLimits())
	first, err := parser.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want, _ := json.Marshal(first.Headers)

	for i := 0; i < 200; i++ {
		msg, err := parser.Parse(raw)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		got, _ := json.Marshal(msg.Headers)
		if string(got) != string(want) {
			t.Fatalf("headers JSONB differs between parses (iteration %d):\n%s\n%s", i, want, got)
		}
	}

	// Multi-valued headers must preserve their on-the-wire order — Received
	// order is load-bearing for tracing a message's path.
	rec := first.Headers["Received"]
	if len(rec) != 3 {
		t.Fatalf("Received = %v, want 3 values", rec)
	}
	for i, want := range []string{"from one", "from two", "from three"} {
		if rec[i] != want {
			t.Errorf("Received[%d] = %q, want %q — wire order must be preserved", i, rec[i], want)
		}
	}
}
