-- RO5X-013: canonicalize the case of IMAP system flags already in messages.flags.
--
-- System flags are case-insensitive per RFC 9051 §2.3.2, but before R-063 the
-- IMAP server persisted whatever case the client sent, so a client issuing
-- `+FLAGS (\SEEN)` left the literal `\SEEN` in the array. Every comparison
-- downstream is byte-exact, so such a row was invisible to `SEARCH SEEN` and to
-- epistula-api's `flag=\Seen`, and *visible* to `not_flag=\Seen` — the message
-- showed as permanently unread, and the LLM worker and the MCP disagreed with
-- the user's mail client about the same message.
--
-- R-063 fixed the write seam and the readers are now canonicalized too, but
-- neither back-fills existing rows; this does.
--
-- Keyword flags ($Forwarded, $Junk, NonJunk, …) are case-SENSITIVE and are left
-- exactly as they are. Only the five system flags are rewritten.
--
-- ============================================================================
-- OPERATOR NOTE — THIS MIGRATION REWRITES ROWS IN `messages`.
--
-- Unlike every migration before it, this is a DATA migration, not DDL. On a
-- large archive it will rewrite a substantial fraction of the table, take a
-- ROW EXCLUSIVE lock for the duration, and generate WAL proportional to the
-- number of rows touched. `migrate up`'s default one-hour budget is NOT a
-- promise that this finishes inside it.
--
-- The WHERE clause below touches ONLY rows that actually carry a
-- non-canonical system flag, so on a store that has only ever been written by
-- a post-R-063 server this is a cheap indexed-free scan that updates nothing.
-- Check the blast radius before running it:
--
--   SELECT count(*) FROM messages m
--    WHERE EXISTS (
--      SELECT 1 FROM unnest(m.flags) f
--       WHERE lower(f) IN ('\seen','\answered','\flagged','\deleted','\draft')
--         AND f <> CASE lower(f)
--                    WHEN '\seen'     THEN '\Seen'
--                    WHEN '\answered' THEN '\Answered'
--                    WHEN '\flagged'  THEN '\Flagged'
--                    WHEN '\deleted'  THEN '\Deleted'
--                    WHEN '\draft'    THEN '\Draft'
--                  END);
--
-- If that count is large, run the UPDATE yourself in batches during a
-- maintenance window rather than inside `migrate up`.
-- ============================================================================

UPDATE messages m
   SET flags = (
     SELECT coalesce(array_agg(
              CASE lower(f)
                WHEN '\seen'     THEN '\Seen'
                WHEN '\answered' THEN '\Answered'
                WHEN '\flagged'  THEN '\Flagged'
                WHEN '\deleted'  THEN '\Deleted'
                WHEN '\draft'    THEN '\Draft'
                ELSE f
              END
              ORDER BY ord), '{}')
       FROM unnest(m.flags) WITH ORDINALITY AS t(f, ord))
 WHERE EXISTS (
   -- Touch only rows that actually need it: some element is a system flag
   -- (case-insensitively) whose spelling differs from the canonical form.
   SELECT 1 FROM unnest(m.flags) f
    WHERE lower(f) IN ('\seen', '\answered', '\flagged', '\deleted', '\draft')
      AND f <> CASE lower(f)
                 WHEN '\seen'     THEN '\Seen'
                 WHEN '\answered' THEN '\Answered'
                 WHEN '\flagged'  THEN '\Flagged'
                 WHEN '\deleted'  THEN '\Deleted'
                 WHEN '\draft'    THEN '\Draft'
               END);

-- Note on ordering: the rewrite preserves each row's original element order
-- (WITH ORDINALITY + ORDER BY ord) rather than using array_agg(DISTINCT ...),
-- which would sort and silently reorder every array it touched. Flag order is
-- not semantically meaningful in IMAP, but preserving it keeps this migration's
-- diff limited to the case changes it is actually here to make.
