-- Bearer API tokens and the message-annotation sidecar for epistula-api.
--
-- api_tokens: service credentials for api (the HTTP/JSON
-- reader). Tokens are random secrets presented once at mint time and stored
-- only as Argon2id PHC hashes; the printed token embeds the row id
-- ("mapi_<id>_<secret>") so verification is a single-row lookup + one
-- Argon2id computation, never a scan over every hash. Lifecycle is owned by
-- `epistula-database admin api-token-{add,list,revoke}`; epistula-api only verifies.
--
-- message_annotations: derived sidecar written by epistula-api's single write
-- path (PUT /v1/messages/{id}/annotation). Keyed (message_id, model) so each
-- model holds one annotation per message and re-running a model replaces
-- rather than duplicates. Pure derived data: raw blobs and IMAP state are
-- never touched.

CREATE TABLE api_tokens (
    id              BIGSERIAL PRIMARY KEY,
    name            TEXT NOT NULL,
    token_hash      TEXT NOT NULL,
    scope_mailboxes TEXT[] NOT NULL CHECK (cardinality(scope_mailboxes) > 0),
    permissions     TEXT[] NOT NULL CHECK (
        cardinality(permissions) > 0 AND
        permissions <@ ARRAY['read_metadata', 'read_content', 'write_annotation']
    ),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at    TIMESTAMPTZ,
    revoked_at      TIMESTAMPTZ
);

-- A live (unrevoked) token name must be unique, but revoking frees the name
-- for re-mint while the revoked row stays for audit.
CREATE UNIQUE INDEX idx_api_tokens_live_name ON api_tokens (name) WHERE revoked_at IS NULL;

CREATE TABLE message_annotations (
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

CREATE INDEX idx_message_annotations_tags ON message_annotations USING GIN (tags);
CREATE INDEX idx_message_annotations_category
    ON message_annotations (category) WHERE category IS NOT NULL;
