package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestExportStaysCompleteUnderAByteBudget is the RA6X-040 regression.
//
// Export buffered a full page of complete text bodies plus every annotation row
// before writing its first byte: with the default page of 500 and messages
// accepted up to 50 MiB, one legitimate request could ask for tens of gigabytes,
// and the two-stream semaphore caps requests rather than their size.
//
// The bound is on retained BYTES. What must not change is completeness: every
// row still arrives exactly once, in the same order.
func TestExportStaysCompleteUnderAByteBudget(t *testing.T) {
	f := newAPIFixture(t)

	// A budget far smaller than one page of these messages, so the export is
	// forced through many byte-bounded batches.
	f.srv.cfg.Limits.MaxPageBytes = 64
	f.srv.cfg.Limits.ExportPageSize = 500

	resp := f.do(http.MethodGet, "/v1/export", f.classifierToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}

	seen := map[int64]int{}
	var order []int64
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("undecodable NDJSON line %q: %v", line, err)
		}
		if _, isErr := row["error"]; isErr {
			t.Fatalf("export reported an in-band error: %s", line)
		}
		id := int64(row["id"].(float64))
		seen[id]++
		order = append(order, id)
	}

	// Every fixture message exactly once: 7 of alice's plus 1 of bob's.
	if len(seen) != 8 {
		t.Fatalf("export returned %d distinct messages, want 8", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("message %d appeared %d times; a byte-bounded batch must not duplicate rows", id, n)
		}
	}
	// Ordering is unchanged: internal_date DESC, id DESC.
	for i := 1; i < len(order); i++ {
		if order[i] > order[i-1] {
			// ids are assigned in ascending insert order, and the fixture's
			// dates ascend with them, so a strictly descending id sequence is
			// the expected order here.
			t.Errorf("ordering broke at position %d: %v", i, order)
			break
		}
	}
}

// TestExportAdmitsARowLargerThanTheBudget pins the forward-progress rule: a
// message bigger than the whole budget must still be exported, not stall the
// stream forever.
func TestExportAdmitsARowLargerThanTheBudget(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.cfg.Limits.MaxPageBytes = 1 // smaller than any real row
	f.srv.cfg.Limits.ExportPageSize = 500

	resp := f.do(http.MethodGet, "/v1/export", f.classifierToken, nil)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	lines := 0
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if line != "" {
			lines++
		}
	}
	if lines != 8 {
		t.Fatalf("export produced %d rows under a 1-byte budget, want all 8", lines)
	}
}

// TestPageBudgetAdmitsTheFirstRowUnconditionally pins the rule directly.
func TestPageBudgetAdmitsTheFirstRowUnconditionally(t *testing.T) {
	b := newPageBudget(10)
	if !b.admit(1_000_000) {
		t.Fatal("the first row of a page must always be admitted, however large")
	}
	if b.admit(1) {
		t.Fatal("a second row must be refused once the budget is spent")
	}
}

// TestFolderListingWithTextIsByteBounded pins that a text-enabled list page is
// bounded too, and still hands back a cursor so the client can continue.
func TestFolderListingWithTextIsByteBounded(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.cfg.Limits.MaxPageBytes = 64
	f.srv.cfg.Limits.DefaultPageSize = 500
	f.srv.cfg.Limits.MaxPageSize = 500

	resp := f.do(http.MethodGet,
		"/v1/mailboxes/alice/folders/INBOX/messages?fields=text&limit=500",
		f.aliceContentToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("listing = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Messages   []map[string]any `json:"messages"`
		NextCursor string           `json:"next_cursor"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Messages) == 0 {
		t.Fatal("a byte-bounded page returned nothing")
	}
	if len(out.Messages) >= 7 {
		t.Fatalf("the byte budget did not bound the page: %d messages", len(out.Messages))
	}
	if out.NextCursor == "" {
		t.Fatal("a page cut short by the byte budget must still hand back a cursor")
	}
}
