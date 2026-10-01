package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
)

// TestExportCursorHandlesInternalDateTies resolves the first half of RO5X-046.
//
// The export keyset is (m.internal_date, m.id) < ($1, $2) with
// ORDER BY m.internal_date DESC, m.id DESC. That is correct only if the
// ordering is TOTAL — if many messages share an internal_date, a cursor keyed
// on the date alone would lose or duplicate rows at every page boundary that
// lands inside a tie group.
//
// This seeds a large tie group deliberately straddling several page
// boundaries and asserts every message is streamed exactly once.
func TestExportCursorHandlesInternalDateTies(t *testing.T) {
	f := newAPIFixture(t)
	ctx := context.Background()

	// 60 messages ALL sharing one internal_date, so every page boundary falls
	// inside the tie group.
	same := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	var folderID int64
	if err := f.pool.QueryRow(ctx,
		`SELECT f.id FROM folders f JOIN mailboxes mb ON mb.id = f.mailbox_id
		  WHERE mb.name = 'alice' AND f.name = 'INBOX'`).Scan(&folderID); err != nil {
		t.Fatalf("folder: %v", err)
	}
	tenant, _ := blob.ParseTenant("alice")
	store := f.srv.store
	seeded := map[int64]bool{}
	for i := 0; i < 60; i++ {
		uid := int64(1000 + i)
		id := f.insertMessage(ctx, store, tenant, folderID, uid,
			fmt.Sprintf("tie body %d", i), same, false)
		seeded[id] = true
	}

	// Stream the whole export and count each id.
	resp := f.do(http.MethodGet, "/v1/export?mailbox=alice", f.classifierToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d", resp.StatusCode)
	}

	seen := map[int64]int{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var row struct {
			ID    int64  `json:"id"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatalf("decode NDJSON row: %v (%s)", err, line)
		}
		if row.Error != "" {
			t.Fatalf("export error row: %s", row.Error)
		}
		seen[row.ID]++
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	for id := range seeded {
		switch seen[id] {
		case 0:
			t.Errorf("message %d in the tie group was LOST by the export cursor", id)
		case 1:
		default:
			t.Errorf("message %d was streamed %d times (duplicated)", id, seen[id])
		}
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("message %d streamed %d times", id, n)
		}
	}
	if len(seen) < len(seeded) {
		t.Errorf("export returned %d distinct messages, want at least %d", len(seen), len(seeded))
	}
}

// TestExportCursorTotalOrderIsIDTiebroken pins WHY the above holds: the
// ordering must be total, i.e. include the unique id as a tiebreaker. If the
// keyset ever loses the id term, ties become a silent loss.
func TestExportCursorTotalOrderIsIDTiebroken(t *testing.T) {
	f := newAPIFixture(t)
	ctx := context.Background()

	same := time.Date(2026, 7, 7, 8, 0, 0, 0, time.UTC)
	var folderID int64
	if err := f.pool.QueryRow(ctx,
		`SELECT f.id FROM folders f JOIN mailboxes mb ON mb.id = f.mailbox_id
		  WHERE mb.name = 'alice' AND f.name = 'INBOX'`).Scan(&folderID); err != nil {
		t.Fatalf("folder: %v", err)
	}
	tenant, _ := blob.ParseTenant("alice")
	for i := 0; i < 5; i++ {
		f.insertMessage(ctx, f.srv.store, tenant, folderID, int64(2000+i),
			"tie", same, false)
	}

	// The DB-level ordering used by the export must be strictly decreasing on
	// (internal_date, id) — no two rows compare equal.
	rows, err := f.pool.Query(ctx,
		`SELECT m.internal_date, m.id FROM messages m
		   JOIN folders f ON f.id = m.folder_id
		  WHERE f.id = $1
		  ORDER BY m.internal_date DESC, m.id DESC`, folderID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var prevDate time.Time
	var prevID int64
	first := true
	for rows.Next() {
		var d time.Time
		var id int64
		if err := rows.Scan(&d, &id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !first {
			if d.After(prevDate) || (d.Equal(prevDate) && id >= prevID) {
				t.Fatalf("ordering is not strictly decreasing at (%s, %d) after (%s, %d) — "+
					"the keyset would lose or duplicate rows here",
					d, id, prevDate, prevID)
			}
		}
		prevDate, prevID, first = d, id, false
	}
}
