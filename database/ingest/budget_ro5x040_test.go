package ingest

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// TestReadCappedBudgetBinds is the RO5X-040 regression.
//
// readCapped's guard was `if budget >= 0 && limit > budget`, so a NEGATIVE
// budget silently stopped applying and the shared O(MaxMessageBytes) bound
// reverted to the per-part `encoded x MaxTransferExpansion` ceiling — the
// exact regression the shared budget exists to prevent. The budget now always
// binds, and an exhausted one is a hard stop.
func TestReadCappedBudgetBinds(t *testing.T) {
	payload := strings.Repeat("A", 4000)
	encoded := base64.StdEncoding.EncodeToString([]byte(payload))

	// A small positive budget caps the output below the 10x ratio ceiling.
	got, err := decodeTransfer([]byte(encoded), "base64", 10, 1000)
	if !errors.Is(err, ErrZipBomb) {
		t.Errorf("decode with a 1000-byte budget: err = %v, want ErrZipBomb (got %d bytes)", err, len(got))
	}

	// A budget that comfortably fits decodes normally.
	got, err = decodeTransfer([]byte(encoded), "base64", 10, 1<<20)
	if err != nil {
		t.Fatalf("decode with an ample budget: %v", err)
	}
	if len(got) != len(payload) {
		t.Errorf("decoded %d bytes, want %d", len(got), len(payload))
	}

	// A ZERO budget is a hard stop, not a fall-through.
	if _, err := decodeTransfer([]byte(encoded), "base64", 10, 0); !errors.Is(err, ErrZipBomb) {
		t.Errorf("zero budget: err = %v, want ErrZipBomb", err)
	}

	// A NEGATIVE budget must NOT disable the bound — this is the trapdoor.
	if _, err := decodeTransfer([]byte(encoded), "base64", 10, -1); !errors.Is(err, ErrZipBomb) {
		t.Errorf("negative budget: err = %v, want ErrZipBomb — "+
			"a negative budget used to disable the shared bound entirely (RO5X-040)", err)
	}
}

// TestPassthroughEncodingsChargeBudget covers the second half: 7bit/8bit/
// binary and unknown encodings returned their input unbudgeted, so a message
// made entirely of such parts could exhaust the shared budget silently.
func TestPassthroughEncodingsChargeBudget(t *testing.T) {
	body := []byte(strings.Repeat("x", 5000))

	for _, enc := range []string{"", "7bit", "8bit", "binary", "x-uuencode"} {
		// Ample budget: passes through unchanged.
		got, err := decodeTransfer(body, enc, 10, 1<<20)
		if err != nil {
			t.Errorf("%q with an ample budget: %v", enc, err)
			continue
		}
		if len(got) != len(body) {
			t.Errorf("%q returned %d bytes, want %d", enc, len(got), len(body))
		}

		// Budget smaller than the body: refused.
		if _, err := decodeTransfer(body, enc, 10, 100); !errors.Is(err, ErrZipBomb) {
			t.Errorf("%q with a 100-byte budget: err = %v, want ErrZipBomb", enc, err)
		}
		// Exhausted budget: refused.
		if _, err := decodeTransfer(body, enc, 10, 0); !errors.Is(err, ErrZipBomb) {
			t.Errorf("%q with a zero budget: err = %v, want ErrZipBomb", enc, err)
		}
		if _, err := decodeTransfer(body, enc, 10, -1); !errors.Is(err, ErrZipBomb) {
			t.Errorf("%q with a negative budget: err = %v, want ErrZipBomb", enc, err)
		}
	}
}

// TestWalkerBudgetNeverGoesNegative pins the clamp in the walker: whatever a
// future decoder does, w.remaining can only ever mean "exhausted", never
// "unbounded".
func TestWalkerBudgetNeverGoesNegative(t *testing.T) {
	payload := strings.Repeat("B", 2000)
	encoded := base64.StdEncoding.EncodeToString([]byte(payload))
	raw := []byte("From: a@b.invalid\r\nSubject: s\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" + encoded + "\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nsecond part\r\n--B--\r\n")

	w := &mimeWalker{
		limits:    DefaultLimits(),
		remaining: 2500, // enough for the first part, not much after
	}
	_, err := w.walk("", "multipart/mixed; boundary=B", "", "", "", extractBody(raw), 0)
	// Either it parses within budget or it trips the bomb guard; what must
	// never happen is a negative remaining, which would re-open the trapdoor.
	if w.remaining < 0 {
		t.Errorf("walker budget went negative (%d); it must clamp at 0 (RO5X-040)", w.remaining)
	}
	_ = err
}

// extractBody returns the body section of a raw message (after the blank line).
func extractBody(raw []byte) []byte {
	if i := strings.Index(string(raw), "\r\n\r\n"); i >= 0 {
		return raw[i+4:]
	}
	return raw
}
