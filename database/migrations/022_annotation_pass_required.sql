-- The annotation pipeline's work queue.
--
-- "Not annotated yet" used to be an absence: the worker found its work by
-- walking every message and probing message_annotations (and
-- message_classifications) for each. An absence cannot be indexed, so the
-- question cost a scan of the whole store however little there was to do,
-- and grew with the store even when no work remained.
--
-- annotation_pass_required makes it a presence. Every message is marked in
-- the same transaction that stores it (deliver, import, import-blobs, IMAP
-- APPEND through storage.Ingest; IMAP COPY carries the source's marker), so
-- no message is stored unmarked and no marker outlives a rolled-back
-- delivery. epistula-api's `pass_required=true` reads the marked messages through
-- this table's primary key, at a cost set by the queue rather than the store,
-- and `POST /v1/pass-required/prune` deletes the markers of messages the
-- pipeline has finished. A message stays marked until then, so a failed
-- message is retried.
--
-- The queue is the fast path, not the guarantee. Work that no insert marks --
-- a new annotation model, a retired archive category, anything stored before
-- this migration -- is found by the worker's complete round, the full sweep it
-- runs at startup and every few hours. Nothing is backfilled here for that
-- reason.
CREATE TABLE annotation_pass_required (
    message_id BIGINT PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
    marked_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Grants for the roles that already exist, identified by what they can do,
-- as migrations 019 and 020 do. A role created later gets these from its
-- deploy grants (api/deploy/README.md,
-- imap/deploy/README.md).
DO $$
DECLARE r RECORD;
BEGIN
    -- Writers of messages (epistula-imap: APPEND marks, COPY carries).
    FOR r IN
        SELECT rolname FROM pg_roles
         WHERE NOT rolsuper
           AND rolname !~ '^pg_'
           AND rolname <> current_user
           AND has_table_privilege(oid, 'public.messages', 'INSERT')
    LOOP
        EXECUTE format('GRANT SELECT, INSERT ON annotation_pass_required TO %I', r.rolname);
    END LOOP;
    -- The annotation sidecar's writer that stores no messages (epistula-api):
    -- reads the queue and prunes finished work.
    FOR r IN
        SELECT rolname FROM pg_roles
         WHERE NOT rolsuper
           AND rolname !~ '^pg_'
           AND rolname <> current_user
           AND has_table_privilege(oid, 'public.message_annotations', 'INSERT')
           AND NOT has_table_privilege(oid, 'public.messages', 'INSERT')
    LOOP
        EXECUTE format('GRANT SELECT, DELETE ON annotation_pass_required TO %I', r.rolname);
    END LOOP;
END
$$;
