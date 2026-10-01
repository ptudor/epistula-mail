-- Folder redirects: where a merged folder's mail goes now.
--
-- `admin folder-merge` empties one folder, or a folder and its subtree, into
-- another ("Sent", "Sent/2005", "Sent/pre-2008" into the \Sent folder
-- "Sent Messages"), and `admin folder-prune-empty` then removes the emptied
-- sources. A later Maildir sync walks the same legacy folders again. Without
-- a record of the merge, `import` would recreate "Sent/2005" and, because it
-- deduplicates within its target folder only, store every message the merge
-- had already moved a second time.
--
-- folder_redirects is that record. Each merge writes one row: its source,
-- whether it covered the subtree, its destination and its batch. `import`
-- consults it only when its target folder does not exist, so a folder that
-- is still there (or that an operator re-creates on purpose) is imported into
-- as named. `admin archive-undo` of the merge batch deletes the row, as it
-- restores the moved messages. The table holds folder names, never message
-- content, and only the owner reads or writes it: merges, undo and import all
-- run as epistula-database.
CREATE TABLE folder_redirects (
    mailbox_id  BIGINT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
    from_folder TEXT NOT NULL,
    subtree     BOOLEAN NOT NULL,
    to_folder   TEXT NOT NULL,
    batch       TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (mailbox_id, from_folder, batch),
    CONSTRAINT folder_redirects_distinct_chk CHECK (from_folder <> to_folder)
);

-- Merges made before this migration are recovered from their journal:
-- folder-merge is the only writer of `merge-<UTC timestamp>` batches. Whether
-- a merge covered its subtree is not journaled, so each source folder that
-- had a message moved becomes an exact-name redirect of its own. A source
-- whose every message was dropped as a duplicate left no journal row and
-- cannot be recovered here.
INSERT INTO folder_redirects (mailbox_id, from_folder, subtree, to_folder, batch, created_at)
SELECT mailbox_id, from_folder, false, to_folder, batch, min(moved_at)
  FROM archive_moves
 WHERE batch LIKE 'merge-%' AND undone_at IS NULL AND from_folder <> to_folder
 GROUP BY mailbox_id, from_folder, to_folder, batch
ON CONFLICT DO NOTHING;
