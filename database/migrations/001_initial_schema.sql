-- Initial schema. Kept in sync with schema.sql at the project root; either
-- file can bootstrap a fresh database. Once a deployment has used the
-- migration runner once, schema.sql is for reference only — all subsequent
-- changes ship as 002+ migrations.

CREATE TABLE IF NOT EXISTS schema_versions (
    version     INTEGER PRIMARY KEY,
    applied_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    description TEXT
);

CREATE TABLE IF NOT EXISTS mailboxes (
    id            BIGSERIAL PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,
    quota_bytes   BIGINT,
    used_bytes    BIGINT NOT NULL DEFAULT 0,
    password_hash TEXT NOT NULL,
    disabled_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS domains (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    is_wildcard BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS aliases (
    id         BIGSERIAL PRIMARY KEY,
    domain_id  BIGINT NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
    localpart  TEXT NOT NULL,
    mailbox_id BIGINT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (domain_id, localpart)
);
CREATE INDEX IF NOT EXISTS idx_aliases_mailbox ON aliases (mailbox_id);

CREATE TABLE IF NOT EXISTS folders (
    id          BIGSERIAL PRIMARY KEY,
    mailbox_id  BIGINT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    uidvalidity BIGINT NOT NULL,
    uidnext     BIGINT NOT NULL DEFAULT 1,
    special_use TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (mailbox_id, name)
);
CREATE INDEX IF NOT EXISTS idx_folders_mailbox ON folders (mailbox_id);

CREATE TABLE IF NOT EXISTS folder_subscriptions (
    mailbox_id BIGINT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
    folder_id  BIGINT NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
    PRIMARY KEY (mailbox_id, folder_id)
);

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
    fts           TSVECTOR GENERATED ALWAYS AS (
        setweight(to_tsvector('simple', coalesce(subject, '')), 'A') ||
        setweight(to_tsvector('simple', coalesce(text_body, '')), 'B')
    ) STORED,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (folder_id, uid)
);
CREATE INDEX IF NOT EXISTS idx_messages_fts          ON messages USING GIN (fts);
CREATE INDEX IF NOT EXISTS idx_messages_headers      ON messages USING GIN (headers);
CREATE INDEX IF NOT EXISTS idx_messages_folder_date  ON messages (folder_id, internal_date DESC);
CREATE INDEX IF NOT EXISTS idx_messages_message_id   ON messages (message_id) WHERE message_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_messages_raw_sha256   ON messages (raw_sha256);
CREATE INDEX IF NOT EXISTS idx_messages_folder_sha   ON messages (folder_id, raw_sha256);

CREATE TABLE IF NOT EXISTS attachments (
    id           BIGSERIAL PRIMARY KEY,
    message_id   BIGINT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    part_number  TEXT NOT NULL,
    filename     TEXT,
    content_type TEXT NOT NULL,
    content_id   TEXT,
    disposition  TEXT,
    size_bytes   BIGINT NOT NULL,
    sha256       BYTEA NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_attachments_message ON attachments (message_id);
CREATE INDEX IF NOT EXISTS idx_attachments_sha256  ON attachments (sha256);

CREATE TABLE IF NOT EXISTS gc_candidates (
    sha256            BYTEA NOT NULL,
    kind              TEXT NOT NULL CHECK (kind IN ('raw', 'att')),
    first_seen_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    generation_marked BIGINT NOT NULL,
    PRIMARY KEY (sha256, kind)
);

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

-- Legacy admin tables: kept in 001 for parity with deployments that
-- bootstrapped from an earlier schema.sql that still declared them.
-- Migration 005 drops them; do not use these tables for new code.
CREATE TABLE IF NOT EXISTS admin_users (
    id            BIGSERIAL PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS admin_sessions (
    id            TEXT PRIMARY KEY,
    admin_user_id BIGINT NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
    csrf_token    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_admin_sessions_expires ON admin_sessions (expires_at);
