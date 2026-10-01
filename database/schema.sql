-- database PostgreSQL schema
-- Requires PostgreSQL 14+ (generated columns, GIN tsvector, JSONB indexing).

BEGIN;

CREATE TABLE IF NOT EXISTS schema_versions (
    version     INTEGER PRIMARY KEY,
    applied_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    description TEXT
);

-- IMAP accounts. One mailbox can be the destination for many domain/localpart
-- combinations via aliases. password_hash is the Argon2id PHC-encoded value used
-- by imap to verify AUTH=PLAIN.
-- name is also the on-disk blob tenant (a single, path-safe directory
-- component), so it is constrained to the canonical charset — kept in lockstep
-- with blob.ParseTenant and the admin mailbox-add validator (migration 007).
CREATE TABLE IF NOT EXISTS mailboxes (
    id            BIGSERIAL PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,
    quota_bytes   BIGINT,
    used_bytes    BIGINT NOT NULL DEFAULT 0,
    password_hash TEXT NOT NULL,
    disabled_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Declared last because migration 013 appends it, and the bootstrap must
    -- reproduce a migrated database's column order exactly.
    --
    -- Non-NULL means this mailbox is quiesced for an operation that moves its
    -- blob tenant tree (RA6X-013): storage.Ingest refuses writes and gc skips
    -- the tenant, so the window where the database and the filesystem disagree
    -- about where the blobs live can be crossed with nobody writing into it.
    -- Distinct from disabled_at, which rejects authentication but still
    -- accepts delivery.
    maintenance_at TIMESTAMPTZ,
    CONSTRAINT mailboxes_name_chk CHECK (name ~ '^[a-z0-9][a-z0-9._-]{0,63}$')
);

-- Inbound domains. is_wildcard=TRUE means accept any localpart without an
-- explicit alias row (resolution then falls through to the localpart='' catchall).
CREATE TABLE IF NOT EXISTS domains (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    is_wildcard BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Address → mailbox routing. localpart='' is the catchall for a domain.
CREATE TABLE IF NOT EXISTS aliases (
    id         BIGSERIAL PRIMARY KEY,
    domain_id  BIGINT NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
    localpart  TEXT NOT NULL,
    mailbox_id BIGINT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (domain_id, localpart)
);
CREATE INDEX IF NOT EXISTS idx_aliases_mailbox ON aliases (mailbox_id);

-- Per-domain allowlist / denylist. See recipients package for resolution
-- semantics: deny wins absolutely; allow rows (when any exist for a domain)
-- gate the wildcard catchall.
CREATE TABLE IF NOT EXISTS domain_acl (
    id         BIGSERIAL PRIMARY KEY,
    domain_id  BIGINT NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
    localpart  TEXT   NOT NULL CHECK (length(localpart) > 0),
    kind       TEXT   NOT NULL CHECK (kind IN ('allow', 'deny')),
    note       TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (domain_id, localpart, kind)
);
CREATE INDEX IF NOT EXISTS idx_domain_acl_lookup
    ON domain_acl (domain_id, localpart, kind);
CREATE INDEX IF NOT EXISTS idx_domain_acl_allow_count
    ON domain_acl (domain_id) WHERE kind = 'allow';

-- IMAP folders per mailbox. special_use carries the RFC 6154 attribute when set
-- (one of '\Sent', '\Drafts', '\Trash', '\Junk', '\Archive', '\Important',
-- '\Flagged', or '\All').
--
-- NOTE: column order in this file mirrors the cumulative migrations (the 001
-- definition with later ADD COLUMNs at the end), so a schema.sql bootstrap
-- and a migrated database agree on pg_dump output and SELECT * ordinals.
-- Keep additive columns LAST; the parity test enforces this.
CREATE TABLE IF NOT EXISTS folders (
    id              BIGSERIAL PRIMARY KEY,
    mailbox_id      BIGINT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    uidvalidity     BIGINT NOT NULL,
    uidnext         BIGINT NOT NULL DEFAULT 1,
    special_use     TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    highest_modseq  BIGINT NOT NULL DEFAULT 1,  -- migration 004
    UNIQUE (mailbox_id, name)
);
CREATE INDEX IF NOT EXISTS idx_folders_mailbox ON folders (mailbox_id);

-- IMAP SUBSCRIBE state per mailbox.
CREATE TABLE IF NOT EXISTS folder_subscriptions (
    mailbox_id BIGINT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
    folder_id  BIGINT NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
    PRIMARY KEY (mailbox_id, folder_id)
);

-- Messages. One row per recipient mailbox; the raw blob on disk
-- (<mailbox>/raw/<yyyy/mm/dd>/<aa>/<bb>/<hex>.eml) is deduplicated by sha256
-- within the mailbox and date bucket.
CREATE TABLE IF NOT EXISTS messages (
    id            BIGSERIAL PRIMARY KEY,
    folder_id     BIGINT NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
    uid           BIGINT NOT NULL,
    raw_sha256    BYTEA NOT NULL,
    raw_size      BIGINT NOT NULL,
    internal_date TIMESTAMPTZ NOT NULL,
    message_id    TEXT,
    in_reply_to   TEXT,
    subject       TEXT,
    from_addr     TEXT,
    to_addrs      TEXT[] NOT NULL DEFAULT '{}',
    cc_addrs      TEXT[] NOT NULL DEFAULT '{}',
    sent_date     TIMESTAMPTZ,
    headers       JSONB NOT NULL DEFAULT '{}'::jsonb,
    text_body     TEXT,
    html_body     TEXT,
    bodystructure JSONB NOT NULL DEFAULT '{}'::jsonb,
    flags         TEXT[] NOT NULL DEFAULT '{}',
    -- Historical: migration 014 drops this and re-adds it with a bounded
    -- expression. Declared and dropped below so a bootstrapped database has
    -- the same attnum layout as a migrated one — PostgreSQL does not renumber
    -- attnums after DROP COLUMN. Delete this line and the DROP below together,
    -- never separately.
    fts           TSVECTOR,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    raw_blob_date DATE NOT NULL DEFAULT CURRENT_DATE,  -- migration 003
    mod_seq       BIGINT NOT NULL DEFAULT 1,           -- migration 004
    UNIQUE (folder_id, uid)
);
ALTER TABLE messages DROP COLUMN IF EXISTS fts;
-- The vector is built from a BOUNDED prefix (migration 014, RA6X-030).
-- PostgreSQL caps a tsvector at 1 MiB while the parser accepts 50 MiB
-- messages, so indexing the whole projection made a legitimate ~1.6 MB text
-- message impossible to STORE. text_body keeps the full projection and the raw
-- blob is untouched; only the index is bounded. Full-text search therefore
-- covers the first 200,000 characters of subject + text_body — see migration
-- 014 for why that number.
ALTER TABLE messages ADD COLUMN fts TSVECTOR GENERATED ALWAYS AS (
    setweight(to_tsvector('simple', left(coalesce(subject, ''), 200000)), 'A') ||
    setweight(to_tsvector('simple', left(coalesce(text_body, ''), 200000)), 'B')
) STORED;

-- The calendar date the SENDER wrote, with the Date header's own offset
-- (migration 017, RA6X-048). Added here — after the fts re-add — because
-- migration 017 runs after 014 and the bootstrap must reproduce a migrated
-- database's column order.
--
-- sent_date is a timestamptz whose original offset is gone, and reducing it
-- with ::date uses the connection's TimeZone — so SENTSINCE/SENTBEFORE both
-- disagreed with the header and varied by connection. NULL means no Date
-- header, an unparseable one, or a row predating this column; search falls back
-- to the UTC reduction for those.
ALTER TABLE messages ADD COLUMN IF NOT EXISTS sent_date_local DATE;
CREATE INDEX IF NOT EXISTS idx_messages_sent_date_local
    ON messages (sent_date_local) WHERE sent_date_local IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_messages_fts          ON messages USING GIN (fts);
CREATE INDEX IF NOT EXISTS idx_messages_headers      ON messages USING GIN (headers);
CREATE INDEX IF NOT EXISTS idx_messages_folder_date  ON messages (folder_id, internal_date DESC);
CREATE INDEX IF NOT EXISTS idx_messages_message_id   ON messages (message_id) WHERE message_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_messages_raw_sha256   ON messages (raw_sha256);
CREATE INDEX IF NOT EXISTS idx_messages_folder_sha   ON messages (folder_id, raw_sha256);
CREATE INDEX IF NOT EXISTS idx_messages_sha_date     ON messages (raw_sha256, raw_blob_date);
CREATE INDEX IF NOT EXISTS idx_messages_sha_date_folder ON messages (raw_sha256, raw_blob_date, folder_id);
CREATE INDEX IF NOT EXISTS idx_messages_folder_modseq ON messages (folder_id, mod_seq);

-- Attachments extracted from multipart messages. filename is metadata only;
-- the on-disk file is named by sha256
-- (<mailbox>/att/<yyyy/mm/dd>/<aa>/<bb>/<hex>.bin).
CREATE TABLE IF NOT EXISTS attachments (
    id           BIGSERIAL PRIMARY KEY,
    message_id   BIGINT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    part_number  TEXT NOT NULL,
    filename     TEXT,
    content_type TEXT NOT NULL,
    content_id   TEXT,
    disposition  TEXT,
    size_bytes   BIGINT NOT NULL,
    sha256       BYTEA NOT NULL,
    blob_date    DATE NOT NULL DEFAULT CURRENT_DATE
);
CREATE INDEX IF NOT EXISTS idx_attachments_message  ON attachments (message_id);
CREATE INDEX IF NOT EXISTS idx_attachments_sha256   ON attachments (sha256);
CREATE INDEX IF NOT EXISTS idx_attachments_sha_date ON attachments (sha256, blob_date);

-- Two-phase blob GC candidates. kind is 'raw' or 'att'. generation_marked
-- records the epoch of the LAST mark pass that found the blob unreferenced;
-- sweep only deletes candidates re-confirmed by a mark later than their
-- first sighting (generation_marked > epoch(first_seen_at)). tenant is the
-- owning-mailbox subtree (migration 007): blobs are physically isolated per
-- mailbox, so identical content+date in two mailboxes are two distinct
-- candidates and the sweeper needs the tenant to reconstruct the path.
CREATE TABLE IF NOT EXISTS gc_candidates (
    sha256            BYTEA NOT NULL,
    kind              TEXT NOT NULL CHECK (kind IN ('raw', 'att')),
    first_seen_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    generation_marked BIGINT NOT NULL,
    bucket            TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY/MM/DD'),  -- migration 003
    tenant            TEXT NOT NULL,  -- migration 007
    PRIMARY KEY (tenant, sha256, kind, bucket)
);
-- migration 010: back the batched gc sweep's range + keyset pagination.
CREATE INDEX IF NOT EXISTS idx_gc_candidates_sweep
    ON gc_candidates (first_seen_at, tenant, sha256, kind, bucket);

-- Per-delivery audit log. raw_sha256_short carries the first 16 hex chars for
-- log correlation when message_id is NULL (e.g., for rejected deliveries).
CREATE TABLE IF NOT EXISTS delivery_log (
    id                 BIGSERIAL PRIMARY KEY,
    received_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    envelope_from      TEXT,
    envelope_to        TEXT NOT NULL,
    matched_alias_id   BIGINT REFERENCES aliases(id) ON DELETE SET NULL,
    matched_mailbox_id BIGINT REFERENCES mailboxes(id) ON DELETE SET NULL,
    message_id         BIGINT REFERENCES messages(id) ON DELETE SET NULL,
    bytes              BIGINT NOT NULL,
    raw_sha256_short   TEXT,
    outcome            TEXT NOT NULL,
    error_detail       TEXT
);
CREATE INDEX IF NOT EXISTS idx_delivery_log_received ON delivery_log (received_at DESC);

-- Bearer API tokens for api (migration 006). The printed
-- token embeds the row id ("mapi_<id>_<secret>") so verification is one row
-- lookup + one Argon2id computation; only the Argon2id PHC hash is stored.
-- Lifecycle is owned by `epistula-database admin api-token-{add,list,revoke}`.
--
-- Scope is expressed in mailbox IDs, not names (migration 012, RA6X-012).
-- mailboxes.id is a BIGSERIAL that is never reused, so a scope entry for a
-- deleted mailbox resolves to nothing forever and a recreated mailbox with the
-- same name is a different account this token has never been granted. Renaming
-- a mailbox therefore does not touch tokens at all — they follow the account.
-- The wildcard has its own boolean rather than a magic name.
CREATE TABLE IF NOT EXISTS api_tokens (
    id                  BIGSERIAL PRIMARY KEY,
    name                TEXT NOT NULL,
    token_hash          TEXT NOT NULL,
    -- Historical: migration 012 dropped this. It is declared and immediately
    -- dropped below so a bootstrapped database has the same attnum layout as a
    -- migrated one — PostgreSQL does not renumber attnums after DROP COLUMN,
    -- so without this the two would disagree on every subsequent column's
    -- ordinal position, which is exactly what pg_dump and `SELECT *` follow.
    -- Delete this line and the DROP below together, never separately.
    scope_mailboxes     TEXT[],
    permissions         TEXT[] NOT NULL CHECK (
        cardinality(permissions) > 0 AND
        permissions <@ ARRAY['read_metadata', 'read_content', 'write_annotation', 'write_classification']
    ),  -- write_classification: migration 020
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at        TIMESTAMPTZ,
    revoked_at          TIMESTAMPTZ,
    -- Declared after the historical columns because migration 012 appends
    -- them.
    scope_all_mailboxes BOOLEAN NOT NULL DEFAULT false,
    scope_mailbox_ids   BIGINT[] NOT NULL DEFAULT '{}',
    -- Revoked rows are exempt: a token can always be revoked, including one
    -- whose last scope has just been removed with its mailbox.
    CONSTRAINT api_tokens_scope_nonempty CHECK (
        revoked_at IS NOT NULL
        OR scope_all_mailboxes
        OR cardinality(scope_mailbox_ids) > 0
    )
);
ALTER TABLE api_tokens DROP COLUMN IF EXISTS scope_mailboxes;
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_tokens_live_name ON api_tokens (name) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_api_tokens_scope_ids ON api_tokens USING GIN (scope_mailbox_ids);

-- Derived annotation sidecar written by epistula-api (migration 006). Keyed
-- (message_id, model): re-running a model replaces its annotation. Pure
-- derived data — raw blobs and IMAP state are never touched.
CREATE TABLE IF NOT EXISTS message_annotations (
    message_id BIGINT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    model      TEXT NOT NULL,
    tags       TEXT[] NOT NULL DEFAULT '{}',
    category   TEXT,
    summary    TEXT,
    tokens_in  BIGINT,
    tokens_out BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (message_id, model)
);
CREATE INDEX IF NOT EXISTS idx_message_annotations_tags ON message_annotations USING GIN (tags);
CREATE INDEX IF NOT EXISTS idx_message_annotations_category
    ON message_annotations (category) WHERE category IS NOT NULL;

-- Operator registry ranking annotation models (migration 008). Not an FK on
-- message_annotations.model: a model may be written before it is registered, or
-- registered before it annotates. epistula-api LEFT JOINs this and orders a
-- message's annotations by (not retired, priority DESC, created_at DESC); a
-- retired model is kept as an alternative but never primary. Lifecycle:
-- `epistula-database admin annotation-model-{set,list,retire}`.
CREATE TABLE IF NOT EXISTS annotation_models (
    model        TEXT PRIMARY KEY,
    priority     INTEGER NOT NULL DEFAULT 0,
    display_name TEXT,
    retired_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- migration 009 (additive-last): collision-proof UIDVALIDITY source (R-062) +
-- global (internal_date DESC, id DESC) index for search/export (R-018).
CREATE SEQUENCE IF NOT EXISTS folder_uidvalidity_seq AS BIGINT;
SELECT setval('folder_uidvalidity_seq',
    GREATEST(
        (SELECT COALESCE(MAX(uidvalidity), 0) FROM folders),
        EXTRACT(epoch FROM now())::bigint
    ));
CREATE INDEX IF NOT EXISTS idx_messages_date_id ON messages (internal_date DESC, id DESC);

-- The flag canonicalization rule as SQL (migration 016, RA6X-028). It must
-- agree with imapflags.Canonical; the parity is asserted by a test.
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

INSERT INTO schema_versions (version, description) VALUES
    (1, 'initial schema'),
    (2, 'domain acl'),
    (3, 'blob date partition'),
    (4, 'modseq'),
    (5, 'drop legacy admin tables'),
    (6, 'api tokens annotations'),
    (7, 'blob tenant isolation'),
    (8, 'annotation models'),
    (9, 'uidvalidity seq and date index'),
    (10, 'gc candidates sweep index'),
    -- Migration 011 is a DATA migration (it canonicalizes the case of system
    -- flags already in messages.flags, RO5X-013). A fresh schema.sql bootstrap
    -- has no rows to fix, so there is no DDL here to mirror — only the version
    -- record, so a bootstrapped database and a migrated one agree on what has
    -- been applied.
    (11, 'canonicalize flags'),
    (12, 'api token scope ids'),
    (13, 'mailbox maintenance'),
    (14, 'bounded fts'),
    -- Migration 015 is a DATA migration: it deletes gc_candidates rows that
    -- migration 007 left with an empty tenant (RA6X-051). A fresh bootstrap
    -- has no such rows, so there is no DDL to mirror — only the version
    -- record, so a bootstrapped and a migrated database agree on what has
    -- been applied.
    (15, 'purge legacy gc candidates'),
    -- Migration 016 is a DATA migration: it merges keyword-flag case aliases
    -- in messages.flags (RA6X-028). A fresh bootstrap has no rows to fix, so
    -- only the version record is mirrored here — but the helper function it
    -- defines is part of the schema, so it is created below too.
    (16, 'canonicalize keyword flags'),
    (17, 'sent date local'),
    (18, 'bounded uidvalidity'),
    (19, 'annotation parent lock'),
    (20, 'archive sorting'),
    (21, 'folder redirects'),
    (22, 'annotation pass required')
    ON CONFLICT DO NOTHING;

-- All folder creation/rename paths use this allocator. Never wrap or reset the
-- shared sequence: exhaustion requires operator recovery with new identities.
CREATE OR REPLACE FUNCTION mail_next_uidvalidity() RETURNS BIGINT AS $$
DECLARE value BIGINT;
BEGIN
    value := nextval('folder_uidvalidity_seq');
    IF value < 1 OR value > 4294967295 THEN
        RAISE EXCEPTION 'IMAP UIDVALIDITY exceeds the 32-bit protocol range'
            USING ERRCODE = '22003';
    END IF;
    RETURN value;
END;
$$ LANGUAGE plpgsql;

-- The lock epistula-api takes on a message before writing its annotation, without
-- UPDATE privilege on messages (migration 019, OPS-008).
CREATE OR REPLACE FUNCTION mail_lock_message_for_annotation(p_message_id BIGINT)
RETURNS SETOF BIGINT
LANGUAGE sql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $$
    SELECT id FROM public.messages WHERE id = p_message_id FOR KEY SHARE
$$;

-- A new function is executable by PUBLIC. Every role that can connect would
-- then be able to hold message rows locked, blocking EXPUNGE and MOVE for as
-- long as it keeps a transaction open. Revoke that, and grant the function to
-- exactly the roles that may write annotations, the only writers that need it.
-- A role created after this migration gets it from its deploy grants
-- (api/deploy/README.md).
REVOKE ALL ON FUNCTION mail_lock_message_for_annotation(BIGINT) FROM PUBLIC;

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
        EXECUTE format('GRANT EXECUTE ON FUNCTION mail_lock_message_for_annotation(BIGINT) TO %I', r.rolname);
    END LOOP;
END
$$;

-- Archive sorting (migration 020; ARCHIVE_SORTING.md at the repository root).
-- archive_categories is the operator-approved taxonomy of one mailbox: a key
-- the classifier may emit (up to three levels) and the folder it files into,
-- below which an annual category adds a <YYYY> folder. message_classifications
-- is the classifier's latest decision per message, written only under the
-- write_classification permission. archive_moves journals every server-side
-- filing so it can be undone.
--
-- messages.created_at is the time a row entered its CURRENT folder: delivery,
-- import, APPEND, COPY and MOVE all insert a new row, and the server-side
-- in-place move (storage.MoveMessages) resets it. The live sorter's settle
-- delay and the Trash retention period are both measured from it.
CREATE TABLE IF NOT EXISTS archive_categories (
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

CREATE TABLE IF NOT EXISTS message_classifications (
    message_id BIGINT PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
    category   TEXT NOT NULL,
    confidence REAL NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
    model      TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS archive_moves (
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
CREATE INDEX IF NOT EXISTS idx_archive_moves_batch ON archive_moves (mailbox_id, batch);
CREATE INDEX IF NOT EXISTS idx_archive_moves_message ON archive_moves (message_id);

-- Folder redirects (migration 021): where a merged folder's mail goes now.
-- `admin folder-merge` records its source, subtree flag, destination and
-- batch; `import` follows the record only when its target folder does not
-- exist, so a Maildir sync after the merge lands in the merged folder instead
-- of re-creating the source and storing its messages twice. `archive-undo`
-- of the batch deletes the row. Owner-only: merge, undo and import all run as
-- epistula-database. Migration 021 also recovers earlier merges from
-- archive_moves; a fresh bootstrap has none.
CREATE TABLE IF NOT EXISTS folder_redirects (
    mailbox_id  BIGINT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
    from_folder TEXT NOT NULL,
    subtree     BOOLEAN NOT NULL,
    to_folder   TEXT NOT NULL,
    batch       TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (mailbox_id, from_folder, batch),
    CONSTRAINT folder_redirects_distinct_chk CHECK (from_folder <> to_folder)
);

-- The annotation pipeline's work queue (migration 022). storage.Ingest marks
-- every message in the transaction that stores it and IMAP COPY carries the
-- source's marker, so "not processed yet" is a row that can be read through
-- a primary key instead of an absence found by scanning the store. epistula-api
-- serves the marked messages (pass_required=true) and prunes the markers of
-- finished ones; the worker's complete round finds whatever no insert marks.
CREATE TABLE IF NOT EXISTS annotation_pass_required (
    message_id BIGINT PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
    marked_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Grants for roles that already exist, as migration 020 makes them.
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

-- Grants for roles that already exist, as migration 022 makes them.
DO $$
DECLARE r RECORD;
BEGIN
    FOR r IN
        SELECT rolname FROM pg_roles
         WHERE NOT rolsuper
           AND rolname !~ '^pg_'
           AND rolname <> current_user
           AND has_table_privilege(oid, 'public.messages', 'INSERT')
    LOOP
        EXECUTE format('GRANT SELECT, INSERT ON annotation_pass_required TO %I', r.rolname);
    END LOOP;
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

COMMIT;
