-- RA6X-048: preserve the sender's own calendar date for IMAP SENTON/SENTBEFORE/
-- SENTSINCE.
--
-- RFC 9051 §6.4.4 defines the sent-date criteria against the date written in the
-- Date header, disregarding its time AND its zone. messages.sent_date is a
-- timestamptz, so the original offset is gone by the time it is stored: a header
-- dated `04 Sep 2026 00:30:00 +1400` is September 3 in UTC, and a search for
-- messages sent on September 4 missed it.
--
-- Casting made it worse. `sent_date::date` uses the connection's TimeZone
-- setting, which nothing in this daemon sets, so the same search over the same
-- data returned different results on different connections.
--
-- sent_date_local carries the calendar date the sender wrote. It is nullable,
-- and a NULL is meaningful: the message had no Date header, or an unparseable
-- one, or predates this column. Search falls back to the UTC reduction of
-- sent_date for those, which is exactly what the old comparison meant on a UTC
-- connection — so nothing regresses while the back-fill is outstanding.
--
-- ============================================================================
-- OPERATOR NOTE — THIS MIGRATION DOES NOT BACK-FILL.
--
-- Deliberately. Recovering the sender's offset means re-reading the Date header
-- and parsing RFC 5322, which SQL cannot do safely: PostgreSQL's timestamptz
-- input parser raises on anything it does not recognise, and one malformed
-- header in the archive would abort the whole statement. Real mail carries
-- plenty of malformed Date headers.
--
-- The back-fill is therefore a maintenance pass that uses the same Go parser
-- ingest uses:
--
--   epistula-database reparse-bodystructure -dry-run     # see the blast radius
--   epistula-database reparse-bodystructure              # back-fill
--
-- Until it runs, sent-date search for historical messages behaves as it did
-- before this fix. New mail is correct from the moment the code is deployed.
-- ============================================================================

ALTER TABLE messages ADD COLUMN IF NOT EXISTS sent_date_local DATE;

CREATE INDEX IF NOT EXISTS idx_messages_sent_date_local
    ON messages (sent_date_local) WHERE sent_date_local IS NOT NULL;
