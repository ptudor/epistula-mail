-- Drop what only the pre-027 binaries used. Run this AFTER deploying the
-- epistula-database, epistula-imap and epistula-api binaries built with
-- migrations 027-029 (`migrate up -to 29`, deploy, `migrate up`): the old
-- binaries still name both columns in their INSERTs, so dropping them first
-- would defer every delivery and fail every IMAP COPY until the deploy.
--
-- messages.fts: replaced by the mail_fts expression index (migration 027).
--   Dropping the column drops idx_messages_fts with it.
--
-- messages.html_body: the decoded text/html part, stored at ingest and read
--   by nobody. On a production store of 630K messages it was 2.9 GB of TOAST,
--   24% of the database, the largest single consumer, for 68% of messages.
--   The raw message is the source of truth and already on disk; epistula-api
--   now derives html_body from it on demand (one blob read and parse for a
--   GET that asks for it; its MCP consumer never did), and IMAP SEARCH
--   BODY/TEXT searches the text projection, which for an HTML-only message is
--   the HTML's text. The IMAP FETCH path never read the column: body sections
--   come from the blob. See DATABASE_OPERATIONS.md at the repository root for
--   the trade-off and the numbers.
--
-- annotation_pass_required.mailbox_id becomes NOT NULL now that every writer
--   fills it (migration 029).
--
-- ---------------------------------------------------------------------------
-- OPERATOR NOTE: space is not returned to the filesystem by this migration.
--
-- DROP COLUMN is instant: it marks the column dropped and leaves the stored
-- values in place. The TOAST the two columns occupy (5.5 GB on the store
-- above) is freed only when the rows are rewritten. `VACUUM FULL messages`
-- does that, holding an exclusive lock for the duration (deliveries defer,
-- IMAP waits) and needing free space for the rewritten table (about half the
-- table's current size). pg_repack does the same online. Until one of them
-- runs, plain VACUUM keeps the dead space reusable inside the table, so the
-- database stops growing for as long as it takes new mail to fill it; it does
-- not shrink. The runbook is in DATABASE_OPERATIONS.md.
-- ---------------------------------------------------------------------------

SET LOCAL lock_timeout = '10s';

UPDATE annotation_pass_required p
   SET mailbox_id = f.mailbox_id
  FROM messages m
  JOIN folders f ON f.id = m.folder_id
 WHERE p.message_id = m.id AND p.mailbox_id IS NULL;

ALTER TABLE annotation_pass_required ALTER COLUMN mailbox_id SET NOT NULL;

ALTER TABLE messages DROP COLUMN IF EXISTS fts;
ALTER TABLE messages DROP COLUMN IF EXISTS html_body;
