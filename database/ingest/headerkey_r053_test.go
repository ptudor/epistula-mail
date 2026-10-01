package ingest

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestHeaderKeyNULSanitized is the R-053 regression: a NUL byte in a header
// *name* (which net/mail passes through raw — verified reachable) must be
// stripped so the derived headers map is JSONB-safe. Before the fix the raw
// key reached json.Marshal and Postgres rejected the resulting \u0000 escape,
// failing the Ingest INSERT as a non-retryable EX_SOFTWARE.
func TestHeaderKeyNULSanitized(t *testing.T) {
	raw := "From: a@x.invalid\r\n" +
		"X-Bad\x00Key: value1\r\n" +
		"Subject: s\r\n\r\nbody\r\n"

	p := New(DefaultLimits())
	msg, err := p.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	for k, vs := range msg.Headers {
		if strings.ContainsRune(k, 0) {
			t.Errorf("header key retained a NUL: %q", k)
		}
		for _, v := range vs {
			if strings.ContainsRune(v, 0) {
				t.Errorf("header value retained a NUL: key=%q val=%q", k, v)
			}
		}
	}
	if got := msg.Headers["X-BadKey"]; len(got) != 1 || got[0] != "value1" {
		t.Errorf("sanitized key X-BadKey = %v, want [value1] (value dropped?)", got)
	}

	// JSONB-safety proxy: json.Marshal renders a NUL byte as the six-character
	// escape \u0000, which is exactly what Postgres jsonb rejects. Assert it is
	// absent from what the ingest INSERT would bind.
	b, err := json.Marshal(msg.Headers)
	if err != nil {
		t.Fatalf("marshal headers: %v", err)
	}
	if strings.Contains(string(b), "\\u0000") {
		t.Errorf("marshaled headers contain a JSONB-invalid NUL escape: %s", b)
	}
}

// TestHeaderKeyCollisionMerges: two distinct wire keys that collapse to the
// same map key must merge their values, not overwrite — otherwise one key's
// values are silently dropped. The NUL-bearing key "X-Dup\x00key" stays raw
// (non-token byte defeats canonicalization) and sanitizes to "X-Dupkey"; the
// clean "X-DupKey" canonicalizes to the same "X-Dupkey". They collide.
func TestHeaderKeyCollisionMerges(t *testing.T) {
	raw := "From: a@x.invalid\r\n" +
		"X-Dup\x00key: first\r\n" +
		"X-DupKey: second\r\n" +
		"Subject: s\r\n\r\nbody\r\n"

	p := New(DefaultLimits())
	msg, err := p.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := msg.Headers["X-Dupkey"]
	if len(got) != 2 {
		t.Fatalf("X-Dupkey = %v, want both values merged (len 2)", got)
	}
	seen := map[string]bool{}
	for _, v := range got {
		seen[v] = true
	}
	if !seen["first"] || !seen["second"] {
		t.Errorf("merged values = %v, want both 'first' and 'second'", got)
	}
}
