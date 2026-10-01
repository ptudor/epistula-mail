package main

import (
	"context"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestGCSweepBatchesAcrossLimit is the R-025 regression: with more eligible
// candidates than one batch (sweepBatchSize=1000), the batched keyset sweep must
// still process every one and terminate. Seeds 1200 unreferenced candidates
// (distinct sha, orphan tenant, old + generation-confirmed) whose blob files
// don't exist — sweep tolerates the missing file, deletes the row — and asserts
// all 1200 candidate rows are cleared. Crosses the LIMIT boundary (1000 + 200),
// proving the cursor covers the whole set without materializing it.
func TestGCSweepBatchesAcrossLimit(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}

	// 1200 distinct-sha candidates: old enough (first_seen_at 2 days ago <
	// cutoff) and generation-confirmed (generation_marked > epoch(first_seen)).
	// Tenant has no mailbox row → unreferenced → reapable. No blob files exist,
	// so each sweep tx unlinks nothing (ENOENT tolerated) and deletes the row.
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO gc_candidates (tenant, sha256, kind, bucket, first_seen_at, generation_marked)
		SELECT 'batch-tenant',
		       decode(lpad(to_hex(g), 64, '0'), 'hex'),
		       'raw', '2003/06/15',
		       now() - interval '2 days',
		       floor(extract(epoch FROM now()))::bigint
		  FROM generate_series(1, 1200) g`); err != nil {
		t.Fatalf("seed candidates: %v", err)
	}

	var before int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM gc_candidates`).Scan(&before); err != nil {
		t.Fatalf("count before: %v", err)
	}
	if before != 1200 {
		t.Fatalf("seeded %d candidates, want 1200", before)
	}

	if code := gcSweep(ctx, db, store, time.Hour); code != EX_OK {
		t.Fatalf("gcSweep exit=%d, want EX_OK", code)
	}

	var after int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM gc_candidates`).Scan(&after); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != 0 {
		t.Fatalf("sweep left %d candidates, want 0 (batched cursor must cover all 1200 across the LIMIT boundary)", after)
	}
}
