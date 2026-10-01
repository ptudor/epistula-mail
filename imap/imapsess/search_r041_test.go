package imapsess

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

// TestSearchQueryWindowSelection is the R-041 unit regression: the UID/no-seqnum
// query must drop the row_number() window (so indexes are usable) while the
// window variant keeps it (so seq positions stay correct).
func TestSearchQueryWindowSelection(t *testing.T) {
	where := "fts @@ plainto_tsquery('simple', $2)"

	windowed := searchQuery(true, where, "")
	if !strings.Contains(windowed, "row_number()") {
		t.Errorf("windowed query must use row_number():\n%s", windowed)
	}

	flat := searchQuery(false, where, "")
	if strings.Contains(flat, "row_number()") {
		t.Errorf("UID/no-seqnum query must NOT use row_number():\n%s", flat)
	}
	if !strings.Contains(flat, "WHERE folder_id = $1 AND (") {
		t.Errorf("UID/no-seqnum query must filter the base table directly:\n%s", flat)
	}
}

// TestSearchUIDBodyAvoidsWindowAgg is the R-041 plan regression: EXPLAIN of the
// query a UID `SEARCH BODY` generates must not contain a WindowAgg node (the
// row_number() window that defeated the fts GIN index). The presence of
// WindowAgg depends only on the query shape, not the row count, so this is
// deterministic on a small fixture.
func TestSearchUIDBodyAvoidsWindowAgg(t *testing.T) {
	sess := searchFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	b := &searchSQL{}
	b.args = append(b.args, sess.selectedFolderID)
	where, err := b.buildCriteria(&imap.SearchCriteria{Body: []string{"urgent"}})
	if err != nil {
		t.Fatalf("buildCriteria: %v", err)
	}

	explain := func(q string) string {
		rows, err := sess.be.Pool.Query(ctx, "EXPLAIN "+q, b.args...)
		if err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		defer rows.Close()
		var sb strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scan plan: %v", err)
			}
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("plan rows: %v", err)
		}
		return sb.String()
	}

	// UID kind, no SeqNum criterion → no window → no WindowAgg node.
	flatPlan := explain(searchQuery(false, where, ""))
	if strings.Contains(flatPlan, "WindowAgg") {
		t.Errorf("UID SEARCH BODY plan still has a WindowAgg node:\n%s", flatPlan)
	}
	// The windowed variant (Seq kind / SeqNum criterion) does carry WindowAgg —
	// the contrast confirms the difference is the window, not something else.
	if wp := explain(searchQuery(true, where, "")); !strings.Contains(wp, "WindowAgg") {
		t.Errorf("windowed query unexpectedly lacks WindowAgg:\n%s", wp)
	}
}
