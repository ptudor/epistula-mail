-- Per-domain allowlist / denylist of localparts.
--
-- Resolution semantics (see recipients package):
--   1. Deny wins absolutely. A (domain_id, localpart, 'deny') match rejects
--      delivery even if an explicit alias exists.
--   2. Exact alias match (aliases) wins step 2.
--   3. Wildcard catchall (domains.is_wildcard or a localpart='' alias)
--      is gated by the allowlist when any 'allow' row exists for the
--      domain: the localpart must match an allow row, otherwise reject.
--   4. No allowlist rows → wildcard accepts everything not explicitly denied.
--
-- localpart is stored normalized (lowercase + NFC), matching what the
-- recipients resolver derives from envelope addresses. The empty string is
-- not permitted here (use aliases.localpart='' for catchall routing).

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

-- Counts the 'allow' rows for a domain quickly; used by the resolver to
-- decide whether the allowlist is active (any rows present → gate the
-- wildcard) without an extra round-trip.
CREATE INDEX IF NOT EXISTS idx_domain_acl_allow_count
    ON domain_acl (domain_id) WHERE kind = 'allow';
