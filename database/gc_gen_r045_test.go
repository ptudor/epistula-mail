package main

import (
	"context"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestGCMarkGenerationNotAheadOfFirstSeen is the R-045 regression: gcMark must
// take its generation from the DB clock, so a freshly inserted candidate's
// generation_marked can never exceed epoch(first_seen_at). If it could (the old
// Go-clock behavior, when the Go clock ran ahead of Postgres), the sweep's
// strict `generation_marked > epoch(first_seen_at)` gate would fire on the very
// first sighting and collapse the required two mark passes into one.
func TestGCMarkGenerationNotAheadOfFirstSeen(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}
	bucket := blob.Bucket("2003/06/15")
	_, path := writeTestBlob(t, store, "gc-gen", bucket, []byte(gcTestMessage))
	backdateFile(t, path, 48*time.Hour) // old enough that mark doesn't skip it

	if code := gcMark(ctx, db, store, 24*time.Hour); code != EX_OK {
		t.Fatalf("gcMark: exit %d", code)
	}

	var gen, firstSeenEpoch int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT generation_marked, floor(extract(epoch FROM first_seen_at))::bigint
		   FROM gc_candidates LIMIT 1`,
	).Scan(&gen, &firstSeenEpoch); err != nil {
		t.Fatalf("read candidate: %v", err)
	}
	if gen > firstSeenEpoch {
		t.Fatalf("generation_marked (%d) > epoch(first_seen_at) (%d): a first-sighting candidate would be sweep-eligible (R-045)", gen, firstSeenEpoch)
	}
}
