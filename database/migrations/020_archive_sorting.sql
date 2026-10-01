-- Archive sorting: Archive is the default action, and archiving files the
-- message for you (ARCHIVE_SORTING.md at the repository root).
--
-- archive_categories is the operator-approved archive taxonomy of one mailbox.
-- A category is a KEY the classifier may emit (up to three levels,
-- "finance/banking/acme-bank") plus the FOLDER that key files into. The
-- model only ever names a key from this list — epistula-api refuses any other —
-- and the key-to-folder mapping is operator data, so a message cannot steer
-- itself into a folder nobody approved. An ANNUAL category files into a year
-- folder below its own, <folder>/<YYYY>, the year taken from the message's
-- own date rather than from the model. Categories are retired, never deleted,
-- so a classification made against an older list stays explicable; a retired
-- key files nothing.
--
-- message_classifications is the classifier's decision for one message: which
-- category, and how confident it was. It is separate from message_annotations
-- on purpose. An annotation is advisory prose that any holder of
-- write_annotation may replace, including epistula-mcp's interactive annotate
-- tool; a classification MOVES mail, so it is written only under its own
-- permission, write_classification, and re-running a summary model can never
-- re-file a message. One row per message: the latest decision wins.
--
-- archive_moves is the journal of every server-side filing: the one-time
-- reorganization (reason 'reorg') and the live sorter (reason 'sort'). It is
-- what `epistula-database admin archive-undo` reverses and the answer to "why is
-- this message here". It holds folder names and the category, never message
-- content, and it disappears with the message it describes.

CREATE TABLE archive_categories (
    mailbox_id  BIGINT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
    key         TEXT NOT NULL,
    folder      TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    annual      BOOLEAN NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    retired_at  TIMESTAMPTZ,
    PRIMARY KEY (mailbox_id, key),
    CONSTRAINT archive_categories_key_chk
        CHECK (key ~ '^[a-z0-9][a-z0-9-]{0,39}(/[a-z0-9][a-z0-9-]{0,39}){0,2}$'),
    CONSTRAINT archive_categories_folder_chk
        CHECK (length(folder) BETWEEN 1 AND 255),
    CONSTRAINT archive_categories_description_chk
        CHECK (length(description) <= 500)
);

CREATE TABLE message_classifications (
    message_id BIGINT PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
    category   TEXT NOT NULL,
    confidence REAL NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
    model      TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE archive_moves (
    id          BIGSERIAL PRIMARY KEY,
    mailbox_id  BIGINT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
    message_id  BIGINT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    batch       TEXT NOT NULL,
    reason      TEXT NOT NULL CHECK (reason IN ('reorg', 'sort')),
    from_folder TEXT NOT NULL,
    to_folder   TEXT NOT NULL,
    category    TEXT,
    confidence  REAL,
    moved_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    undone_at   TIMESTAMPTZ
);
CREATE INDEX idx_archive_moves_batch ON archive_moves (mailbox_id, batch);
CREATE INDEX idx_archive_moves_message ON archive_moves (message_id);

-- The classifier writes through epistula-api under a permission of its own.
ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_permissions_check;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_permissions_check CHECK (
    cardinality(permissions) > 0 AND
    permissions <@ ARRAY['read_metadata', 'read_content', 'write_annotation', 'write_classification']
);

-- Grants for the roles that already exist, identified by what they can do
-- today, as migration 019 does. A role created later gets these from its
-- deploy grants (api/deploy/README.md,
-- imap/QUICKSTART-FREEBSD.md).
--
--   * A sidecar writer (INSERT on message_annotations: epistula-api, and
--     epistula-imap, which copies sidecars on COPY/MOVE) reads the taxonomy
--     and writes classifications.
--   * A message-state writer (DELETE on messages: epistula-imap) also runs the
--     live sorter and the Trash purge, so it journals moves.
DO $$
DECLARE r RECORD;
BEGIN
    FOR r IN
        SELECT rolname FROM pg_roles
         WHERE NOT rolsuper
           AND rolname !~ '^pg_'
           AND rolname <> current_user
           AND has_table_privilege(oid, 'public.message_annotations', 'INSERT')
    LOOP
        EXECUTE format('GRANT SELECT ON archive_categories TO %I', r.rolname);
        EXECUTE format('GRANT SELECT, INSERT, UPDATE ON message_classifications TO %I', r.rolname);
    END LOOP;
    FOR r IN
        SELECT rolname FROM pg_roles
         WHERE NOT rolsuper
           AND rolname !~ '^pg_'
           AND rolname <> current_user
           AND has_table_privilege(oid, 'public.messages', 'DELETE')
    LOOP
        EXECUTE format('GRANT SELECT ON archive_categories TO %I', r.rolname);
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON message_classifications TO %I', r.rolname);
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON archive_moves TO %I', r.rolname);
        EXECUTE format('GRANT USAGE, SELECT ON SEQUENCE archive_moves_id_seq TO %I', r.rolname);
    END LOOP;
END
$$;
