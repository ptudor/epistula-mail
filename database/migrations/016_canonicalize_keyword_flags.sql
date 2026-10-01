-- RA6X-028: give every keyword flag one identity, merging existing case aliases.
--
-- Flag names are case-insensitive — ALL of them, not only the five system
-- flags (RFC 9051 §2.3.2, RFC 9007 §1.2). Migration 011 canonicalized the
-- system flags and deliberately left keywords alone, on the understanding that
-- keywords were case-sensitive. They are not, so `$Junk` and `$junk` became two
-- separate flags: a client could not reliably clear or find a keyword written
-- in another spelling, and a message could carry both at once.
--
-- The write seam now stores one spelling per flag (imapflags.Canonical): the
-- five system flags and the well-known keywords keep their conventional
-- capitalisation, and any other keyword is stored lowercased. This back-fills
-- rows written before that.
--
-- ============================================================================
-- OPERATOR NOTE — THIS MIGRATION REWRITES ROWS IN `messages`, AND BUMPS mod_seq.
--
-- Like migration 011 this is a DATA migration. The WHERE clause touches only
-- rows that actually carry a non-canonical keyword, so a store whose keywords
-- were always written in one case updates nothing. Check the blast radius:
--
--   SELECT count(*) FROM messages m
--    WHERE EXISTS (SELECT 1 FROM unnest(m.flags) f
--                   WHERE f <> mail_canonical_flag(f));
--
-- mod_seq is bumped on every row this changes, and the owning folder's
-- highest_modseq with it. That is deliberate: a CONDSTORE client that has
-- cached flags for these messages must be told they changed, and the modseq is
-- the only channel that says so. Clients will re-fetch the affected flags.
--
-- If the count is large, run the UPDATE yourself in batches during a
-- maintenance window rather than inside `migrate up`.
-- ============================================================================

-- The canonicalization rule, as SQL. It must agree with imapflags.Canonical;
-- the well-known list is duplicated here rather than imported because a
-- migration has to be self-contained, and the parity is asserted by a test.
CREATE OR REPLACE FUNCTION mail_canonical_flag(f TEXT) RETURNS TEXT AS $$
  SELECT COALESCE(
    (SELECT k FROM unnest(ARRAY[
        '\Seen', '\Answered', '\Flagged', '\Deleted', '\Draft',
        '$Forwarded', '$Junk', '$NotJunk', '$Phishing', '$MDNSent',
        '$Submitted', '$Important', '$Answered', '$Flagged', '$Redirected',
        '$Unsubscribed', '$Ignored', '$Muted', '\Recent'
     ]) AS k WHERE lower(k) = lower(f) LIMIT 1),
    lower(f))
$$ LANGUAGE SQL IMMUTABLE;

-- Bump the owning folders' highest_modseq FIRST, so every rewritten message can
-- be stamped with a value at or below it and the folder's invariant
-- (highest_modseq >= max(mod_seq)) holds throughout.
UPDATE folders f
   SET highest_modseq = highest_modseq + 1
 WHERE EXISTS (
   SELECT 1 FROM messages m
    WHERE m.folder_id = f.id
      AND EXISTS (SELECT 1 FROM unnest(m.flags) fl WHERE fl <> mail_canonical_flag(fl))
 );

UPDATE messages m
   SET flags = (
         SELECT COALESCE(array_agg(DISTINCT mail_canonical_flag(fl) ORDER BY mail_canonical_flag(fl)), '{}')
           FROM unnest(m.flags) AS fl
       ),
       mod_seq = (SELECT highest_modseq FROM folders WHERE id = m.folder_id)
 WHERE EXISTS (
   SELECT 1 FROM unnest(m.flags) fl WHERE fl <> mail_canonical_flag(fl)
 );

-- DISTINCT above is what MERGES a pre-existing case alias: a message carrying
-- both `$Junk` and `$junk` ends with one `$Junk`, and unrelated flags on the
-- same message are untouched because they canonicalize to themselves.
