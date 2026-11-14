# Epistula changelog

## 1.0.1 — 2026-11-13

- Build all five services with Go 1.27.4, which includes the November 2026
  toolchain and HTTP security fixes. Update pgx to 5.12.0, its connection
  pool, Prometheus support modules, the Go text/system libraries, and MCP's
  indirect dependencies. Pin `golang.org/x/crypto` to the upstream revision
  that removes the obsolete OpenPGP packages flagged by GO-2026-5932;
  Epistula did not import those packages. Release builds now use GoReleaser
  2.20.0, including its publishing-key metadata security fix.
- Release automation includes the checked-in release notes and verifies that
  the draft contains them before publication.
- `admin folder-resync` gives a repaired folder a new UIDVALIDITY so IMAP
  clients discard stale cached state and read it again. It supports a dry run
  and requires `-yes` to apply; sessions with that folder selected close so
  their clients reconnect.
- IMAP FETCH continues answering readable messages after an unreadable blob
  in the requested range, then reports the failed command. Blob directory
  creation also preserves reader access if a writer exits before completing
  its permission setup.
- The `messages` table no longer stores two projections nothing read, and its
  indexes follow the readers. epistula-database migrations 027–030: the
  full-text vector is the expression of the new IMMUTABLE `mail_fts(subject,
  text_body)` function and lives only in the `idx_messages_search` GIN index
  (027); the UNIQUE `(folder_id, uid)` constraint INCLUDEs `mod_seq` so the
  IMAP session view reads index-only, `idx_messages_unseen_folder` serves
  STATUS UNSEEN, and the never-scanned headers GIN and
  `idx_attachments_sha256` are dropped (028); `annotation_pass_required`
  carries each marker's `mailbox_id`, indexed (029); and the stored `fts`
  and `html_body` columns are dropped (030). On a 630K-message store the two
  columns and the headers GIN held 6.2 GB of the 13 GB database. 027–029
  build their indexes `CONCURRENTLY` through the migration runner's new
  `-- migrate: no-transaction` mode, and `migrate up -to N` lets the
  rollout stay online: `migrate up -to 29`, deploy, `migrate up`.
  `DROP COLUMN` returns no disk space by itself; `DATABASE_OPERATIONS.md`
  at the repository root has the `VACUUM FULL`/`pg_repack` runbook and the
  measurements.
- epistula-api derives `html_body` of `GET /v1/messages/{id}` from the raw
  blob on each request that asks for it, byte-for-byte what ingest stored
  before; `html=false` skips the read. `/v1/search` and `q=` on `/v1/export`
  use the expression index and answer 503 until migration 027 exists.
  `pass_required=true` is scoped on the queue's own `mailbox_id`, so a
  worker whose mailbox has nothing queued reads its empty slice of the queue
  through its index instead of walking every message (1.5 s a round before,
  with 357K markers queued for mailboxes nobody annotates), and
  `POST /v1/pass-required/prune` is one set of hash joins over the scoped
  queue rather than a correlated subquery per marker. Both answer 503 until
  migration 029.
- epistula-imap STATUS computes only the aggregates the client asked for,
  each from an index: on a 46K-message INBOX the poll every client issues for
  every folder dropped from 121 ms and 19,308 buffers (a scan of the folder's
  rows, the largest read on the store) to an index-only count. SEARCH BODY
  and TEXT match the text projection only: the decoded HTML is no longer
  stored, so a word present only in the HTML alternative of a
  multipart/alternative message, or only in markup, is no longer found. The
  daemon logs an error at startup when the schema is behind migration 029
  (COPY and MOVE write the marker's `mailbox_id`).
- The live archive sorter no longer reads every row of a mailbox's `\Archive`
  folder on each pass. Its candidate query first joins only `(folder_id, id)`
  against `message_classifications`, which epistula-database migration 026's
  `idx_messages_folder_id_id` serves index-only, and reads the wide `messages`
  heap for qualifying candidates alone. On a 274k-message store the old pass
  touched ~6,000 heap pages a minute to file nothing and intermittently hit
  the 10 s statement timeout. Run `migrate up`; large stores may pre-create the
  index `CONCURRENTLY` under the same name first (see `ARCHIVE_SORTING.md`).
- Outbound submission of finished messages, off by default. epistula-database
  migration 025 adds the `submit_message` token permission (run `migrate up`;
  `admin api-token-add -permission submit_message` mints it). epistula-api
  adds `POST /v1/mailboxes/{name}/submissions`, which relays one complete
  RFC 5322 message to the MTA's authenticated submission service (STARTTLS
  with a verified certificate, AUTH PLAIN) as that mailbox, changing nothing
  but line endings to CRLF so DKIM, S/MIME and OpenPGP signatures still
  verify. The envelope sender and every `From`/`Sender` address must be the
  mailbox's own explicit aliases; every recipient must be accepted or nothing
  is sent; the response carries the queue id and the sha256 of the bytes sent,
  and a `504` says when a message may have been sent without a reply. The
  relay is configured per mailbox in `[[submission.accounts]]` and bounded by
  `[submission]` size, recipient, per-token hourly and session-time limits.
  epistula-mcp adds the `send` tool, registered only with a third credential,
  `send_token`, which may not equal `token` or `annotate_token`; it is marked
  destructive and open-world so MCP clients ask before each call, and it never
  retries.
- epistula-mcp's `send` tool prepends `User-Agent: epistula-mcp/<version>
  (+https://github.com/ptudor/epistula-mail)` to a message that has no
  `User-Agent` or `X-Mailer` field and no `DKIM-Signature`, and otherwise sends
  it unchanged. Its epistula-api requests, and every epistula-llm-worker
  request to epistula-api and LM Studio, now carry a `User-Agent` of the same
  form with the project URL; the worker used to send Go's default.
- The epistula-database Linux package's post-install no longer fails when the
  `epistula-imap` user does not exist yet. RPM runs it before epistula-imap's
  own pre-install when both packages install in one transaction, and the spool
  ownership check named that user to `find`, which rejects an unknown name; on
  Fedora 43 the failed scriptlet fails the whole transaction.
- Database metrics `mail_database_messages` and `mail_database_gc_candidates`
  are now `mail_database_messages_estimate` and `mail_database_gc_candidates_estimate`.
  They use PostgreSQL live-tuple statistics to avoid full-table counts per scrape;
  update dashboards and allow for statistics lag.

- IMAP Trash purge now defaults `purge_all_copies` to false, preserving copies
  in other folders and newer Trash rows. Existing explicit `true` settings
  retain their destructive all-copy behavior; review them before enabling purge.

- All five services reject unknown TOML keys with their key paths and line
  numbers. Previously tolerated misspellings now fail configuration loading.
  Configuration discovery uses only `/usr/local/etc/epistula/<service>.toml`
  and `/etc/epistula/<service>.toml`, where `<service>` is `epistula-database`,
  `epistula-imap`, `epistula-api`, `epistula-llm-worker` or `epistula-mcp`.
  Use `-config` for other locations; the working directory is no longer searched.
- Also incorporates the security fixes from Go 1.27.3 and
  `golang.org/x/net` v0.62.0 for advisories in `net/http`, HTTP/2, `crypto/tls`,
  `net/textproto`, `os`, and
  `crypto/x509` (GO-2026-6603 through GO-2026-6617, GO-2026-6809) and in
  `golang.org/x/net/html` (GO-2026-6860, GO-2026-6866). Rebuild and redeploy
  every component.
- Includes `prometheus/client_golang` v1.25.0, `go-toml` v2.4.3, the MCP Go
  SDK v1.8.0, and the PostgreSQL DSN parsing changes introduced in `pgx`
  v5.11.0. `pgx` now parses PostgreSQL connection strings as libpq does:
  a raw `=` inside a URI query value is a parse error (percent-encode it as
  `%3D`), a backslash in
  a keyword/value string escapes the next character, and a repeated URI
  parameter takes its last value instead of its first. Run `check-config`
  against each existing configuration before restarting on the new build.
- `check-config` (database, IMAP and API) now asks the PostgreSQL connection
  layer to parse `postgres.dsn` in every mode, opening no connection. Only
  production mode used to, so a development configuration could pass
  `check-config` with a DSN the daemon then failed to start with: an
  unreadable `sslrootcert`, or a spelling `pgx` 5.11 no longer accepts.
- epistula-imap: an authenticated command over `command_rate_per_sec` now waits
  for its turn instead of failing with `NO [LIMIT]`. A client checking a
  mailbox with thousands of folders sends one STATUS per folder in a burst
  every few minutes; most of each sweep was refused, so those folders were not
  checked, and the server logged a warning per refused command. The sustained
  rate limit is unchanged. The log now gets one line per burst, and the new
  `imap_database_commands_paced_total` metric counts delayed commands.
- FreeBSD: `service epistula_mcp restart` no longer fails with "process already
  running" and leaves the connector stopped. Stop now waits for the daemon(8)
  supervisor as well as the connector, and it ignores a stale supervisor
  pidfile.
- epistula-api: add `GET /v1/messages/{id}/attachments/{part}`, which streams one
  attachment's decoded bytes to a `read_content` token in the message's mailbox
  scope. The response is a non-rendering download (`Content-Disposition:
  attachment`, `nosniff`, a sandboxing CSP), and the blob's size is checked
  against the database before any header is sent, as for `/raw`. The service
  user now also reads the `<mailbox>/att` blob tree, which the storage group
  already covers.
- epistula-mcp: add the read-only `attachment` tool, which returns an
  attachment as MCP content: an image for PNG/JPEG/GIF/WebP, embedded text for
  UTF-8 text files, and an embedded blob with its MIME type for anything else,
  such as a PDF. Files over the new `max_attachment_bytes` setting (default
  10 MiB) are refused rather than truncated.

## 1.0.0 — 2026-10-01

- Establish Epistula's public repository from the current five-component baseline,
  with a separate explanatory introduction commit for each source file.
- Provide PostgreSQL mail delivery, immutable message and attachment blobs,
  administrator commands, Maildir import, verification, recovery, and garbage collection.
- Provide IMAP access over implicit TLS, scoped HTTP search and export, optional
  model annotations and archive classification, and an MCP connector.
- Define the PostgreSQL schema, environment variable names, and Prometheus
  metric names as stable 1.0 contracts.
- Add contributor and security guidance, integration CI, third-party notices,
  signed release archives and Linux packages, and signature verification.
