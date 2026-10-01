-- R-062: a shared, collision-proof source for folders.uidvalidity.
--
-- Both the LDA writer (storage) and the IMAP server (epistula-imap — a
-- separate Go module against the same schema) previously seeded uidvalidity
-- from time.Now().Unix(). A folder DELETEd via IMAP and recreated within the
-- same wall-clock second by an LDA delivery (or a COPY/MOVE auto-create) reused
-- the identical value with uidnext reset to 1 — the exact (UIDVALIDITY, UID)
-- collision UIDVALIDITY exists to prevent, and cross-process so no single
-- daemon could serialize it. A Postgres sequence is the one coordination point
-- both processes already share; nextval() is atomic and strictly increasing, so
-- every folder create/rename gets a distinct value. Existing folder rows are
-- left untouched (the fix changes only how NEW values are minted).
CREATE SEQUENCE IF NOT EXISTS folder_uidvalidity_seq AS BIGINT;

-- Seed strictly above every existing uidvalidity AND above the current epoch,
-- so the first minted value (seed + 1) can never collide with a value an older
-- binary already wrote from time.Now().Unix() during the rollout window.
SELECT setval('folder_uidvalidity_seq',
    GREATEST(
        (SELECT COALESCE(MAX(uidvalidity), 0) FROM folders),
        EXTRACT(epoch FROM now())::bigint
    ));

-- R-018: /v1/search and /v1/export sort globally by (internal_date DESC,
-- id DESC) across all mailboxes. idx_messages_folder_date only leads on
-- folder_id, so the global sort fell back to a sequential scan + top-N over
-- the whole filtered set — a full export degenerates toward O(N^2/page) and any
-- batch exceeding the 10s statement_timeout aborts the stream. This b-tree
-- matches the ORDER BY exactly, keeping keyset pagination O(page) at any depth.
CREATE INDEX IF NOT EXISTS idx_messages_date_id ON messages (internal_date DESC, id DESC);
