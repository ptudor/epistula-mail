-- Large stores may first run these outside a transaction to avoid blocking
-- writers during index removal:
--   DROP INDEX CONCURRENTLY IF EXISTS idx_messages_raw_sha256;
--   DROP INDEX CONCURRENTLY IF EXISTS idx_messages_sha_date;
-- The migration runner itself is transactional.
DROP INDEX IF EXISTS idx_messages_raw_sha256;
DROP INDEX IF EXISTS idx_messages_sha_date;

-- Optional pre-creation for large stores (outside a transaction):
-- CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_deleted_folder
--   ON messages (folder_id) WHERE '\Deleted' = ANY(flags);
CREATE INDEX IF NOT EXISTS idx_messages_deleted_folder
    ON messages (folder_id) WHERE '\Deleted' = ANY(flags);

-- Historical rows retain name fallback. These are historical identities, not
-- foreign keys: deleting a folder must not erase which identity a batch used.
ALTER TABLE archive_moves ADD COLUMN IF NOT EXISTS from_folder_id BIGINT;
ALTER TABLE archive_moves ADD COLUMN IF NOT EXISTS to_folder_id BIGINT;
