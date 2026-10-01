package storage

import (
	"context"
	"hash/fnv"
	"sort"

	"github.com/jackc/pgx/v5"
)

// BlobAdvisoryLockKey derives the pg_advisory_xact_lock key for one
// content-addressed blob. Ingest and `gc sweep` both lock this key around
// their blob-referencing work, which is what makes the GC race-free:
//
//   - Ingest takes the lock inside its transaction, deletes any
//     gc_candidates row for the blob, and re-verifies (via
//     IngestParams.EnsureBlobs) that the file is still on disk —
//     rewriting it if a sweep won the race before the lock was taken.
//   - Sweep takes the lock inside its per-candidate transaction around
//     the reference re-check and the unlink.
//
// Whichever side acquires first, the other observes a consistent state:
// sweep either sees the committed row (and resurrects the candidate) or
// ingest sees the unlinked file (and rewrites it). A hash collision
// between distinct blobs only causes spurious serialization, never a
// correctness problem.
//
// The key is keyed on the tenant as well as (kind, bucket, sha): blobs are
// physically isolated per mailbox, so the same content+date in two mailboxes
// is two distinct files that must serialize independently. The tenant string
// MUST be byte-identical on both sides — it is the canonical mailboxes.name,
// supplied by ingest via IngestParams.MailboxName and read by sweep from the
// candidate row / on-disk directory name.
func BlobAdvisoryLockKey(tenant, kind, bucket, shaHex string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(tenant))
	_, _ = h.Write([]byte{'/'})
	_, _ = h.Write([]byte(kind))
	_, _ = h.Write([]byte{'/'})
	_, _ = h.Write([]byte(bucket))
	_, _ = h.Write([]byte{'/'})
	_, _ = h.Write([]byte(shaHex))
	return int64(h.Sum64())
}

// acquireBlobLocks takes pg_advisory_xact_lock on every key, sorted and
// deduplicated so concurrent multi-blob ingests acquire in a stable order
// and cannot deadlock against each other (sweep only ever takes one key
// per transaction). Locks release automatically at commit/rollback.
func acquireBlobLocks(ctx context.Context, tx pgx.Tx, keys []int64) error {
	if len(keys) == 0 {
		return nil
	}
	sorted := make([]int64, len(keys))
	copy(sorted, keys)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var prev int64
	for i, k := range sorted {
		if i > 0 && k == prev {
			continue
		}
		prev = k
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, k); err != nil {
			return err
		}
	}
	return nil
}
