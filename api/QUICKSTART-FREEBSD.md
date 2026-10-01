# Epistula — FreeBSD Quick Start — epistula-api

**Stage 2.** This assumes the mail store is already standing on this host —
you finished [`epistula-database`'s
QUICKSTART-FREEBSD.md](../database/QUICKSTART-FREEBSD.md):
Postgres role + `epistula_database` DB, schema applied (including the `api_tokens`
and `message_annotations` migrations this API depends on), the blob store at
`/var/spool/epistula-database`, and at least one mailbox provisioned.

Here you stand up the **read-and-annotate JSON API**: a loopback HTTP listener
that programmatic consumers (the LLM worker, indexers, analytics) query for
decoded message text and write annotations back through. It owns no schema and
makes no IMAP-state changes — it reads the store and writes one derived sidecar
table.

```text
LLM worker / indexer ── HTTPS+Bearer ──► Apache (TLS) ──► epistula-api (127.0.0.1:8784)
                                                              ├─► Postgres (read + message_annotations upsert)
                                                              └─► raw/ blobs (read-only, group access)
```

> **Co-location assumption:** this guide runs `epistula-api` on the same host as
> `epistula-database` and Postgres (the example DSN is `127.0.0.1:5432` and blob
> reads are local group access). For a remote consumer host, front this
> listener with mTLS at Apache and replicate/NFS the blob tree — out of scope
> here.

## Prerequisites

- `epistula-database` quickstart complete on this host; `epistula-database migrate status`
  all `applied` (so `api_tokens` + `message_annotations` exist).
- Root/`sudo`; the `epistula-database` service account, group, and
  `/var/spool/epistula-database` blob tree already exist.

---

## 0. Variables

```sh
SVC_USER="epistula-api"
SVC_GROUP="epistula-api"
STORAGE_GROUP="epistula-database"                          # epistula-database's blob group
CONFIG="/usr/local/etc/epistula/epistula-api.toml"
BIN="/usr/local/bin/epistula-api"

PG_DB="epistula_database"                                  # same DB as epistula-database
PG_ROLE="epistula_api"                                     # dedicated least-priv role
PG_PASS="$(openssl rand -base64 24 | tr -d '/+=')"

echo "epistula_api PG pass: ${PG_PASS}"                    # save it; goes into the config in step 3
```

Build & copy the binary as in the parent guide (`make build-freebsd` →
`${BIN}`, `chmod 755`), then `"${BIN}" version`.

---

## 1. Service account + blob-group membership

```sh
pw groupadd -n "${SVC_GROUP}" 2>/dev/null || true
pw useradd  -n "${SVC_USER}" -g "${SVC_GROUP}" -d /nonexistent \
            -s /usr/sbin/nologin -c "Epistula api" 2>/dev/null || true

# GET /v1/messages/{id}/raw streams from epistula-database's per-mailbox blob tree
# (<mailbox>/raw/...). Put the epistula-api user in the storage group so the blobs
# are group-readable.
pw groupmod "${STORAGE_GROUP}" -m "${SVC_USER}"

# rc.d log file + run dir are created by the rc.d prestart on start; nothing
# to pre-create here.
```

---

## 2. PostgreSQL — dedicated least-privilege role

A read-plus-sidecar role enforces the API's boundary at the database, not just
in code. Run on this host as the DB owner:

```sh
su - postgres -c "psql -v ON_ERROR_STOP=1 -d ${PG_DB} <<SQL
CREATE ROLE ${PG_ROLE} LOGIN PASSWORD '${PG_PASS}';
GRANT CONNECT ON DATABASE ${PG_DB} TO ${PG_ROLE};
GRANT USAGE ON SCHEMA public TO ${PG_ROLE};
GRANT SELECT ON mailboxes, domains, aliases, folders, messages, attachments TO ${PG_ROLE};
GRANT SELECT, UPDATE (last_used_at) ON api_tokens TO ${PG_ROLE};
GRANT SELECT, INSERT, UPDATE ON message_annotations TO ${PG_ROLE};
GRANT SELECT ON annotation_models TO ${PG_ROLE};
GRANT EXECUTE ON FUNCTION mail_lock_message_for_annotation(bigint) TO ${PG_ROLE};
GRANT SELECT ON archive_categories TO ${PG_ROLE};
GRANT SELECT, INSERT, UPDATE ON message_classifications TO ${PG_ROLE};
GRANT SELECT, DELETE ON annotation_pass_required TO ${PG_ROLE};
SQL"
```

These are the grants in [`deploy/README.md`](deploy/README.md). The schema
migrations grant their tables and the lock function only to roles that already
existed when they ran, so a role created afterwards, like this one, needs every
line:

- `EXECUTE` on `mail_lock_message_for_annotation` — migration 019 revokes the
  function from `PUBLIC`, and every annotation and classification write locks
  its message through it. Without the grant those writes fail with
  `permission denied`.
- `annotation_models` — ranked reads (`GET /v1/messages/{id}`,
  `fields=annotation`, every `/v1/export` batch) `LEFT JOIN` the model registry.
  The startup probe checks `has_table_privilege`, so a missing grant degrades to
  *unranked* annotations with a WARN rather than failing requests.
- `archive_categories` / `message_classifications` (archive sorting) and
  `annotation_pass_required` (the annotation pass queue) — without them the
  daemon starts, logs a warning, and answers the requests that need them with
  `503`.

No `DELETE` except on the pass queue (prune clears the markers of finished
messages), no write on `messages`/`folders`, no other tables — no table this
role writes has a serial column, so no sequence grant is needed.

---

## 3. Configuration

```sh
install -o root -g "${SVC_GROUP}" -m 640 epistula-api.toml.example "${CONFIG}"

sed -i '' \
  -e "s#^dsn .*#dsn               = \"postgres://${PG_ROLE}:${PG_PASS}@127.0.0.1:5432/${PG_DB}?sslmode=disable\"#" \
  "${CONFIG}"

grep -E '^(dsn|root|listen_addr)' "${CONFIG}"
```

Confirm `[storage] root` matches epistula-database's `storage.root`
(`/var/spool/epistula-database`). Leave `production = false` and `sslmode=disable`
for the loopback bring-up; the **Going to production** section flips both.

---

## 4. Validate, start, check health

```sh
"${BIN}" check-config -config "${CONFIG}"     # must exit 0

install -m 555 deploy/freebsd/epistula_api /usr/local/etc/rc.d/epistula_api
sysrc epistula_api_enable=YES
service epistula_api start

sockstat -4 -l | grep -E '878[45]'
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8785/healthz   # expect 200
```

### Log rotation (newsyslog)

The rc.d prestart re-asserts `epistula-api` ownership on `/var/log/epistula_api.log`
every start, so a stray root-owned file never blocks startup. To keep *rotations*
owned by the service account too — otherwise a rotation recreates the log
root-owned and the next restart has to fix it — drop a newsyslog entry
(fields: path, owner:group, mode, count, size, when, flags):

```sh
# /etc/newsyslog.conf.d/epistula_api.conf
# logfilename                 owner:group     mode count size  when  flags
# /var/log/epistula_api.log       epistula-api:epistula-api  640  7     *     @T00  J
```

Uncomment after installing; `newsyslog -nv` dry-runs the rule.

---

## 5. Mint a token and exercise the API

Tokens are minted on the `epistula-database` side (it owns `api_tokens`); `epistula-api`
only verifies. A read-only token is enough to prove the wiring:

```sh
epistula-database admin api-token-add -name smoke -mailboxes "*" -permission read_metadata
# prints mapi_... once

curl -s -H "Authorization: Bearer mapi_..." http://127.0.0.1:8784/v1/mailboxes
```

A `200` with your mailbox list means Postgres reads, token verification, and
scope filtering all work. For the LLM worker, mint a
`read_content + write_annotation` token instead (see that project's guide).

---

## 6. Front with Apache (TLS, optional mTLS)

```sh
# Include deploy/apache/epistula-api-include.conf in the TLS vhost that publishes
# /v1. It reverse-proxies the loopback listener, restricts /metrics, and leaves
# /health public. Add SSLVerifyClient in the surrounding vhost for mTLS when a
# consumer lives off-box.
apachectl configtest && service apache24 reload
```

---

## Going to production

```sh
# In ${CONFIG}:
#   production = true
#   [postgres] dsn = "...?sslmode=verify-full&sslrootcert=/path/to/ca.crt"
"${BIN}" check-config -config "${CONFIG}"     # strict mode rejects disable/allow sslmode
service epistula_api restart
```

`production = true` requires `sslmode=verify-full` (or `verify-ca`) on the DSN
and keeps both the API and admin listeners on loopback. Provision the Postgres
TLS cert/CA before flipping it.

---

## Troubleshooting

**`check-config` fails / won't start** — the rc.d prestart runs it and refuses
to start on failure; re-run `"${BIN}" check-config -config "${CONFIG}"` for the
reason.

**`403` on `/v1/mailboxes/{name}/...`** — the token is authenticated but not
scoped to that mailbox. Re-mint with the right `-mailboxes`. `401` is a bad/absent
token; `404` is a genuinely-absent resource.

**`GET .../raw` returns 5xx / permission error** — the `epistula-api` user isn't in
the `${STORAGE_GROUP}` group, or the blob tree isn't group-readable. Re-check
step 1 and `ls -l /var/spool/epistula-database/*/raw` (blobs are partitioned per
mailbox, so there is no top-level `raw/` — each mailbox has its own subtree).

**`503` on `PUT .../annotation`** — the `message_annotations` migration isn't
applied. Run `epistula-database migrate up` on the store.
