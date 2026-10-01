-- Add date-partitioned blob paths.
--
-- Blobs on disk move from raw/aa/bb/<sha>.eml to
-- raw/yyyy/mm/dd/aa/bb/<sha>.eml (same change for att/). The bucket date
-- is recorded in the database so reads don't have to guess the path.
--
-- For new deliveries the bucket is wall-clock arrival time; for imports
-- it's the message's Date header (falling back to file mtime).
-- Pre-existing rows are defaulted to CURRENT_DATE — if the deployment
-- already had blobs at the un-dated layout, operators must either rebuild
-- the store or relocate blobs to match.

ALTER TABLE messages
    ADD COLUMN IF NOT EXISTS raw_blob_date DATE NOT NULL DEFAULT CURRENT_DATE;

ALTER TABLE attachments
    ADD COLUMN IF NOT EXISTS blob_date DATE NOT NULL DEFAULT CURRENT_DATE;

-- gc_candidates needs the bucket too, otherwise the sweeper can't construct
-- the on-disk path. The composite primary key gains the bucket column.
ALTER TABLE gc_candidates
    ADD COLUMN IF NOT EXISTS bucket TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY/MM/DD');

ALTER TABLE gc_candidates
    DROP CONSTRAINT IF EXISTS gc_candidates_pkey;

ALTER TABLE gc_candidates
    ADD PRIMARY KEY (sha256, kind, bucket);

-- Indices to support resolver/reader lookups by (sha, date).
CREATE INDEX IF NOT EXISTS idx_messages_sha_date
    ON messages (raw_sha256, raw_blob_date);
CREATE INDEX IF NOT EXISTS idx_attachments_sha_date
    ON attachments (sha256, blob_date);
