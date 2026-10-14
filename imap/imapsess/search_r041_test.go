package imapsess

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// Both result kinds share a flat query; sequence results come from the view.
func TestSearchQueryFlatForm(t *testing.T) {
	flat := searchQuery("uid > $2", " LIMIT $3")
	if strings.Contains(flat, "row_number()") || !strings.Contains(flat, "WHERE folder_id = $1 AND (") || !strings.HasSuffix(flat, " LIMIT $3") {
		t.Fatal(flat)
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
	flatPlan := explain(searchQuery(where, ""))
	if strings.Contains(flatPlan, "WindowAgg") {
		t.Errorf("UID SEARCH BODY plan still has a WindowAgg node:\n%s", flatPlan)
	}
	for _, kind := range []imapserver.NumKind{imapserver.NumKindUID, imapserver.NumKindSeq} {
		if _, err := sess.Search(kind, &imap.SearchCriteria{Body: []string{"urgent"}}, nil); err != nil {
			t.Fatalf("kind %v: %v", kind, err)
		}
	}
}
