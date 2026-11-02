-- migrate: no-transaction
--
-- The annotation pass queue carries the mailbox, so a scoped reader costs
-- what ITS queue holds.
--
-- Migration 022 keyed annotation_pass_required by message_id alone, and
-- epistula-api applied the token's scope only after joining the queue to
-- messages and folders: the whole queue, then the scope. A marker is
-- permanent until a worker finishes the message, so a mailbox nobody
-- annotates keeps its markers forever; on a production store the queue held
-- 357K markers, almost all for mailboxes imported after migration 022 that no
-- worker annotates, and a worker scoped to one mailbox paid 1.5 s and 1.17M
-- buffers per round to learn that nothing of its own was queued. The queue's
-- size is not a bug; reading all of it for one mailbox was.
--
-- The mailbox is immutable for a message id: folders belong to one mailbox,
-- the in-place move keeps the id within it, and COPY and MOVE create a new
-- id. It is therefore safe to denormalize, and it needs no foreign key of its
-- own: the row already cascades with its message.
--
-- Additive. The column is nullable until migration 030, after the binaries
-- that write it are deployed; a marker written by an older binary in between
-- is backfilled there.

SET lock_timeout = '10s';
ALTER TABLE annotation_pass_required ADD COLUMN IF NOT EXISTS mailbox_id BIGINT;
RESET lock_timeout;

UPDATE annotation_pass_required p
   SET mailbox_id = f.mailbox_id
  FROM messages m
  JOIN folders f ON f.id = m.folder_id
 WHERE p.message_id = m.id AND p.mailbox_id IS NULL;

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_annotation_pass_required_mailbox
    ON annotation_pass_required (mailbox_id, message_id);
