-- RA6X-030: make durable storage independent of indexing the whole body.
--
-- messages.fts is a STORED generated column, so its expression runs inside the
-- INSERT that stores the message. PostgreSQL caps a tsvector at 1 MiB, and the
-- parser accepts messages up to 50 MiB — so a message far under the size limit
-- could be REJECTED ENTIRELY because its search vector did not fit:
--
--   SELECT length(to_tsvector('simple', string_agg(md5(g::text), ' ')))
--     FROM generate_series(1,50000) g;
--   ERROR:  string is too long for tsvector (1813306 bytes, max 1048575 bytes)
--
-- That input is about 1.65 MB — a large but entirely legitimate text message,
-- a log excerpt or a mailing-list digest. Delivery bounced it, and once
-- RA6X-008 made SQL failures defer instead of bounce, it would have retried
-- forever instead.
--
-- The fix bounds what is INDEXED, never what is STORED. text_body keeps the
-- whole projection and the raw blob is untouched; only the vector is built
-- from a prefix.
--
-- ============================================================================
-- SEARCH COVERAGE SEMANTICS
--
-- Full-text search (epistula-api /v1/search, and IMAP SEARCH where it narrows
-- candidates) matches within the first 200,000 CHARACTERS of
-- "subject + text_body". Beyond that a message is stored, retrievable and
-- readable, but its later text is not in the index.
--
-- Why 200,000: the limit is 1 MiB of VECTOR, and a worst-case
-- all-unique-token input produces a vector about 1.1x its byte length. At
-- 4 bytes per character (the UTF-8 maximum) 200,000 characters is at most
-- 800 KB of input and therefore about 880 KB of vector — under the cap with
-- margin, for text in any script. Ordinary mail is a few kilobytes, so this
-- boundary is not reachable by normal correspondence.
--
-- RA6X-026 changes IMAP BODY/TEXT search to substring matching against the
-- stored text, where FTS may only narrow candidates and never exclude a valid
-- match, so IMAP search coverage is unaffected by this bound.
-- ============================================================================
--
-- ============================================================================
-- OPERATOR NOTE — THIS MIGRATION REWRITES THE `messages` TABLE.
--
-- A generated column's expression cannot be altered in place, so the column is
-- dropped and re-added. That rewrites every row and rebuilds the GIN index,
-- taking an ACCESS EXCLUSIVE lock for the duration. On a large archive this is
-- a maintenance-window operation, not something to run inside `migrate up`
-- against a live server. Check the size first:
--
--   SELECT pg_size_pretty(pg_total_relation_size('messages'));
--
-- Rows that would have overflowed cannot already exist — their INSERT failed —
-- so the rewrite itself cannot hit the limit.
-- ============================================================================

ALTER TABLE messages DROP COLUMN IF EXISTS fts;

ALTER TABLE messages ADD COLUMN fts TSVECTOR GENERATED ALWAYS AS (
    setweight(to_tsvector('simple', left(coalesce(subject, ''), 200000)), 'A') ||
    setweight(to_tsvector('simple', left(coalesce(text_body, ''), 200000)), 'B')
) STORED;

CREATE INDEX IF NOT EXISTS idx_messages_fts ON messages USING GIN (fts);
