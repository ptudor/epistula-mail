# Epistula — database

A Postfix LDA that ingests RFC 5322 mail into a PostgreSQL-backed store. The companion `imap` component reads from the same schema to serve IMAP.

## Design goal

Move from a per-message-file Maildir to a hybrid store:

- **Postgres** holds parsed message metadata, headers, the extracted plain-text and HTML bodies, IMAP state (UID, flags, mailbox membership), and the routing tables that map `user@domain` (and wildcards) to mailboxes.
- **Disk** holds two content-addressed blob trees: the original raw RFC 5322 message (so we can always reproduce `FETCH BODY[]` byte-for-byte) and extracted attachments. Both are sha256-named, partitioned per mailbox and by date, and sharded by the first two bytes of the hash, so a single newsletter delivered to N addresses of one mailbox stores one blob, not N.

PostgreSQL provides transactional message metadata and full-text search (`tsvector`), while immutable blobs preserve the original message bytes.

## Common patterns (inherited from parent)

| Pattern | Implementation |
|---------|----------------|
| **Transport** | Postfix `pipe` for delivery; no FastCGI socket for the LDA. `serve` exposes only `/health`, `/healthz`, `/metrics` (no admin web UI — see "Management interface" below). |
| **Logging** | Structured logging via `log/slog` (JSON or text) |
| **Metrics** | Prometheus + expvar endpoints |
| **Graceful Shutdown** | SIGINT/SIGTERM with in-flight delivery completion |
| **Configuration** | TOML config file (preferred) or environment variables |
| **Health Checks** | `/health` and `/healthz` endpoints (serve mode only) |
| **Database** | PostgreSQL only — no MySQL/MariaDB driver. Bytea + JSONB are central to the design. |
| **Errors** | `sysexits.h` exit codes for `deliver` and the CLI subcommands |

### Configuration

TOML preferred, env-var fallback, same precedence as the other Epistula daemons:

```bash
epistula-database deliver -config /usr/local/etc/epistula/epistula-database.toml
epistula-database serve   -config /usr/local/etc/epistula/epistula-database.toml
epistula-database import  -config /usr/local/etc/epistula/epistula-database.toml --maildir /path/to/Maildir
epistula-database admin   mailbox-add -name jdoe -password-stdin
epistula-database admin   domain-add  -name example.invalid
epistula-database admin   alias-add   -domain example.invalid -localpart jdoe -mailbox jdoe
```

Default search paths:
```
/usr/local/etc/epistula/epistula-database.toml
/etc/epistula/epistula-database.toml
./epistula-database.toml
```

### Dependencies

- `github.com/jackc/pgx/v5` — PostgreSQL driver.
- `github.com/prometheus/client_golang` — metrics
- `github.com/pelletier/go-toml/v2` — config
- `golang.org/x/crypto/argon2` — mailbox password hashing (Argon2id). There is no admin-account schema or code; operator management is the `admin` CLI.
- `golang.org/x/net/html` — HTML→text projection for messages with no text/plain part
- Standard library: `net/mail`, `mime`, `mime/multipart`, `mime/quotedprintable`, `encoding/base64`, `crypto/sha256`, `crypto/subtle`

**Password hashing:** Epistula uses bounded Argon2id verification. The encoded hash format (`$argon2id$v=19$m=65536,t=3,p=4$...`) carries its parameters, so future cost tuning does not require a schema migration.

Go toolchain: use [the repository pin](../.go-version), including when building this component alone.

---

## Architecture

```
Postfix transport (pipe)
   │
   ▼
epistula-database deliver  ──► parse RFC 5322 ──► extract parts ──► write blobs ──► single PG tx
                                                                   │              │
                                                                   ▼              ▼
                                   <mailbox>/raw/<yyyy/mm/dd>/<aa>/<bb>/<sha>.eml messages + attachments rows
                                   <mailbox>/att/<yyyy/mm/dd>/<aa>/<bb>/<sha>.bin

epistula-database serve    ──► loopback metrics + health only
epistula-database import   ──► one-shot Maildir → DB importer (cur/ + new/, flags from :2,X suffix)

imap  ◄── reads schema, serves IMAP4rev1 / RFC 9051 IMAP4rev2
```

The LDA process is **stateless and short-lived** — Postfix spawns one per message, it streams stdin, commits or fails atomically, exits with a `sysexits.h` code. The `serve` process is long-lived only for health and metrics. They share zero in-memory state; Postgres is the only coordination point.

## Subcommands

| Subcommand | Purpose |
|------------|---------|
| `deliver` | Read RFC 5322 from stdin, parse, store, exit with sysexits code. Invoked by Postfix `pipe`. |
| `serve` | Long-running HTTP listener (loopback by default) for Prometheus `/metrics`, `/health`, `/healthz`. Operator management (CRUD, log browsing, password resets) lives in the `admin` subcommand — `serve` does NOT host an admin UI or admin REST API. |
| `admin` | Operator CLI for domain/mailbox/alias/folder CRUD, password resets, mailbox enable/disable, delivery-log inspection. The management surface. |
| `import` | Walk a Maildir tree and ingest every message through the same code path as `deliver`. Idempotent — re-import is a no-op because raw-blob sha256 + recipient is the dedup key. |
| `import-blobs` | Disaster recovery: walk the per-tenant `<mailbox>/raw/yyyy/mm/dd` blob tree itself and re-ingest every `.eml` after Postgres loss. The owning mailbox is now encoded in the path, so each blob is recovered back into **its own** mailbox automatically (a tenant subtree with no mailbox row is skipped with a warning; `-mailbox` optionally restricts recovery to one subtree). The bucket date becomes the INTERNALDATE fallback. Folder placement and flags remain PG-only state, not recoverable from disk — prefer a `pg_dump` restore when one exists. Idempotent on (folder, sha256). |
| `gc` | Garbage-collect blob files no longer referenced by any `messages` or `attachments` row. Safe to run while `deliver` is active (uses generational marking). |
| `version` | Print version and exit. |

## Storage layout

### On-disk blob trees

```
${storage_root}/<mailbox>/raw/2026/05/18/aa/bb/aabbccdd...eef.eml   # original RFC 5322 bytes
${storage_root}/<mailbox>/att/2026/05/18/aa/bb/aabbccdd...eef.bin   # extracted attachment payload (post-decode)
${storage_root}/<mailbox>/tmp/blob-*                                # atomic-write staging (per tenant — see below)
${storage_root}/<mailbox>/raw/1970/01/01/aa/bb/aabbccdd...eef.eml   # date-undetermined sentinel (see below)
```

- **Tenant (mailbox) isolation** is the outermost level: `<mailbox>` is the owning mailbox's canonical `name`. A user's entire corpus is a single subtree, so `rsync ${storage_root}/jdoe/` backs up exactly one user, `rm -rf ${storage_root}/jdoe/` removes one user cleanly, and each `${storage_root}/<mailbox>` can be its own ZFS dataset (per-user snapshots, quotas, and native-encryption keys for free). Dedup is therefore **intra-mailbox**: one user pointing many domains at one inbox still stores one blob, but the same bytes delivered to two different mailboxes are two files in two subtrees — the intended isolation. The tenant name is validated to a single path-safe component (`^[a-z0-9][a-z0-9._-]{0,63}$`, the `blob.ParseTenant` charset, also enforced by the `mailboxes.name` CHECK constraint and `admin mailbox-add`), eliminating path traversal.
- **Renaming a mailbox is a two-part operation**, because the name *is* the tenant path. `admin mailbox-rename` owns the Postgres half — the `mailboxes.name` row. It no longer has to rewrite API-token scopes: since migration 012 (RA6X-012) `api_tokens` stores durable mailbox **ids** (`scope_mailbox_ids`, plus a `scope_all_mailboxes` boolean for the wildcard), so a rename leaves every token pointing at the same account and a deleted name can never be reused to inherit an old token's grant. It deliberately does not touch the filesystem: on a per-mailbox-dataset deployment the move is `zfs rename`, not `os.Rename`, and it has to happen with deliveries stopped. The command therefore refuses unless `<storage_root>/<old>` is already gone and `<storage_root>/<new>` is already there, so a half-applied rename that splits the store cannot be reached by running it. The IMAP login name changes with it.
- **Rename is an enforced offline operation for the whole blob store** (RA6X-013). Updated IMAP/API daemons retain a shared lock on the storage-root directory until process exit; delivery/import/verification/reparse/GC retain it for their entire blob operation. Maintenance on/off and rename require the exclusive lock and refuse while any participant is active. Stop and drain IMAP/API, Postfix and maintenance jobs first. The durable flag then refuses new blob processes between commands. All participants must use updated binaries and the same mounted root; move only tenant subtrees, keeping the root in place. See [the deployment procedure](deploy/README.md#offline-mailbox-rename). No database connection is reserved.
  ```sh
  # Stop IMAP/API, Postfix and all import/reparse/GC processes; wait for exit.
  epistula-database admin mailbox-maintenance -name old -on
  # Move <storage_root>/old to <storage_root>/new (zfs rename on dataset hosts).
  epistula-database admin mailbox-rename -from old -to new -yes
  # Restart services after successful size/hash verification and rename.
  ```
  If the move succeeded but `mailbox-rename` failed, nothing is lost: the mailbox stays quiesced so no writer can act on the disagreement. Fix what the command reported and re-run, or restore the complete old tree and `-off` maintenance to abandon the rename. That release also verifies every referenced size/hash. Do not run `gc` until one of those is done.
- **Date partitioning** by `yyyy/mm/dd` sits inside the tenant, in front of the sha-shard. Makes per-period rsync / backup / cold-archive policies trivial ("backup only what's new since last week"), keeps any single day's bucket bounded, and gives operators an at-a-glance sense of growth from `du -sh ${storage_root}/jdoe/raw/2026/*`.
- **Date is the wall-clock arrival time** for live deliveries (`deliver` invokes `time.Now()` once at the start of the LDA run) and the message's parsed `Date:` header (falling back to the file mtime) for the one-time `import` of legacy Maildir trees. Dedup runs only within the (tenant, bucket) — two identical bytes arriving on different days produce two on-disk blobs. That's fine: most spam/newsletter dedup wins are within a single day anyway, and almost every modern message carries per-recipient UUID tracking URLs that defeat cross-day dedup regardless. The simpler model wins.
- The chosen bucket date is persisted alongside the sha256 in the DB (`messages.raw_blob_date DATE`, `attachments.blob_date DATE`) so reads never have to guess the path; the tenant is the owning mailbox, reachable via `folder_id → folders.mailbox_id → mailboxes.name`.
- Two-byte sha sharding is retained inside the date bucket. Even a 100M-message day stays under ~1.5k files per leaf dir.
- Messages whose import date can't be determined at all (no parseable Date, no mtime, no `Received:`) bucket under `1970/01/01` — a real, parseable date that predates email so it's an unambiguous "unknown" sentinel without forcing a magic-string special case in the path parser. Should be rare in practice.
- **`tmp/` is per-tenant**, a sibling of `raw/`/`att/` *inside* each mailbox subtree — not a single shared `${storage_root}/tmp`. The finalize step is `os.Link` (no cross-device rename fallback), so the staging file must live on the same filesystem as its final path; when each tenant is its own ZFS dataset, a shared tmp would make every link cross-device (`EXDEV`). Writes are tmp → fsync → `link` (atomic; `EEXIST` is the per-(tenant,bucket) dedup hit) → fsync parent dir.
- Permissions: dirs 0750, files 0640, owned by the daemon user, group readable by the IMAP server's user. Per-tenant subtrees are created on demand at first write.
- `fsync` the file and the parent directory before committing the PG transaction. Durability ordering: blob on disk → row in DB. A blob with no row is GC'd later; a row with no blob is the bug we never want.

### Postgres schema (sketch — `schema.sql` and the migrations are authoritative)

```sql
CREATE TABLE domains (
    id              BIGSERIAL PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,            -- 'example.invalid'
    is_wildcard     BOOLEAN NOT NULL DEFAULT FALSE,  -- accept *@name
    catchall_box_id BIGINT REFERENCES mailboxes(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE mailboxes (
    id            BIGSERIAL PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,    -- 'jdoe'
    quota_bytes   BIGINT,                  -- NULL = unlimited
    used_bytes    BIGINT NOT NULL DEFAULT 0, -- maintained in-tx by deliver/expunge; reconciled by `gc reconcile-quotas`
    password_hash TEXT NOT NULL,           -- Argon2id PHC-encoded ($argon2id$v=19$m=65536,t=3,p=4$...)
    disabled_at   TIMESTAMPTZ,             -- non-NULL means auth rejected; messages still deliverable
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE aliases (
    id          BIGSERIAL PRIMARY KEY,
    domain_id   BIGINT NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
    localpart   TEXT NOT NULL,           -- '' means catchall for the domain
    mailbox_id  BIGINT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
    UNIQUE (domain_id, localpart)
);

CREATE TABLE domain_acl (                -- per-domain allowlist / denylist of localparts
    id           BIGSERIAL PRIMARY KEY,
    domain_id    BIGINT NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
    localpart    TEXT NOT NULL,          -- normalized (lower + NFC); '' is not allowed here
    kind         TEXT NOT NULL CHECK (kind IN ('allow', 'deny')),
    note         TEXT,                   -- optional human reason for the entry
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (domain_id, localpart, kind)
);
CREATE INDEX domain_acl_lookup_idx ON domain_acl (domain_id, localpart, kind);

CREATE TABLE folders (                   -- IMAP folders per mailbox
    id          BIGSERIAL PRIMARY KEY,
    mailbox_id  BIGINT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,           -- 'INBOX', 'Sent', 'Archive/2026'
    uidvalidity BIGINT NOT NULL,         -- bumped on rename/recreate
    uidnext     BIGINT NOT NULL DEFAULT 1,
    UNIQUE (mailbox_id, name)
);

CREATE TABLE messages (
    id              BIGSERIAL PRIMARY KEY,
    folder_id       BIGINT NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
    uid             BIGINT NOT NULL,                  -- IMAP UID, monotonic per folder
    raw_sha256      BYTEA NOT NULL,                   -- → <mailbox>/raw/<yyyy/mm/dd>/<aa>/<bb>/<hex>.eml
    raw_size        BIGINT NOT NULL,
    internal_date   TIMESTAMPTZ NOT NULL,             -- IMAP INTERNALDATE
    message_id      TEXT,                             -- Message-ID header
    in_reply_to     TEXT,
    subject         TEXT,
    from_addr       TEXT,
    to_addrs        TEXT[],
    cc_addrs        TEXT[],
    sent_date       TIMESTAMPTZ,                      -- parsed Date: header
    headers         JSONB NOT NULL,                   -- full header set, case-preserved
    text_body       TEXT,                             -- decoded text/plain part(s)
    html_body       TEXT,                             -- decoded text/html part(s)
    bodystructure   JSONB NOT NULL,                   -- precomputed IMAP BODYSTRUCTURE
    flags           TEXT[] NOT NULL DEFAULT '{}',     -- \Seen, \Answered, \Flagged, ...
    fts             TSVECTOR                          -- generated from subject + text_body
        GENERATED ALWAYS AS (
            setweight(to_tsvector('simple', coalesce(subject, '')), 'A') ||
            setweight(to_tsvector('simple', coalesce(text_body, '')), 'B')
        ) STORED,
    UNIQUE (folder_id, uid)
);

CREATE INDEX messages_fts_idx        ON messages USING GIN (fts);
CREATE INDEX messages_headers_idx    ON messages USING GIN (headers);
CREATE INDEX messages_folder_date    ON messages (folder_id, internal_date DESC);
CREATE INDEX messages_message_id_idx ON messages (message_id);

CREATE TABLE attachments (
    id            BIGSERIAL PRIMARY KEY,
    message_id    BIGINT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    part_number   TEXT NOT NULL,        -- IMAP part path: '2', '2.1', etc.
    filename      TEXT,
    content_type  TEXT NOT NULL,
    content_id    TEXT,                 -- for inline images
    disposition   TEXT,                 -- 'inline' | 'attachment'
    size_bytes    BIGINT NOT NULL,
    sha256        BYTEA NOT NULL        -- → <mailbox>/att/<yyyy/mm/dd>/<aa>/<bb>/<hex>.bin
);
CREATE INDEX attachments_sha256_idx ON attachments (sha256);

CREATE TABLE delivery_log (              -- audit trail, retained per policy
    id            BIGSERIAL PRIMARY KEY,
    received_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    envelope_from TEXT,
    envelope_to   TEXT,
    matched_alias_id BIGINT REFERENCES aliases(id),
    message_id    BIGINT REFERENCES messages(id) ON DELETE SET NULL,
    bytes         BIGINT NOT NULL,
    outcome       TEXT NOT NULL          -- 'delivered' | 'rejected:<reason>' | 'duplicate'; a salvaged parse is '<verb>:degraded' (OPS-003)
);
```

### Resolved schema decisions

- **One `messages` row per recipient mailbox.** IMAP flags (`\Seen`, `\Flagged`, etc.) are per-mailbox; modelling them on a shared row forces a `message_flags(message_id, mailbox_id, flags[])` join and every STORE becomes a multi-row mutation. The storage cost of duplicating metadata rows is negligible vs. the blob, which is already deduped on disk. The win for IMAP semantics is large.
- **`mailboxes.used_bytes` maintained transactionally in Go, not by SQL triggers.** Every `INSERT INTO messages` and `DELETE FROM messages` updates `used_bytes` in the same transaction. No triggers — keeping the logic in Go makes it visible, testable, and free of `pg_dump`/`pg_restore` ordering hazards. `epistula-database gc reconcile-quotas` walks all mailboxes and rewrites `used_bytes` from `SUM(raw_size)`; safe to run hot, takes a brief `FOR UPDATE` row lock per mailbox.
- **HTML stored as decoded UTF-8 in `html_body`; IMAP serves it untouched.** Sanitization is the MUA's responsibility, not ours. Full-text search runs over `subject + text_body` only. If a message has *only* `text/html` and no `text/plain` part, we generate a plain-text projection at ingest using `golang.org/x/net/html` (tokenize, strip tags, collapse whitespace) and store it in `text_body` — so FTS still works and plaintext-only IMAP clients can FETCH something useful. The original raw bytes are always preserved on disk; this projection is purely derived.

## Delivery flow (the LDA hot path)

1. `deliver` reads stdin into a bounded buffer (configurable max, default 50 MiB; if exceeded, reject `EX_DATAERR` so Postfix bounces — matching the MIME-limits table below and `deliver.go`. Requeuing an oversize message would only retry the same too-large bytes forever, so a bounce is correct).
2. Compute `sha256` of the raw bytes in-flight.
3. Parse with `net/mail` to get headers and recursive MIME parts.
4. Walk MIME tree:
   - `text/plain` → accumulate into `text_body` (decode transfer-encoding, decode charset to UTF-8).
   - `text/html` → accumulate into `html_body`.
   - Anything else → write to `att/<aa>/<bb>/<sha>.bin` if not present, record `(filename, content_type, sha256, size)` for the attachment row.
   - A `text/plain` or `text/html` part that is explicitly attached (`Content-Disposition: attachment`, or `inline` with a filename) is both: text and an attachment row (RA6X-049). Every part yields at most one attachment row (OPS-006).
5. Resolve envelope recipient(s) against `domains` + `aliases` + `domain_acl`. Resolution order, applied per recipient:
   1. **Denylist check.** If `(domain_id, localpart, 'deny')` exists in `domain_acl`, reject with `EX_NOUSER` (67), `delivery_log.outcome = 'rejected:denylist'`. Deny ALWAYS wins, even over an explicit alias — operator intent is unambiguous.
   2. **Exact alias.** `aliases(domain_id, localpart)` match → deliver to that mailbox.
   3. **Wildcard / catchall.** If the domain has `is_wildcard = true` or a `catchall_box_id`, then:
      - If the domain has any `(domain_id, _, 'allow')` rows configured, the localpart MUST match an allowlist row. Otherwise reject with `delivery_log.outcome = 'rejected:not-allowlisted'`. (An allowlist's presence makes the wildcard opt-in per address.)
      - If no allowlist rows exist, accept any localpart and route to the catchall mailbox.
      - **Allow rows on a standard (non-wildcard, no-catchall) domain are intentionally inert.** Resolution on a standard domain ends at step 2: an address either has an explicit alias or it doesn't, so there is no open acceptance for an allowlist to gate. Only deny rows (step 1) are meaningful there. `acl-add -allow` on such a domain is accepted (the domain may become a wildcard later) but has no effect until it does. This is the intended resolver behavior.
   4. **No match.** Reject `EX_NOUSER`, `outcome = 'rejected:no-mailbox'`.
   Each resolved mailbox gets its own `messages` row and its own copy of the raw blob under its tenant subtree (Postfix invokes the LDA once per recipient).
6. Write `<mailbox>/raw/<yyyy/mm/dd>/<aa>/<bb>/<sha>.eml` if not present. `fsync` file + dir.
7. Open one PG transaction:
   - Allocate UID per folder (`UPDATE folders SET uidnext = uidnext + 1 RETURNING uidnext - 1`).
   - INSERT into `messages`.
   - INSERT into `attachments`.
   - INSERT into `delivery_log` (outcome = 'delivered').
   - COMMIT.
8. Exit `EX_OK` (0). Any failure: exit `EX_TEMPFAIL` (75) for retryable errors (DB unavailable, disk full), `EX_NOUSER` (67) for unknown recipient, `EX_DATAERR` (65) for a message whose own header does not parse or that exceeds a MIME limit. **Never `EX_SOFTWARE` for routine errors** — Postfix interprets that as bounce-worthy.

## Maildir import

`epistula-database import --maildir <path> --mailbox <name> [--folder INBOX]`:

- Walk `cur/` and `new/` (skip `tmp/`).
- For each file: parse the Maildir filename for flags (`:2,S` → `\Seen`, `:2,FR` → `\Flagged \Answered`, etc.) and use file mtime as `INTERNALDATE` if no `Received:` header gives something better.
- Run through the same MIME-extraction code path as `deliver`.
- Idempotent: if `(folder_id, raw_sha256)` already exists, skip with `outcome='duplicate'` in the log.
- A nested `--folder` (`Sent/2004/11-Nov`; the hierarchy delimiter is `/`) also creates every missing ancestor (`Sent`, `Sent/2004`), as IMAP CREATE does. All folder creation — delivery, import, IMAP CREATE, COPY/MOVE auto-create and RENAME's destination — goes through `storage.EnsureFolder` (OPS-001). Folders imported before that can be repaired with `admin folder-repair-ancestors`.
- A target folder that `admin folder-merge` emptied and `folder-prune-empty` removed is imported into the merge's destination instead of being re-created (migration 021, `folder_redirects`; `storage.ResolveImportFolder`). A Maildir sync after the Sent tree was merged into "Sent Messages" therefore lands there, and per-folder dedup recognises what the merge moved. The redirect applies only while the target folder does not exist; a subtree merge also covers new folders below its source; `archive-undo` of the merge batch removes it.
- Import never assigns an RFC 6154 special-use (`\Sent`, `\Drafts`, …): which of several candidate folders plays a role is an operator decision, made with `admin folder-set-special-use` (OPS-002).
- Import salvages rather than refuses (OPS-003, see the MIME limits section): a malformed or over-limit message is imported with outcome `imported:degraded`. A file that still fails, such as one whose header net/mail rejects, stays in `<checkpoint>.failures.d/`, and `-retry-failures` reprocesses exactly those files.

This optional command imports an existing Maildir into a chosen Epistula mailbox and folder. It is not required for a new installation.

## Management interface

**Go CLI only.** All operator management is performed via the `admin` subcommand against the same Postgres database the LDA writes. No web UI ships with the Go daemon. The `serve` process exposes Prometheus `/metrics`, `/health`, `/healthz` on a loopback admin listener and nothing else.

The Go code owns the Postfix delivery path, blob store, parser/ingest, IMAP server (in [`imap`](../imap/)), GC, quota reconciliation, migrations / schema runtime decisions, and the `admin` CLI. A web operator dashboard is not included; external administration tools may read and write the same Postgres schema directly (they are not HTTP clients of this daemon).

Keeping admin out of the Go daemon means: no HTML templating in the LDA, no admin session/cookie/CSRF surface to harden, and no auth scheme for "admin users" distinct from "mailbox users." The Argon2id encoded-hash format in `mailboxes.password_hash` is the integration contract for any external tool that sets passwords (for example with `passlib` or `argon2-cffi`).

### `admin` CLI subcommands

```
epistula-database admin domain-add     -name N [-wildcard]
epistula-database admin domain-list
epistula-database admin domain-delete  -name N

epistula-database admin mailbox-add    -name N [-quota-bytes B] [-password-stdin]
epistula-database admin mailbox-list
epistula-database admin mailbox-passwd -name N [-password-stdin]
epistula-database admin mailbox-disable -name N
epistula-database admin mailbox-enable  -name N
epistula-database admin mailbox-delete  -name N
epistula-database admin mailbox-rename  -from OLD -to NEW -yes

epistula-database admin alias-add    -domain D -mailbox M (-localpart L | -catchall)
epistula-database admin alias-list   [-domain D]
epistula-database admin alias-delete -domain D -localpart L

epistula-database admin folder-list  -mailbox M
epistula-database admin folder-repair-ancestors (-mailbox M | -all) [-dry-run]
epistula-database admin folder-set-special-use  -mailbox M -folder F (-use ATTR | -clear) [-dry-run]
epistula-database admin log-tail     [-since DUR] [-mailbox M] [-outcome STR]

epistula-database admin acl-add      -domain D -localpart L (-allow | -deny) [-note "..."]
epistula-database admin acl-list     [-domain D] [-allow | -deny]
epistula-database admin acl-delete   -domain D -localpart L (-allow | -deny)

epistula-database admin api-token-add    -name N -mailboxes ("*" | "a,b") -permission P [-permission P ...]
epistula-database admin api-token-list   [-all]
epistula-database admin api-token-revoke -name N

epistula-database admin annotation-model-set    -model M -priority N [-display "..."]
epistula-database admin annotation-model-list   [-all]
epistula-database admin annotation-model-retire -model M

epistula-database admin folder-create           -mailbox M -folder F [-use ATTR] [-dry-run]
epistula-database admin folder-merge            -mailbox M -from F [-subtree] -to T [-drop-duplicates] (-dry-run | -yes)
epistula-database admin folder-prune-empty      -mailbox M (-dry-run | -yes)
epistula-database admin archive-category-import -mailbox M -file F [-dry-run]
epistula-database admin archive-category-list   -mailbox M [-all]
epistula-database admin archive-category-suggest -mailbox M [-min N] [-share P] [-examples K] [-min-confidence C] [-format text|toml]
epistula-database admin archive-reclassify      -mailbox M -key K [-refile] (-dry-run | -yes)
epistula-database admin archive-plan            -mailbox M [-rules F] [-moves-out F]
epistula-database admin archive-apply           -mailbox M [-rules F] [-batch-size N] [-limit N] [-pause D] -yes
epistula-database admin archive-undo            -mailbox M -batch B (-dry-run | -yes)
epistula-database admin archive-batches         -mailbox M
```

The `archive-*` commands, `folder-create`, `folder-merge` and `folder-prune-empty` are archive sorting's
operator surface (`../ARCHIVE_SORTING.md`, migration 020). The logic they share
with epistula-imap's live sorter and Trash purge is in the `archive` package
(plan, apply, undo, `SortOnce`, `PurgeOnce`) and `storage/archive.go`:
`MoveMessages` (the in-place server-side move: id kept, fresh UIDs, canonical
lock order, `pg_notify` on both folders), `PurgeMessages` (rows, optionally
every copy of the same content; files are left to `gc`), the category list,
and `PruneEmptyFolders`. `messages.created_at` is the time a row entered its
current folder, and both the sorter's settle delay and the Trash retention
are measured from it.

All admin subcommands accept `-config <path>` and operate transactionally. Destructive operations require an explicit `-yes` flag.

## What this project does NOT do

- **Does not run IMAP.** That's `imap`. Keeping them separate means an IMAP outage doesn't stop deliveries, and a deploy of either side is independent.
- **Does not run SMTP.** Postfix still handles inbound SMTP, queueing, TLS, and spam filtering. We're a `pipe` LDA, nothing more.
- **Does not handle outbound mail.** Sent mail lands here only if the user's MUA `APPEND`s to a `Sent` folder via the IMAP server.
- **Does not filter live deliveries with reject rules.** Spam filtering belongs upstream of the LDA; this component stores what Postfix hands it. The optional `admin reject-export` command converts CSV rules to Postfix policy files.

## Production hardening & security

These are non-negotiable defaults. The daemon refuses to start if any of them are misconfigured.

### Postgres connection

- `sslmode=verify-full` required when `production=true` in config. The daemon refuses to start if the DSN contains `sslmode=disable` or `sslmode=allow`. `verify-ca` is permitted only for development.
- All queries use parameterized statements (`$1`, `$2`, ...). No `fmt.Sprintf` into SQL — not even for identifiers. Folder name lookups go through `quote_ident`-equivalent escaping.
- A `statement_timeout` of 30s is set per session via `SET LOCAL statement_timeout` at transaction start in the LDA path; the IMAP server side sets its own per-command.

### Filesystem & blob storage

- `storage_root` permissions are checked at startup: refuses to start if mode is `o+r`, `o+w`, or `o+x`. Required: dir `0750`, files `0640`, owner = `epistula-database` user, group = shared with IMAP server.
- Blob path construction is **hash-only**. The hex sha256 is validated against `^[0-9a-f]{64}$` before being used in a path. Attachment `filename` fields are stored as metadata in PG only — never used to construct a path on disk. This eliminates path-traversal as a class.
- Blob writes use the `write-to-tmp + fsync + link + unlink(tmp) + fsync(parent dir)` pattern. `link(2)` is atomic and fails with `EEXIST` when the content-addressed path already exists, so concurrent deliveries of the same blob are race-safe: the loser verifies the existing file's size and digest (rewriting it from its own bytes if it is corrupt) and `unlink`s its tmp file.
- Durability ordering is strict: blob bytes on disk + fsync before the PG row commit. A power loss between blob-write and DB-commit leaves an orphan blob, GC'd later — never a dangling row.

### MIME parsing limits (zip-bomb / billion-laughs defense)

| Limit | Default | Reason |
|-------|---------|--------|
| Max raw message size | 50 MiB | Postfix message_size_limit is the real ceiling; this is the LDA's own cap. |
| Max MIME recursion depth | 10 | RFC 2049 doesn't bound nesting; real mail never nests past 4–5. |
| Max parts per message | 200 | Pathological multipart explosions. |
| Max single header size | 16 KiB | Defends against header smuggling. |
| Max total header section | 256 KiB | Same. |
| Max transfer-decode expansion ratio | 10× | If `base64`/`qp` decode produces > 10× the encoded size, treat as malformed and reject. |
| Per-delivery wall clock | 60 s | The LDA process kills itself if exceeded; Postfix retries. |

All limits configurable. Exceeding any of them: log at WARN with envelope info (never the body), exit `EX_DATAERR` (65). Postfix will bounce per its bounce-message policy. IMAP APPEND refuses with `NO` under the same limits.

**Malformed is not the same as over a limit (OPS-003).** The raw blob is the source of truth and is stored verbatim, so a malformation costs only derived data, and the parser reads such mail the way mail clients do rather than refusing it, in every mode:

- A multipart body with no close delimiter ends its last part at EOF. Parts are split by `ingest.VisitRawMultipart`, the same function the IMAP reader uses for `BODY[n]`.
- A header block with no empty line after it is a message (or part) with no body (RFC 5322 §3.5). The header ends at the first empty line, whether `\n` or `\r\n`, which is net/mail's rule: `ingest.HeaderSeparatorEnd`, shared with the IMAP reader's streamed header read.
- Quoted-printable is decoded without a line-length limit (the stdlib reader failed any line over 4096 bytes with `bufio: buffer full`). Malformed escapes and control bytes pass through literally.
- A body its transfer encoding cannot decode (truncated or corrupt base64) keeps its place and encoded size in BODYSTRUCTURE. What did decode feeds the text projection. No attachment row or enclosed-message structure is derived from a partial decode.
- A MIME part whose own header does not parse is kept as opaque bytes.

Each such problem is recorded as a defect on its BODYSTRUCTURE node (`"defects": ["missing_close_delimiter"]`, kinds in `ingest/ingest.go`). `delivery_log` gets outcome `<verb>:degraded` with the defects in `error_detail`. Find degraded messages with `SELECT id FROM messages WHERE bodystructure @? '$.**.defects'`. No schema change.

Two things are still refused everywhere. One is a message whose own header net/mail rejects, because IMAP builds ENVELOPE with net/mail from the same bytes, and storing it would fail FETCH for its folder. The other is a message over `max_message_bytes`. The resource limits above still refuse for `deliver` and APPEND. `import`, `import-blobs` and `reparse-bodystructure` use `ingest.NewSalvaging` instead: a limit stops the parser where it is reached (an entity past the depth limit is kept opaque, the parts after the parts limit are not listed, over-limit header fields are left out of the parsed headers) and is recorded as a defect. No limit is raised. A Content-Type or Content-Transfer-Encoding field over a header limit cannot be left out, since the IMAP reader traverses by it. On the message itself that still refuses; on a part it makes the part opaque.

### Address parsing

- Headers parsed with `mime.WordDecoder` for RFC 2047 encoded-words; reject malformed UTF-8 with `EX_DATAERR`.
- Recipient lookup against `aliases` uses normalized localpart (lowercase, NFC-normalized) — `User@EXAMPLE.INVALID` and `user@example.invalid` resolve to the same row. The original case is preserved in `delivery_log.envelope_to` for audit.

### LDA exit code policy (`sysexits.h`)

| Condition | Exit | Postfix behavior |
|-----------|------|------------------|
| Delivered, committed | `EX_OK` (0) | Done. |
| PG unreachable, disk full, transient | `EX_TEMPFAIL` (75) | Requeue, retry per backoff schedule. |
| Own header unparseable, exceeds a MIME limit | `EX_DATAERR` (65) | Bounce to sender. A malformed part or body is delivered as degraded instead (OPS-003). |
| Domain not configured at all | `EX_NOHOST` (68) | Bounce ("host unknown" semantics — the domain itself is foreign). |
| Recipient resolves to no mailbox (known domain) | `EX_NOUSER` (67) | Bounce. |
| Mailbox over quota (`mailboxes.quota_bytes` set, non-NULL) | `EX_CANTCREAT` (73) | Bounce. Enforced in the Ingest tx: the message that pushes `used_bytes` past `quota_bytes` is rejected and rolled back. Import paths bypass. |
| Internal bug, unexpected panic | `EX_SOFTWARE` (70) | Bounce. Should never fire in steady state. |

**Critical rule: never `EX_OK` without a committed PG transaction.** A panic mid-delivery returns `EX_TEMPFAIL` via `recover()` in main. Postfix retries are the safety net; silent dataloss is the failure mode we engineer against.

### `serve` listener (metrics/health only)

- Binds `127.0.0.1` by default. Never expose beyond loopback — there is no auth on this listener because there is no privileged surface to protect (no admin CRUD, no log access, no message access).
- Endpoints: `/metrics` (Prometheus), `/health`, `/healthz`. Anything else returns 404.
- Read-only as far as application state; the only effect of a request is incrementing Prometheus counters.

### Admin CLI

- Mailbox password hashing uses Argon2id encoded PHC format. Password input via `-password-stdin` (or interactive prompt with terminal echo disabled via `golang.org/x/term`) — never accepted on argv (would leak via `ps`).
- Destructive operations (`*-delete`) require `-yes` and print the affected row counts before committing.
- Operates against the same Postgres DSN as `deliver` / `serve`. No separate admin connection pool — short-lived, one tx per command.

### Logging

- Never log raw message bodies or attachment content. Ever.
- Log envelope info, `Message-ID`, raw sha256 truncated to 16 hex chars, size, recipient mailbox name, outcome.
- Redact `Authorization`, `Cookie`, `Set-Cookie`, and any header matching `(?i)(password|token|secret|key)` from any structured log output that includes headers.
- Use `log/slog` with `Source: false` in production (no file:line leakage to log aggregation).

### Blob garbage collection

GC is two-phase, with three independent protections against race-deleting a blob that a concurrent `deliver`/`import`/IMAP `APPEND` references:

1. **Mark pass**: enumerate every blob file across all tenants. Files whose mtime is younger than the grace period (default 24h) are skipped outright — a fresh blob may belong to an uncommitted delivery. For each older file, the reference check is **scoped to the owning mailbox**: `(sha256, bucket date)` against `messages`/`attachments` joined to `folders.mailbox_id = <tenant's mailbox>`. This scoping is essential under per-tenant isolation — without it, alice's orphan would be falsely "referenced" because bob holds identical content+date in his own file, and alice's orphan would leak forever. A tenant directory whose name no longer resolves to a mailbox (deleted box, or a stray subtree) has all its blobs treated as unreferenced and reapable. Unreferenced blobs are upserted into `gc_candidates` (keyed by `(tenant, sha256, kind, bucket)`); a re-mark updates `generation_marked` (the mark run's epoch) while preserving `first_seen_at`.
2. **Sweep pass** (separate invocation): a candidate is eligible only when it is older than the grace cutoff AND `generation_marked > epoch(first_seen_at)` — i.e. a mark pass *later* than its first sighting re-confirmed it (the generation gate). Each eligible candidate is processed in its own transaction that takes the blob's advisory lock (`storage.BlobAdvisoryLockKey(tenant, kind, bucket, sha)` — the tenant is part of the key so the same content in two mailboxes serializes independently), re-checks the reference with the same mailbox-scoped + unresolvable-tenant logic as mark, unlinks before commit, and clears the candidate row. Referenced candidates are resurrected (row dropped, file kept). After a delete it best-effort `rmdir`s now-empty shard/date dirs up to the tenant's per-kind subtree root (ignoring `ENOTEMPTY`/`ENOENT`, so a concurrent first-delivery `MkdirAll` is never corrupted).
3. **Ingest-side protocol**: every ingest transaction takes the same per-blob advisory locks, deletes any `gc_candidates` rows for its blobs (matched on tenant too), and runs `IngestParams.EnsureBlobs` — re-verifying each file survived any sweep that ran before the lock was held and rewriting it from memory if not. Whichever side wins the lock, the end state is consistent: sweep either sees the committed row (and resurrects) or ingest sees the unlinked file (and rewrites it). The tenant string is the canonical `mailboxes.name` on both sides, so the lock keys and candidate rows always agree byte-for-byte.

Conservative by design — orphans linger at least a grace period plus one mark interval before being reaped. Covered by `gc_integration_test.go`, including a live test that sweep blocks on a held ingest lock and a cross-tenant test proving one mailbox's delete reaps only that mailbox's file.

IMAP `COPY`/`MOVE` (`epistula-imap`) joins the same protocol before it inserts rows that point at existing blobs (R-061): it takes each source blob's advisory lock in the same sorted order as ingest, deletes any `gc_candidates` rows for those blobs, and verifies each file's size and digest. The bytes are not in hand to rewrite as `EnsureBlobs` does, so a missing or corrupt source blob fails the command with `NO [SERVERBUG]` instead of committing a dangling row.

### Configuration safety

- The daemon refuses to start if `production=true` and any of: PG `sslmode` is not `verify-full`/`verify-ca`, storage_root is world-accessible, Argon2 memory below the production floor. (There is no admin password or TLS surface in this daemon: operator management is the local `admin` CLI, and in production `serve` must bind to loopback.)
- No `SIGHUP` config reload: the LDA is a short-lived process that reads its config fresh on every delivery, and the `serve` listener is metrics/health only — a restart is instant and drops nothing. Config changes take effect on the next delivery automatically.

---

## Resolved design notes

- **LISTEN/NOTIFY on a `mail_arrived` channel** is emitted from the LDA's commit transaction (`pg_notify('mail_arrived', folder_id::text)`). The IMAP server's IDLE handler subscribes. Cheap, idiomatic, no polling.
- **The annotation pipeline's work is a queue, not an absence** (migration 022). `storage.Ingest` inserts an `annotation_pass_required` row in the transaction that stores the message (so deliver, import, import-blobs and IMAP APPEND all mark it; epistula-imap's COPY and MOVE carry the marker), and epistula-api's `pass_required=true` reads the queue by primary key. An absence query would instead probe every message. epistula-api's prune clears the markers of finished messages; the worker's periodic complete round still sweeps for what nothing queues (a new model, a retired category, pre-022 mail). The PostgreSQL marker commits or rolls back with the message, the worker reaches it through epistula-api, and there is no second store to back up.
- **Encryption at rest** is delegated to the storage layer (ZFS native encryption on ZFS hosts, dm-crypt on Linux hosts). Application-layer per-blob encryption added complexity without meaningful threat-model gain; revisit if it's ever wanted. Because blobs are now partitioned per mailbox (`${storage_root}/<mailbox>/…`), a per-user ZFS dataset gives each mailbox its own snapshot schedule, quota, and **encryption key** at the filesystem layer without any application change — the isolation the per-tenant layout was built for.
- **No `raw_blobs(sha256, refcount)` refcount table.** GC uses `SELECT EXISTS` against `messages.raw_sha256` / `attachments.sha256` joined to `folders.mailbox_id` (the `idx_messages_sha_date_folder` covering index supports the mark-pass walk). Refcount tables are race-prone under concurrent INSERT/DELETE; the two-phase generational mark-and-sweep above replaces them.
