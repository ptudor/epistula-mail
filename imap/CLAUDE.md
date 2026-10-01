# Epistula — imap

A read-mostly IMAP4 server that serves the Postgres + content-addressed blob store written by `database`. The two projects share a schema (defined and owned by epistula-database) and a parsing/ingest code path (vendored from epistula-database for the one write operation IMAP supports: `APPEND`).

## Design goal

**Stateless IMAP**: every IMAP command is a Postgres transaction; no per-connection in-memory state beyond the currently SELECTed mailbox cursor and the auth context. The server can crash, restart, or scale horizontally without coordination. Postgres is the only source of truth for IMAP state; the disk blobs are immutable and content-addressed.

The IMAP server has **exactly one write path into the store**: a client `APPEND` (typically a MUA dropping into a `Sent` folder). That path calls into the shared `ingest` package from epistula-database — the same code Postfix-side delivery uses — so there is one canonical parser, one canonical blob layout, one canonical schema mutation. No second implementation to drift.

## Common patterns (inherited from parent)

| Pattern | Implementation |
|---------|----------------|
| **Transport** | Implicit TLS on port 993. No port 143. No STARTTLS. (See security posture below.) |
| **Logging** | Structured logging via `log/slog` (JSON or text) |
| **Metrics** | Prometheus + expvar endpoints on a separate admin listener |
| **Graceful Shutdown** | SIGINT/SIGTERM: stop accepting, drain open sessions (with timeout), close PG pool. |
| **Configuration** | TOML config file (preferred) or environment variables |
| **Health Checks** | `/health` and `/healthz` on the admin listener (not exposed on the IMAP port) |
| **Database** | PostgreSQL, `sslmode=verify-full` in production |

### Dependencies

- `github.com/emersion/go-imap/v2` — IMAP protocol library. Battle-tested, used by multiple production mail systems, supports IMAP4rev1 (RFC 3501) and IMAP4rev2 (RFC 9051). Writing an IMAP server from scratch is a multi-quarter project and a security minefield; this is the right library to take.
- `github.com/jackc/pgx/v5` — Postgres driver (the same driver the other Epistula components use).
- `github.com/prometheus/client_golang` — metrics.
- `github.com/pelletier/go-toml/v2` — config.
- `golang.org/x/crypto/argon2` — password verification.
- `crypto/subtle` (stdlib) — constant-time comparisons.

The shared `ingest`/`blob`/`storage`/`auth` packages live in epistula-database and are imported via a `replace` directive in `go.mod` pointing at `../database`. Both projects live in the same monorepo and move in lockstep, so the sibling checkout is always present and version skew between them is impossible by construction. The trade-off is explicit: this repo does NOT build standalone — a release build needs the monorepo (or a future switch to tagged epistula-database releases if the projects ever split repos).

Go toolchain: use [the repository pin](../.go-version), including when building this component alone.

---

## Architecture

```
IMAP client (Mail.app, Thunderbird, mutt, Aerc, K-9, ...)
   │
   │ implicit TLS, TCP/993
   ▼
epistula-imap serve
   │
   ├─► Postgres  (folders, messages, mailboxes, headers JSONB, text_body, html_body, flags)
   │     │
   │     └─► LISTEN mail_arrived  →  wakes IDLE clients
   │
   ├─► Disk blobs (read-only)
   │     ├─► <mailbox>/raw/<yyyy/mm/dd>/<aa>/<bb>/<sha>.eml   →  every FETCH body section, ENVELOPE
   │     └─► <mailbox>/att/<yyyy/mm/dd>/<aa>/<bb>/<sha>.bin   →  written by APPEND; FETCH never reads it
   │
   └─► ingest pkg (write — APPEND only)
         │
         └─► Postgres tx + blob write (identical to epistula-database deliver)
```

Two listeners:
- **IMAP listener** on `:993` (or as configured), implicit TLS, public-facing.
- **Admin listener** on `127.0.0.1:8783` (or as configured), plaintext HTTP, for `/metrics`, `/health`, `/healthz`. Never exposed beyond loopback. **No admin/management UI** lives here — operator tasks (mailbox/domain/alias CRUD, password resets, log inspection) are owned by `database`'s `admin` CLI; a web operator dashboard is not included. The IMAP daemon is a pure read/append-mostly protocol server.

## Security posture (production defaults, all non-negotiable)

These are the defaults the daemon refuses to weaken without an explicit `production=false` config flag.

### Transport

- **Implicit TLS on port 993 only.** No port 143. No `STARTTLS`. STARTTLS has a well-documented history of plaintext command-injection attacks: the class was first publicized as CVE-2011-0411 in SMTP, and the same flaw later turned up in IMAP and POP3 servers. The cleanest defense is to never speak cleartext on the wire. If a legacy client cannot do implicit TLS, run `stunnel` in front of cleartext IMAP — don't enable STARTTLS in this daemon.
- **TLS 1.3 minimum.** TLS 1.2 must be explicitly enabled in config (`min_tls_version = "1.2"`). 1.0/1.1 are refused regardless.
- **Cipher suites** for TLS 1.2 (when enabled) limited to the AEAD set: `ECDHE-ECDSA-AES128-GCM-SHA256`, `ECDHE-RSA-AES128-GCM-SHA256`, `ECDHE-ECDSA-AES256-GCM-SHA384`, `ECDHE-RSA-AES256-GCM-SHA384`, `ECDHE-ECDSA-CHACHA20-POLY1305`, `ECDHE-RSA-CHACHA20-POLY1305`. TLS 1.3 uses Go's default suite list (all AEAD).
- **Certificate reload on SIGHUP** without dropping connections. Required for Let's Encrypt renewals. New connections pick up the new cert; existing connections finish on the old.
- **HSTS-equivalent** is N/A for IMAP, but the published service record (SRV / MTA-STS-equivalent if you publish one) should point only at port 993.

### Authentication

- **AUTH=PLAIN over TLS only.** The server refuses `AUTH=PLAIN` if the connection has not negotiated TLS. (Implicit TLS makes this trivially true, but the check is enforced defensively.)
- **No CRAM-MD5, no APOP, no DIGEST-MD5.** Challenge-response mechanisms require storing the password (or a plaintext-equivalent) on the server. Argon2id verification of plaintext-over-TLS is strictly more secure.
- **No SASL EXTERNAL.** Client-certificate authentication is not implemented; `AUTH_CLIENT_CERTS.md` is the design for adding it as an opt-in.
- **No `LOGIN` capability advertised pre-TLS.** Combined with implicit TLS, this is belt-and-braces.
- Verification: fetch `password_hash` from `mailboxes`, run `argon2.IDKey` with the encoded parameters, compare with `crypto/subtle.ConstantTimeCompare`. A `mailboxes.disabled_at IS NOT NULL` row fails auth regardless of password match (so disabling a mailbox is immediate without touching the hash).

### Rate limits & connection caps

- **TLS handshake deadline**: each accepted connection must complete its handshake within `tls_handshake_timeout` (default 10s) or it is dropped — handshakes run eagerly under a deadline before a connection ever reaches the IMAP server, so a connect-and-say-nothing peer cannot hold a slot.
- **Pre-auth timeout**: a handshaken connection must authenticate within `preauth_timeout` (default 60s); the read deadline is lifted on successful LOGIN.
- **Per-IP concurrent connection limit**: 10 (configurable). Excess connections receive `* BYE Too many connections` and are dropped. The cap is enforced at raw-TCP accept, before any TLS work, so a capped IP costs no handshake CPU (its BYE goes out in cleartext).
- **Per-mailbox concurrent session limit**: 5 (laptop + phone + watch + spare + headroom).
- **Login throttling**: per-IP token bucket, 5 failed AUTH attempts per 60s. On lockout, the connection accepts the `LOGIN` command but always returns `NO [AUTHENTICATIONFAILED]` for the duration, with a configurable extra delay (default 2s) before responding to slow down credential-spray attacks.
- **Command-rate cap**: 100 commands/sec per connection, intended to detect a misbehaving or compromised client rather than a real attack.
- **Idle connection timeout**: 30 minutes (RFC 9051 §5.4 allows 30 min minimum for IDLE; we honor it).

### Resource bounds

- **Max literal size on APPEND**: same cap as the LDA (50 MiB default). Larger APPENDs receive `NO [TOOBIG]` per RFC 7889.
- **Max in-flight FETCH BODY[] streams per connection**: 1. (Pipelined FETCH is allowed, but body-stream serialization keeps memory bounded.)
- **Per-session memory soft cap**: ~10 MiB. Enforced via bounded buffers, not heap accounting.

### Logging & audit

- Never log message bodies, attachment contents, or password material.
- Log: connection open/close (with remote IP), AUTH result (success/failure, mailbox name, never password), SELECT (mailbox name), APPEND (size, sha256:16), errors with command name + tag (never command arguments that might contain literals).
- Every refused LOGIN logs `login failed` with a `reason` and the client IP, never the password. Against an existing mailbox it is WARN with `mailbox=` (`wrong password`, `mailbox disabled`); for a username that names no mailbox, or a LOGIN missing its username or password, it is INFO with the client's `username=`, cut to 64 bytes (to avoid creating a username-enumeration oracle in the log, though the difference is minor with rate limiting in place). The client always gets the same AUTHENTICATIONFAILED. A throttled LOGIN logs `login throttled` instead.

---

## Configuration

TOML with the same precedence as the other Epistula daemons:

```bash
epistula-imap serve -config /usr/local/etc/epistula/epistula-imap.toml
```

Default search paths:
```
/usr/local/etc/epistula/epistula-imap.toml
/etc/epistula/epistula-imap.toml
./epistula-imap.toml
```

Required config keys:
- `[server]` — listen address (default `:993`), admin listen address (default `127.0.0.1:8783`), TLS cert/key paths.
- `[postgres]` — DSN (must include `sslmode=verify-full` if `production=true`).
- `[storage]` — `root` path (matches epistula-database's `storage_root`; the IMAP server must be in the group that can read its blob files).
- `[limits]` — connection caps, rate limits, timeouts.
- `[production]` — boolean; gates the strict-mode startup checks.

---

## Capabilities advertised

The set is built in `serve.go:advertisedCaps()`. Several entries below are
emitted by the go-imap server library itself rather than named there —
`caps_ro5x007_test.go` asserts the real CAPABILITY line on the wire, so this
table and the running server cannot drift silently. Note the extension set
appears **post-authentication**; a pre-auth `CAPABILITY` legitimately shows
only the login-relevant subset (`IMAP4rev2 IMAP4rev1 SASL-IR LITERAL-
AUTH=PLAIN`).

| Capability | RFC | Notes |
|------------|-----|-------|
| `IMAP4rev1` | 3501 | Required for legacy client compatibility. |
| `IMAP4rev2` | 9051 | Current standard. |
| `AUTH=PLAIN` | 4616 | Only mechanism advertised. Refused pre-TLS. |
| `IDLE` | 2177 | Wakes via PG LISTEN/NOTIFY on `mail_arrived`. |
| `LIST-EXTENDED` | 5258 | |
| `SPECIAL-USE` | 6154 | `\Sent`, `\Drafts`, `\Trash`, `\Junk` annotations, from `folders.special_use` — set by `CREATE … (USE (…))` or, for delivered/imported folders, by `epistula-database admin folder-set-special-use`. Both accept the same set (`storage.CanonicalSpecialUse`). |
| `MOVE` | 6851 | Atomic move between folders. |
| `UIDPLUS` | 4315 | APPENDUID / COPYUID responses. |
| `ESEARCH` | 4731 | Reduced-result SEARCH. |
| `ENABLE` | 5161 | Client-side capability opt-in. |
| `UTF8=ACCEPT` | 6855 | Emitted by the server library. |
| `LITERAL-` | 7888 | Non-synchronizing literals from the server library. |
| `CREATE-SPECIAL-USE` | 6154 | **Not offered.** `CREATE … (USE (\Archive))` *is* parsed by the library regardless and is validated + persisted to `folders.special_use` rather than silently dropped, but the capability itself stays unadvertised. |
| `COMPRESS=DEFLATE` | 4978 | **Not offered.** CRIME-class compression-side-channel concerns when combined with TLS; no implementation ships. |
| `CONDSTORE` / `QRESYNC` | 7162 | Schema and mutation/search groundwork exists, but wire advertisement is deferred until the server library supports the full RFC 7162 command surface. |

---

## FETCH performance model

The IMAP protocol handler must not block on disk I/O while holding the session-level mutex. Routing:

| FETCH item | Source | Latency |
|------------|--------|---------|
| `INTERNALDATE`, `RFC822.SIZE`, `FLAGS`, `UID` | PG only (one row, indexed) | sub-ms |
| `BODYSTRUCTURE` | PG (`bodystructure` JSONB, precomputed at ingest) | sub-ms |
| `ENVELOPE`, `BODY[HEADER]`, `BODY[HEADER.FIELDS (...)]` | Disk: the raw blob's header section, read only up to the empty line that ends it (R-042). ENVELOPE parses it with net/mail. | ~disk-bound |
| `BODY[]` (full raw RFC 5322) | Disk: `<mailbox>/raw/<yyyy/mm/dd>/<aa>/<bb>/<sha>.eml`, streamed | ~disk-bound |
| `BODY[TEXT]`, `BODY[N]`, `BODY[N.MIME]`, `BODY[N.HEADER]`, `BODY[N.TEXT]` | Disk: the raw blob, read whole and sliced by `resolveRawPart`. A part is returned as the sender sent it, in its transfer encoding. | ~disk-bound |

Only `BODY[]` streams: it is copied from the open blob, so the message is never held in memory. Every other body section reads the whole raw message into memory, bounded by the ingest size limit (default 50 MiB), and slices the section out of it. The read and the MIME walk happen once per message per FETCH command (RA6X-054), so `BODY[1] BODY[2] BODY[3]` costs one read, but a session fetching an attachment by part number holds a message-sized buffer while it does. FETCH never reads the extracted attachment blobs under `att/`.

PG queries use a single read-only transaction per IMAP command with `SET LOCAL statement_timeout = '10s'`. Connection pool is sized for `2 × max_concurrent_imap_sessions`, with idle connection lifetime capped (default 5 min) to play well with PG bouncers.

---

## State changes (the small write surface)

| IMAP command | Effect | Concurrency notes |
|--------------|--------|-------------------|
| `STORE` (flags) | UPDATE `messages.flags` WHERE folder + UID. | Per-row, no cross-row locks. |
| `EXPUNGE` | DELETE `messages` WHERE folder + `\Deleted` in flags. Updates `mailboxes.used_bytes -= raw_size` in same tx. With `[archive] delete_archives`, the last copy of a message is archived instead (see below). | Single tx. |
| `COPY` | INSERT `messages` rows with same `raw_sha256` (blob deduped) into target folder; allocate UIDs. Annotations and the archive classification are copied to the new rows, so an archived message keeps what it was classified as. **Quota-enforced**: `used_bytes` is incremented once for the whole copy and compared to `quota_bytes`; a copy that would exceed it rolls back entirely and returns `NO [OVERQUOTA]`. | UID allocation under `FOR UPDATE` on `folders` row. |
| `MOVE` | Atomic COPY + EXPUNGE in one tx. **Quota-neutral**: the source decrement and the destination increment are applied in one statement, so a same-mailbox move never transiently double-counts, and a move is never refused for quota (it frees as much as it adds — including on a mailbox an operator has already pushed over its quota). | Per RFC 6851. |
| `APPEND` | Full ingest pipeline (parse → blob → INSERT). Calls shared `ingest` pkg. | Same code as LDA delivery. |
| `CREATE`, `DELETE`, `RENAME` (folder) | Mutates `folders`. RENAME bumps `uidvalidity`. CREATE and RENAME create missing ancestors of the new name through epistula-database's `storage.EnsureFolder`. | Single tx, mailbox row locked first. |
| `SUBSCRIBE` / `UNSUBSCRIBE` | Mutates a `folder_subscriptions(mailbox_id, folder_id)` table (small addition to epistula-database schema). | |

### Archive sorting jobs (`[archive]`, off by default)

Two background jobs run in `serve` on `archive.interval` (`../ARCHIVE_SORTING.md`):

- **Live sorter** (`sort_enabled`): a message in a mailbox's `\Archive` folder that has been there `settle_delay` (so a client's Undo still finds it), is classified into an active archive category at `min_confidence` or above, and is not `\Deleted` moves to that category's folder through `storage.MoveMessages`: an in-place move that keeps the message id and gives it fresh UIDs, announced on `mail_arrived`, and journaled in `archive_moves`. It files only into folders strictly inside `\Archive` that carry no special-use role.
- **Trash purge** (`purge_enabled`): messages in `\Trash` longer than `trash_retention` are destroyed with `storage.PurgeMessages`, together with every copy of the same content in the mailbox when `purge_all_copies` is set. The blob files leave disk at the next `epistula-database gc` mark/sweep.

- **Delete archives** (`delete_archives`, Gmail's semantics, `imapsess/delete_archives.go`): EXPUNGE outside `\Trash`, `\Drafts` and `\Junk` does not destroy the **last copy** of a message. Outside the archive it moves to `\Archive` (in place, id kept, `\Deleted` cleared, reported to the client as EXPUNGE). Inside the archive it stays, with the mark cleared. A message with another copy elsewhere, which is a copy-then-delete move, is removed as before, and a concurrent un-delete still rescues (RO5X-001). The loop also archives a message left marked `\Deleted` in INBOX once the mark has stood for `settle_delay` (`archive.DeletedTracker`). Mailboxes without `\Archive` keep plain IMAP behaviour. This is what lets Mac Mail's Delete key archive once "Move deleted messages to the Trash mailbox" is off.

All three follow the canonical lock order, skip mailboxes in maintenance, and export
`imap_database_archive_*`, `imap_database_trash_purged_*` and `imap_database_deleted_archived_total` metrics.

UID allocation is the only operation that needs serialization: `UPDATE folders SET uidnext = uidnext + 1 WHERE id = $1 RETURNING uidnext - 1` is atomic and contended only within a single folder. No cross-folder ordering issues.

---

## IDLE implementation

`IDLE` is the one place the server holds a connection open without serving FETCHes. Implementation:

1. Client issues `IDLE`. Server responds `+ idling`.
2. Server's session handler subscribes to `pg_notify` on the `mail_arrived` channel via a dedicated PG connection (one per IDLE session, returned to a small dedicated pool — *not* the main read pool).
3. On NOTIFY where `payload == folder_id` of the selected mailbox, server emits `* <n> EXISTS` and `* <n> RECENT`.
4. Server tears down IDLE on client `DONE` or on the 29-minute heartbeat (one minute under the RFC 9051 minimum).

PG connection budget: `idle_conn_pool_size` (under `[postgres]`) sizes the dedicated LISTEN pool — one connection per IDLE session at worst; 0 shares the main query pool.

---

## What this project does NOT do

- **No SMTP.** Postfix is your MTA. We never accept inbound mail.
- **No outbound delivery.** A client sending mail uses its configured SMTP submission server; we only store what it copies to Sent via APPEND.
- **No spam filtering, sieve, server-side rules.** That's upstream of Postfix's pipe to epistula-database.
- **No POP3.** If you need POP3 against the same backend, run a separate process — the schema is documented enough to point dovecot at, though that's not a supported path.
- **No CalDAV, CardDAV, JMAP.** This is IMAP only.
- **No direct blob writes outside ingest.** The one write path is IMAP `APPEND`, which goes through the shared `ingest`/`blob` packages and writes a raw (and any attachment) blob into the shared store exactly as LDA delivery does — so the daemon needs group write access to the blob tree, and a shared-group deployment sets `[storage] group_writable = true` (see R-007). Outside `APPEND` the daemon only reads blobs; it never constructs blob writes of its own.
- **No schema migrations.** epistula-database owns the schema and ships migrations. This project bumps its dependency on epistula-database to pick up schema changes.
- **No admin web UI, no admin REST API, no admin CLI.** Operator management is epistula-database's responsibility, through its `admin` Go CLI; a web operator dashboard is not included. The IMAP daemon authenticates IMAP clients against `mailboxes.password_hash` and does nothing else administratively.

---

## Subcommands

| Subcommand | Purpose |
|------------|---------|
| `serve` | Run the IMAP server (and admin HTTP listener). |
| `version` | Print version and exit. |
| `check-config` | Validate the TOML config against the current `production` ruleset and exit. Useful for systemd `ExecStartPre`. |

No `import`, no `gc`, no `deliver` — those are epistula-database's responsibilities.

---

## Failure modes & operational notes

- **PG down at connection time**: refuse new IMAP connections with `* BYE Backend unavailable`. Existing sessions whose next query fails get `* BYE` and disconnect. The daemon does NOT exit — it keeps the listener up and retries PG in the background.
- **PG down mid-FETCH**: send `<tag> NO [SERVERBUG] backend temporarily unavailable`, close the session (per RFC, the client will reconnect).
- **Disk blob missing for `FETCH BODY[]`**: send `<tag> NO [SERVERBUG] message body unavailable`, log at ERROR with sha256, mailbox, UID. This indicates store corruption — operator-paging condition.
- **TLS cert expired**: fail closed. The daemon refuses new connections. Existing connections continue until they disconnect naturally. Reload via SIGHUP after renewing.
- **Mailbox over quota during APPEND**: respond `NO [OVERQUOTA]` per RFC 9208.
- **APPEND to a mailbox that does not exist**: respond `NO [TRYCREATE]` per RFC
  3501 §6.3.11 / RFC 9051 §6.3.12 — the folder is *not* auto-created, so a
  client typo cannot leave a permanent folder in everyone's LIST. (COPY/MOVE
  still auto-create their destination; that is deliberate and matches
  drag-to-new-folder behaviour.) `INBOX` — and only `INBOX` — is matched
  case-insensitively per RFC 3501 §5.1. Folder names are capped at 255 bytes
  and may not contain control characters, in `CREATE` and `APPEND` alike.

---

## Future considerations (deferred)

- Full `CONDSTORE` / `QRESYNC` wire support for fast resync on weak network clients. The schema groundwork (`folders.highest_modseq`, `messages.mod_seq`) is already in epistula-database; complete advertisement waits on server-library support for `FETCH MODSEQ`, `STATUS HIGHESTMODSEQ`, `UNCHANGEDSINCE`, and vanished UID tracking.
- `METADATA` (RFC 5464) for server-side per-mailbox annotations.
- Sieve via ManageSieve (RFC 5804). Out of scope; spam/rules belong upstream of Postfix.
- ACL extension (RFC 4314). N/A in single-tenant deployment.
