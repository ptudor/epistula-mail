# Epistula — Deployment artifacts for epistula-api

One long-running surface: the JSON API on loopback behind Apache, plus a loopback observability listener.

| Mode    | Lifecycle    | Surface                                                       |
|---------|--------------|---------------------------------------------------------------|
| `serve` | long-running | `127.0.0.1:8784` JSON API + `127.0.0.1:8785` metrics/health   |

No `deliver`, no `import`, no `gc`, no `admin`, no `migrate` — those live in [epistula-database](../../database/). Token mint/list/revoke is `epistula-database admin api-token-*`; this daemon only verifies.

## Files

- `freebsd/epistula_api` — rc.d script for the `serve` daemon. Copy to `/usr/local/etc/rc.d/epistula_api`, `chmod 555`, then `sysrc epistula_api_enable=YES` and `service epistula_api start`. The script runs `check-config` as `start_precmd`.
- `apache/epistula-api-include.conf` — reverse-proxy Include: TLS termination for `/v1`, restricted `/metrics`, public `/health`. Add mTLS in the surrounding vhost for external consumers.

## Postgres role (recommended)

A dedicated least-privilege role keeps the read-plus-sidecar boundary enforced by the database, not just the application:

```sql
-- password: openssl rand 24 | base64 | sed 's/[/10lO#+=]//g'
CREATE ROLE epistula_api LOGIN PASSWORD '<generated>';
GRANT CONNECT ON DATABASE epistula_database TO epistula_api;
GRANT USAGE ON SCHEMA public TO epistula_api;
GRANT SELECT ON mailboxes, domains, aliases, folders, messages, attachments TO epistula_api;
GRANT SELECT, UPDATE (last_used_at) ON api_tokens TO epistula_api;
GRANT SELECT, INSERT, UPDATE ON message_annotations TO epistula_api;
-- the annotation model registry (epistula-database migration 008): ranks a
-- message's annotations; without it they are served unranked
GRANT SELECT ON annotation_models TO epistula_api;
GRANT EXECUTE ON FUNCTION mail_lock_message_for_annotation(bigint) TO epistula_api;
-- archive sorting (epistula-database migration 020): read the approved category
-- list, write the classifier's choice from it
GRANT SELECT ON archive_categories TO epistula_api;
GRANT SELECT, INSERT, UPDATE ON message_classifications TO epistula_api;
-- the annotation pass queue (epistula-database migration 022): read it, and clear
-- the markers of messages the pipeline has finished
GRANT SELECT, DELETE ON annotation_pass_required TO epistula_api;
-- no other DELETE; no writes to messages/folders; no sequence grants needed
-- (no table this role writes has a serial column).
```

An annotation write holds its message row locked so it cannot race IMAP MOVE.
A row lock needs UPDATE privilege on `messages`, which this role must not have,
so the lock is taken by `mail_lock_message_for_annotation`, a `SECURITY DEFINER`
function that does nothing else (epistula-database migration 019, OPS-008). The
migration grants it to every role that can already insert annotations; a role
created after the migration needs the `GRANT EXECUTE` above.
`annotation_role_ops008_test.go` applies this block verbatim to a fresh role.

A classification write (`PUT /v1/messages/{id}/classification`) takes the same
lock through the same function. Migration 020 grants the two archive-sorting
tables to every role that can already insert annotations; a role created after
it needs the two grants above. Without them `epistula-api` starts, logs a warning,
and answers the archive-sorting endpoints with 503.

The annotation pass queue (`pass_required=true`, `POST /v1/pass-required/prune`)
works the same way: migration 022 grants `annotation_pass_required` to the
existing role, a role created after it needs the grant above, and without it
those two answer 503 while everything else works.

## Blob access

`GET /v1/messages/{id}/raw` streams from epistula-database's per-mailbox blob tree (`<mailbox>/raw/...`). Put the `epistula-api` user in the storage group (the same group the IMAP server reads with) so the blobs are group-readable; the daemon never writes blobs.

## Smoke test after install

```sh
service epistula_api start
curl -s http://127.0.0.1:8785/healthz                   # expect 200 ok

# Mint a token (on the epistula-database side) and exercise the API:
epistula-database admin api-token-add -name smoke -mailboxes "*" \
    -permission read_metadata
curl -s -H "Authorization: Bearer mapi_..." http://127.0.0.1:8784/v1/mailboxes
```

Pagination supports `limit=default:<positive-cap>`: the effective page size is the smaller of the configured API default and caller cap. Numeric limits retain their existing behavior. MCP uses this for omitted/zero/negative tool limits. Each tool makes one API request.

Message inspection is additive: `/v1/messages/{id}?metadata=inspection&text_limit=65536` excludes HTML/header/structure payloads and returns bounded body preview plus four attachments and four model sidecars. Continue with returned `next_attachment` as `attachment_after`, and `next_annotation` as `annotation_after`. Each summary includes its byte count/effective offset; continue a long summary with `annotation_model=<model>&summary_offset=<summary_next_offset>`. Text and summary windows preserve UTF-8 bytes. The ordinary full-document default remains complete.

Inspection model pages use stable model-name order; `primary` still identifies the globally preferred non-retired model. Calls read current sidecars rather than retaining a snapshot between calls, so restart a traversal if concurrent additions before a cursor must be included. Each MCP call still makes one request. `/text` also projects its byte range in PostgreSQL before transfer.

Export write inactivity is enforced for each 8 KiB write/flush unit, including inside large rows. The default 30-minute budget accommodates worker inference pauses and is independent of total export duration. Configure it above the worker's longest legitimate pause. Flush failures abort the stream and release capacity; unsupported deadline enforcement refuses export. The explicit `0s` override still disables enforcement and requires an external bound.
