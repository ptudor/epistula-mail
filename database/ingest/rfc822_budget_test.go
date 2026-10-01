package ingest

import (
	"strings"
	"testing"
)

// TestEmbeddedMessageBudgetNotDoubleCharged is the R-027 regression: a
// message/rfc822 container charged the whole embedded message against the
// budget AND then charged it again as its inner parts decoded, so a legitimate
// large forward (more than half the budget) bounced as a "zip bomb". With the
// container charge refunded before recursion, a ~0.6×-budget forward parses.
func TestEmbeddedMessageBudgetNotDoubleCharged(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxMessageBytes = 20000

	// Inner text ~12.6 KB — over half the 20 KB budget. Single charge fits;
	// the old double charge (~25 KB) would exceed it.
	innerBody := strings.Repeat("forwarded content ", 700)
	inner := "From: original@rfc822.invalid\r\n" +
		"Subject: big forward\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" + innerBody + "\r\n"
	raw := buildForward(inner)
	if int64(len(raw)) > limits.MaxMessageBytes {
		t.Fatalf("fixture %d bytes exceeds MaxMessageBytes %d; adjust the test", len(raw), limits.MaxMessageBytes)
	}

	msg, err := New(limits).Parse(raw)
	if err != nil {
		t.Fatalf("Parse of a legit ~0.6x-budget forward failed (R-027 double-charge regression?): %v", err)
	}
	if !strings.Contains(msg.TextBody, "forwarded content") {
		t.Error("embedded message text not extracted into TextBody")
	}
	// It must be a nested structure, not an opaque attachment.
	for _, a := range msg.Attachments {
		if a.ContentType == "message/rfc822" {
			t.Error("embedded message wrongly degraded to an attachment")
		}
	}
}
