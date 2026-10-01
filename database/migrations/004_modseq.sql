-- CONDSTORE (RFC 7162) per-message modification sequence.
--
-- folders.highest_modseq is the monotonically-increasing counter for the
-- folder; every write that touches a message (INSERT, flag change,
-- DELETE accounted for as a "vanished" event) does an atomic
-- UPDATE ... RETURNING highest_modseq+1 and stamps that value on the
-- affected rows.
--
-- messages.mod_seq carries the highest_modseq value the row was last
-- written at; SEARCH MODSEQ and FETCH MODSEQ both read it; QRESYNC
-- clients use it to skip rows they've already synced.
--
-- Defaulting both to 1 keeps any pre-existing row addressable as
-- "modseq 1" without a back-fill.
-- New writes use the bump-and-stamp pattern from the application layer.

ALTER TABLE folders
    ADD COLUMN IF NOT EXISTS highest_modseq BIGINT NOT NULL DEFAULT 1;

ALTER TABLE messages
    ADD COLUMN IF NOT EXISTS mod_seq BIGINT NOT NULL DEFAULT 1;

-- SEARCH MODSEQ scans by (folder_id, mod_seq); FETCH MODSEQ reads
-- mod_seq for messages already located via folder_id+uid (covered by
-- the existing unique index), so no additional index needed for FETCH.
CREATE INDEX IF NOT EXISTS idx_messages_folder_modseq
    ON messages (folder_id, mod_seq);
