package imapsess

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// explainStatus runs statusCountsQuery under EXPLAIN ANALYZE with sequential
// scans disabled, so the plan shows which index each count can use. With
// seqscan allowed, a five-row fixture would be scanned regardless and the test
// would prove nothing about the predicates.
func explainStatus(t *testing.T, s *Session, folderID int64, unseen, deleted, size bool) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := s.be.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, `EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, SUMMARY OFF) `+statusCountsQuery,
		folderID, unseen, deleted, size)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan.String()
}

// TestStatusCountsUseThePartialIndexes pins the contract between
// statusCountsQuery and epistula-database migrations 024 and 028: the UNSEEN and DELETED
// predicates are written exactly as the partial indexes' predicates, so the
// planner can prove the implication and count from the index. A rephrasing
// on either side — `NOT (flags @> ARRAY['\Seen'])`, say — would still be
// correct and would silently fall back to scanning the folder's rows, which is
// what this replaced.
func TestStatusCountsUseThePartialIndexes(t *testing.T) {
	sess, _, folderID := newStatusFixture(t)

	plan := explainStatus(t, sess, folderID, true, true, true)
	for _, idx := range []string{"idx_messages_unseen_folder", "idx_messages_deleted_folder"} {
		if !strings.Contains(plan, idx) {
			t.Errorf("STATUS plan does not use %s:\n%s", idx, plan)
		}
	}
}

// TestStatusSkipsAggregatesNotRequested: the CASE arms keep the unrequested
// subqueries from running. SIZE is the one that still reads every row of the
// folder, and a client asking only for MESSAGES and UNSEEN must not pay it.
func TestStatusSkipsAggregatesNotRequested(t *testing.T) {
	sess, _, folderID := newStatusFixture(t)

	plan := explainStatus(t, sess, folderID, false, false, false)
	// A custom plan folds the false CASE arm away entirely; a generic plan
	// keeps the subplan and reports it never executed. Either is the point.
	for _, idx := range []string{"idx_messages_unseen_folder", "idx_messages_deleted_folder"} {
		if strings.Contains(plan, idx) && !strings.Contains(plan, "never executed") {
			t.Errorf("unrequested %s count was executed:\n%s", idx, plan)
		}
	}
	if strings.Contains(plan, "raw_size") && !strings.Contains(plan, "never executed") {
		t.Errorf("SIZE aggregate ran although SIZE was not requested:\n%s", plan)
	}
}
