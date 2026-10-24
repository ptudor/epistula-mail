# Epistula changelog

## Unreleased

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
- Security: build with Go 1.27.2 and `golang.org/x/net` v0.61.0, which fix
  advisories in `net/http`, HTTP/2, `crypto/tls`, `net/textproto` and `os`
  (GO-2026-6603 through GO-2026-6617). Rebuild and redeploy every component.
- Dependencies: `pgx` v5.11.0, `prometheus/client_golang` v1.25.0, `go-toml`
  v2.4.3, the MCP Go SDK v1.8.0, and current `golang.org/x` modules. `pgx` now
  parses PostgreSQL connection strings exactly as libpq does: a raw `=` inside a
  URI query value is a parse error (percent-encode it as `%3D`), a backslash in
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
