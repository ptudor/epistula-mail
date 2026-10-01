-- RA6X-013: a maintenance barrier for the one operation that cannot be made
-- atomic — renaming a mailbox, whose name IS its on-disk blob tenant path.
--
-- The rename is inherently two-phase: the operator moves
-- <storage_root>/<old> to <storage_root>/<new> (a `zfs rename` on a
-- ZFS host), then `admin mailbox-rename` updates mailboxes.name. Between
-- those, the database and the filesystem disagree about where this account's
-- blobs live, and every other participant is free to act on the disagreement:
--
--   * an authenticated IMAP session caches the mailbox name (and therefore the
--     tenant) at login, so it can APPEND a blob under the OLD tree while
--     inserting the row into the renamed mailbox — the message is acknowledged
--     and permanently unreadable;
--   * `gc mark` treats a tenant directory whose name resolves to no mailbox as
--     entirely unreferenced, so the freshly moved tree is a reap candidate;
--   * delivery and imports write under whichever name they resolved.
--
-- maintenance_at is the barrier. While it is set, storage.Ingest refuses to
-- write to the mailbox and GC skips its tenant, so the gap can be crossed with
-- nobody writing into it. It is deliberately NOT disabled_at: a disabled
-- mailbox still accepts delivery (that is its documented meaning), and this
-- has to stop delivery too.
--
-- Set and cleared by `admin mailbox-maintenance -name N -on|-off`;
-- `admin mailbox-rename` requires it and clears it on success.

ALTER TABLE mailboxes ADD COLUMN IF NOT EXISTS maintenance_at TIMESTAMPTZ;

COMMENT ON COLUMN mailboxes.maintenance_at IS
    'Non-NULL means this mailbox is quiesced for an operation that moves its blob tenant tree: storage.Ingest refuses writes and gc skips the tenant. Distinct from disabled_at, which rejects authentication but still accepts delivery.';
