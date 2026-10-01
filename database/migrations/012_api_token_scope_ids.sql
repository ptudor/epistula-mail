-- RA6X-012: bind API token scope to durable mailbox identity instead of names.
--
-- api_tokens.scope_mailboxes was a TEXT[] of mailbox NAMES with no foreign key
-- to mailboxes. Names are not identities: `admin mailbox-delete` cascaded a
-- mailbox's folders, messages and aliases but left every token still scoped to
-- the name, so recreating that name handed the old credentials read and write
-- access to a completely different account's mail — indefinitely, and without
-- relying on a stale cache. Renaming away from a name and reusing it quickly
-- opened the same hole through epistula-api's verification cache.
--
-- A mailbox id is durable: mailboxes.id is a BIGSERIAL that is never reused, so
-- a scope entry pointing at a deleted mailbox resolves to nothing forever, and
-- a recreated mailbox with the same name is a different id that no old token
-- names. Renaming becomes a non-event for tokens — they follow the account,
-- which is what "scoped to this mailbox" always meant — so mailbox-rename no
-- longer rewrites token scopes at all.
--
-- The wildcard scope moves to its own boolean rather than the magic name '*',
-- which was indistinguishable from a mailbox legitimately named '*'.
--
-- ============================================================================
-- OPERATOR NOTE — THIS MIGRATION CAN REVOKE TOKENS.
--
-- A live token whose scope named ONLY mailboxes that no longer exist has no
-- access left to express. The scope CHECK cannot represent an empty scope, and
-- silently widening it is exactly the defect being fixed, so such a token is
-- revoked. Check for them before running:
--
--   SELECT id, name, scope_mailboxes FROM api_tokens
--    WHERE revoked_at IS NULL
--      AND scope_mailboxes <> ARRAY['*']
--      AND NOT EXISTS (SELECT 1 FROM mailboxes m WHERE m.name = ANY(scope_mailboxes));
--
-- Any row listed there is a credential that currently believes it is scoped to
-- a deleted account. Re-issue it against the intended mailbox with
-- `admin api-token-add` after migrating.
--
-- Scope entries naming a deleted mailbox are dropped from tokens that retain at
-- least one live mailbox; their other scopes are untouched.
-- ============================================================================

ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS scope_all_mailboxes BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS scope_mailbox_ids BIGINT[] NOT NULL DEFAULT '{}';

-- Wildcard tokens keep wildcard semantics.
UPDATE api_tokens SET scope_all_mailboxes = true
 WHERE scope_mailboxes = ARRAY['*'];

-- Everything else resolves its names to ids. A name with no matching mailbox
-- contributes nothing: it must NEVER be reinterpreted as access to whatever
-- account later takes that name.
UPDATE api_tokens t
   SET scope_mailbox_ids = COALESCE((
         SELECT array_agg(m.id ORDER BY m.id)
           FROM mailboxes m
          WHERE m.name = ANY(t.scope_mailboxes)
       ), '{}')
 WHERE NOT t.scope_all_mailboxes;

-- A live token left with nothing in scope is revoked rather than granted a
-- scope the CHECK would reject or, worse, an empty scope some future reader
-- might treat as "unrestricted".
UPDATE api_tokens
   SET revoked_at = now()
 WHERE revoked_at IS NULL
   AND NOT scope_all_mailboxes
   AND cardinality(scope_mailbox_ids) = 0;

ALTER TABLE api_tokens DROP COLUMN scope_mailboxes;

-- Revoked rows are exempt: a token can always be revoked, including one whose
-- last scope has just been removed, and historical rows must stay readable.
ALTER TABLE api_tokens DROP CONSTRAINT IF EXISTS api_tokens_scope_nonempty;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_scope_nonempty CHECK (
    revoked_at IS NOT NULL
    OR scope_all_mailboxes
    OR cardinality(scope_mailbox_ids) > 0
);

-- Scope lookups are per-request on the API's hot path.
CREATE INDEX IF NOT EXISTS idx_api_tokens_scope_ids ON api_tokens USING GIN (scope_mailbox_ids);
