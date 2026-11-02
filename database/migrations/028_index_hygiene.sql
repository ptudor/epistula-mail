-- migrate: no-transaction
--
-- The messages indexes, reconciled with what the readers actually ask for.
-- Measured on a production store (PostgreSQL 16, 630K messages, index scan
-- counts accumulated since the cluster was created):
--
--   idx_messages_headers   741 MB    0 scans   GIN over the headers JSONB. The
--     only query it could serve is IMAP `SEARCH HEADER <name> ""` (field
--     exists), which no client has ever sent; a HEADER search with a value
--     is a LIKE over the field's values and never used it. Every delivery and
--     every flag change paid to index every header name and value.
--   idx_attachments_sha256   3 MB    0 scans   prefix of idx_attachments_sha_date
--     (sha256, blob_date), which is what the GC reference check asks for.
--
-- What the hot path wanted and did not have:
--
--   STATUS. Every mail client polls `STATUS <folder> (MESSAGES UNSEEN)` for
--     every folder it shows, and the server answered with one scan of the
--     folder's rows: 121 ms and 19,308 buffers for a 46K-message INBOX,
--     against 9 ms and 231 buffers for an index-only count. The folder
--     scan was the single largest read on the database: 17M scans of
--     idx_messages_folder_modseq fetching 640M heap tuples. A partial index
--     makes UNSEEN an index-only count of the few rows without the flag
--     (idx_messages_deleted_folder, migration 024, already does that for
--     DELETED), and the server now aggregates only what the client asked
--     for (epistula-imap session.go).
--   The session view. SELECT and every reconcile after a folder changes read
--     (uid, mod_seq) for the whole folder in UID order. The UNIQUE
--     (folder_id, uid) index now INCLUDEs mod_seq, so that read is an
--     ordered index-only scan instead of a heap scan plus sort (84 ms on
--     the same INBOX).
--
-- Kept on purpose: idx_messages_folder_sha serves the delivery dedup lookup
-- (indexes_rm51x007_test.go pins it); idx_messages_folder_id_id serves the
-- live sorter's candidate scan (migration 026); idx_messages_message_id has
-- no reader today but is the natural key for threading and costs 65 MB.
--
-- Runs outside a transaction so every build and drop is CONCURRENTLY: no
-- lock that stops delivery or IMAP. The UNIQUE constraint swap is the one
-- step that takes an exclusive lock on messages, for milliseconds, bounded
-- by lock_timeout; a timeout fails the run and it is simply rerun.

-- (1) The covering UNIQUE index, built first so the swap below is instant.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS messages_folder_uid_key
    ON messages (folder_id, uid) INCLUDE (mod_seq);

SET lock_timeout = '10s';

-- Attach it as the constraint, replacing the original. A single DO block so
-- the folder is never without a UNIQUE (folder_id, uid) constraint, and
-- guarded so a rerun after it committed is a no-op.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'messages'::regclass AND conname = 'messages_folder_uid_key'
    ) THEN
        ALTER TABLE messages DROP CONSTRAINT IF EXISTS messages_folder_id_uid_key;
        ALTER TABLE messages ADD CONSTRAINT messages_folder_uid_key
            UNIQUE USING INDEX messages_folder_uid_key;
    END IF;
END
$$;

RESET lock_timeout;

-- (2) STATUS: UNSEEN as an index-only count of the unread few.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_unseen_folder
    ON messages (folder_id) WHERE NOT ('\Seen' = ANY(flags));

-- (3) Indexes nothing reads. One per statement: DROP INDEX CONCURRENTLY
--     takes exactly one name.
DROP INDEX CONCURRENTLY IF EXISTS idx_messages_headers;
DROP INDEX CONCURRENTLY IF EXISTS idx_attachments_sha256;
