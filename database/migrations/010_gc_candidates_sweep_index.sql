-- R-025: back the batched gc sweep's range + keyset pagination.
--
-- gcSweep pages candidates with
--   WHERE first_seen_at < cutoff AND generation_marked > epoch(first_seen_at)
--   ORDER BY first_seen_at, tenant, sha256, kind, bucket
--   LIMIT 1000  (re-queried with a keyset cursor per batch)
-- Without an index on that ordering, every batch re-sorts the entire
-- gc_candidates table — catastrophic when deleting a large mailbox turns
-- millions of blobs into candidates. This composite b-tree serves both the
-- first_seen_at range and the full keyset tuple, so each batch is an ordered
-- index range scan from the cursor. The generation_marked gate is applied as a
-- residual filter on the scanned rows.
CREATE INDEX IF NOT EXISTS idx_gc_candidates_sweep
    ON gc_candidates (first_seen_at, tenant, sha256, kind, bucket);
