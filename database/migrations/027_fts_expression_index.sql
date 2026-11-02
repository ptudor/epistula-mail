-- migrate: no-transaction
--
-- Full-text search over an expression index instead of a stored tsvector.
--
-- messages.fts was a STORED generated column: the vector was computed at
-- INSERT, written to the heap (TOASTed), and then indexed by idx_messages_fts.
-- Nothing ever read the column itself. Every query asked `fts @@ tsquery`,
-- which the GIN index answers on its own; the stored copy was only ever
-- consulted for the recheck of a lossy bitmap page, which a plainto_tsquery
-- search never needs. On a production store of 630K messages (12 GB) the
-- stored vectors took 2.6 GB of TOAST, 21% of the database, duplicating what
-- the 2.1 GB index already held.
--
-- The vector is now the expression of an IMMUTABLE SQL function, mail_fts,
-- and the GIN index is built on that expression. A query that writes
-- `mail_fts(m.subject, m.text_body) @@ plainto_tsquery(...)` matches the
-- index: the planner inlines the function on both sides before comparing
-- them. The function is the single definition of what full-text search
-- covers, so a reader cannot drift from the index by retyping the expression.
--
-- The bound is unchanged from migration 014: the first 200,000 characters of
-- subject and of text_body, so a vector can never exceed PostgreSQL's 1 MiB
-- tsvector cap (see that migration for the arithmetic). A row whose text runs
-- past the bound is stored and retrievable; its later text is not searchable.
--
-- Cost moved, not added: a non-HOT UPDATE of a message row (every flag
-- change, since mod_seq is indexed) now recomputes the vector to insert the
-- row's new GIN entries, where it used to copy the stored vector. to_tsvector
-- over the median 3.7 KB body is on the order of a hundred microseconds; the
-- GIN entries were being written either way.
--
-- ---------------------------------------------------------------------------
-- OPERATOR NOTE
--
-- This migration runs outside a transaction so the index can be built
-- CONCURRENTLY: delivery, IMAP and the API keep working while it builds.
-- On a store of hundreds of thousands of messages expect the build to take
-- on the order of ten to thirty minutes; `migrate up -timeout 0` disables the
-- run's time bound. Raise maintenance_work_mem for the duration if you can
-- (the runner cannot SET it for you; set it in postgresql.conf and reload, or
-- accept the slower build).
--
-- This migration is ADDITIVE. The stored column and its old index are dropped
-- by migration 030, after the binaries that use mail_fts are deployed:
--
--   epistula-database migrate up -to 29    # 027, 028, 029: additive
--   (deploy epistula-database, epistula-imap, epistula-api)
--   epistula-database migrate up           # 030: drop what only the old code used
--
-- If an earlier run was interrupted mid-build, the runner drops the INVALID
-- index it left behind and builds it again.
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION mail_fts(subject TEXT, text_body TEXT) RETURNS TSVECTOR
LANGUAGE sql IMMUTABLE PARALLEL SAFE
RETURN setweight(to_tsvector('simple', left(coalesce(subject, ''), 200000)), 'A') ||
       setweight(to_tsvector('simple', left(coalesce(text_body, ''), 200000)), 'B');

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_search
    ON messages USING GIN (mail_fts(subject, text_body));
