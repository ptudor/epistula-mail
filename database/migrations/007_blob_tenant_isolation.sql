-- Per-mailbox blob isolation.
--
-- Blobs move from a shared, tenant-blind tree to one partitioned by the owning
-- mailbox: ${root}/<mailbox>/{raw,att}/yyyy/mm/dd/aa/bb/<sha>.ext. The mailbox
-- name is the tenant. Dedup becomes intra-mailbox (one user's many domains still
-- collapse to one blob; two different users' identical bytes are now two files
-- in two subtrees), so each user's corpus is a self-contained directory that
-- can be rsync'd, restored, deleted, or put on its own ZFS dataset
-- independently. This migration carries the database-side changes:
--
--   1. gc_candidates gains a `tenant` column and includes it in the primary
--      key, so `gc sweep` can reconstruct the per-tenant on-disk path and two
--      mailboxes holding identical content on the same day are tracked (and
--      reaped) separately.
--   2. mailboxes.name is constrained to the canonical, path-safe tenant charset
--      so no writer (the admin CLI, an external administration tool, or a direct
--      psql session) can create a name that is not a legal directory component —
--      this is the database-level guarantee behind the path-traversal defense.
--   3. A covering index supports the GC mark/sweep reference check, which now
--      joins folders to scope a blob's references to its owning mailbox.
--
-- gc_candidates is transient (rebuilt by every `gc mark` pass), so a transient
-- DEFAULT '' on the new column — added then immediately dropped — lets this run
-- whether or not the table currently holds rows, without leaving a column
-- default behind (keeping parity with schema.sql, which declares no default).

ALTER TABLE gc_candidates
    ADD COLUMN IF NOT EXISTS tenant TEXT NOT NULL DEFAULT '';
ALTER TABLE gc_candidates
    ALTER COLUMN tenant DROP DEFAULT;

ALTER TABLE gc_candidates
    DROP CONSTRAINT IF EXISTS gc_candidates_pkey;
ALTER TABLE gc_candidates
    ADD PRIMARY KEY (tenant, sha256, kind, bucket);

-- The mailbox name is also the on-disk blob tenant: a single, path-safe
-- directory component. Keep this regex in lockstep with blob.tenantRe /
-- blob.ParseTenant and the admin mailbox-add validator.
ALTER TABLE mailboxes
    ADD CONSTRAINT mailboxes_name_chk
    CHECK (name ~ '^[a-z0-9][a-z0-9._-]{0,63}$');

-- GC's reference check scopes a blob to its owning mailbox via
-- messages.folder_id -> folders.mailbox_id; carrying folder_id in the index
-- lets the EXISTS be answered from the index during the per-blob mark walk.
CREATE INDEX IF NOT EXISTS idx_messages_sha_date_folder
    ON messages (raw_sha256, raw_blob_date, folder_id);
