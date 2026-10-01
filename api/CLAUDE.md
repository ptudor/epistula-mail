# Epistula — api

A read-mostly HTTP/JSON API over the Postgres + content-addressed blob store written by `database`. It is the **third reader** of that schema — alongside the writer `database` and the interactive-client reader `imap` (IMAP) — purpose-built for *programmatic* consumers: in-house LLM summarization and **tagging**, indexing, analytics, archival export, and ad-hoc search that would be clumsy or slow over IMAP. It is read-mostly — almost every request is a query — and its single write is a derived **annotation sidecar** (tags, an advisory sort category, an optional summary). It performs **no** IMAP message-state changes: moving, flagging, and expunging stay in `epistula-imap`. The blobs are immutable and every reader coordinates through the database, so this API only needs to *inform* its consumers — not mutate mailbox state.

## Why this exists (and why it is a separate process)

IMAP is a stateful, connection-oriented protocol designed for interactive mail clients. Pulling "every message in this mailbox older than a year, give me the plaintext" over IMAP means SELECT → SEARCH → per-message `FETCH BODY[TEXT]` with UID bookkeeping. But the decoded plaintext the summarizer wants is already a column — `messages.text_body`, with the HTML→text projection computed once at ingest — indexed by the `fts` tsvector. A single parameterized query returns it.

This daemon is **not** a `epistula-database serve` mode: that listener is deliberately metrics/health only, binds loopback, and has *zero* message-access surface by design (no auth to harden, no privileged data). It is **not** an `epistula-imap` extension: that is the IMAP protocol server. Keeping content-API as its own stateless process means:

- An API outage never stops Postfix delivery and never touches IMAP.
- It deploys, scales, and is rate-limited independently.
- Its auth surface (service tokens) is isolated from both the LDA (none) and IMAP (per-mailbox Argon2id).
- Cursor pagination and NDJSON streaming walk large folders without increasingly expensive `OFFSET` scans.

## Design goal

**Stateless, read-mostly, query-shaped.** Every request is one or more parameterized transactions; no per-connection state. The daemon is read-dominant: its single write is the *annotation sidecar* — a message's tags, an advisory sort category, and an optional summary, stored in a derived table. It never moves messages between folders, never allocates UIDs, never sets IMAP flags, never expunges, never mutates a message's bytes/headers/text, never writes blobs, never changes schema. Those are "core IMAP" state changes and belong to `epistula-imap`; this API reads the store and writes only derived metadata. Postgres is the source of truth; blobs are immutable and content-addressed.

The daemon **owns no schema**. `epistula-database` defines and migrates every table (including `api_tokens` and the `message_annotations` table this project reads/writes). `epistula-api` bumps its dependency on `epistula-database` to pick up schema changes, exactly as `epistula-imap` does.

## Common patterns (inherited from parent)

| Pattern | Implementation |
|---------|----------------|
| **Transport** | Plain HTTP on a loopback listener; Apache terminates TLS (and optionally mTLS) in front. No TLS in the Go process. |
| **Logging** | Structured logging via `log/slog` (JSON or text). Never logs bodies, summaries, or token values. |
| **Metrics** | Prometheus + expvar on a separate admin listener. |
| **Graceful Shutdown** | SIGINT/SIGTERM: stop accepting, drain in-flight requests with a timeout, close the PG pool. |
| **Configuration** | TOML config file (preferred) or environment variables; same precedence as the other Epistula daemons. |
| **Health Checks** | `/health` and `/healthz` on the admin listener. |
| **Database** | PostgreSQL only, `sslmode=verify-full` in production. A dedicated least-privilege DB role (read grants + writes on the derived sidecar tables only) is the recommended deployment. |
| **Errors (HTTP)** | RFC 7807 `application/problem+json` for every error response. |

### Dependencies

- `github.com/jackc/pgx/v5` — Postgres driver (as in `epistula-database` and `epistula-imap`).
- `github.com/prometheus/client_golang` — metrics.
- `github.com/pelletier/go-toml/v2` — config.
- `golang.org/x/crypto/argon2` — API-token verification (tokens are Argon2id-hashed at rest).
- `crypto/subtle` — constant-time token comparison.
- The shared `storage` / `blob` / `auth` packages from `epistula-database`, imported via a `replace` directive pointing at `../database` (same monorepo coupling as `epistula-imap`; the projects move in lockstep and version skew is impossible by construction).
- Standard library `net/http` — **no web framework.** Kept minimal; routing is the stdlib `http.ServeMux` with method-aware patterns (Go 1.22+).

Go toolchain: use [the repository pin](../.go-version), including when building this component alone.

---

## Architecture

```
LLM classifier / summarizer / indexer / analytics job
   │  HTTPS + Bearer token  (+ optional mTLS)
   ▼
Apache (TLS termination, optional client-cert)
   │  HTTP, loopback
   ▼
epistula-api serve
   ├─► Postgres (read)    folders, messages (text_body/html_body/headers/fts), mailboxes
   │   Postgres (write)   message_annotations  (tags/category/summary upsert — derived sidecar)
   │                      api_tokens            (verify; read-only)
   └─► Disk blobs (read-only)   <mailbox>/raw/<yyyy/mm/dd>/<aa>/<bb>/<sha>.eml   →  GET .../raw
```

Two listeners, mirroring `epistula-imap`:
- **API listener** on `127.0.0.1:8784` (configurable) — the JSON surface, behind Apache.
- **Admin listener** on `127.0.0.1:8785` (configurable) — `/metrics`, `/health`, `/healthz`, loopback only.

---

## Authentication & authorization (the central security decision)

The consumer is a **service** (a summarization pipeline), not a person. Reusing mailbox `password_hash` would conflate "a human logging in" with "a batch job reading," and would force the pipeline to hold a user's password. So:

- **Bearer API tokens.** `Authorization: Bearer <token>`. Tokens are random 256-bit secrets, presented once at mint time, stored only as an Argon2id PHC hash in `api_tokens`. Verification reuses the `auth` package and `crypto/subtle`.
- **Per-token mailbox scope.** Each token carries a scope: a set of mailbox names (or `*` for all) and one or more permission levels (`read_metadata`, `read_content`, `write_annotation`). A classifier token is typically `read_content + write_annotation` limited to exactly the mailboxes it should ever touch; an analytics token might be `read_metadata` only. A token can read without writing, or read-and-annotate — the levels compose.
- **Token lifecycle lives in `epistula-database`, not here.** `epistula-database` owns the schema, so `api_tokens` is a `epistula-database` migration and token CRUD is a `epistula-database admin api-token-{add,list,revoke}` subcommand (one secret printed at creation, never recoverable). `epistula-api` only *verifies* — it never mints. This preserves the "Go owns the admin CLI; this reader owns no admin surface" boundary that `epistula-imap` already follows.
- **Loopback bind + Apache front.** The Go process binds `127.0.0.1`; Apache terminates TLS. For external access (e.g. a summarizer on another host), front with **mTLS at Apache** — defense in depth on top of the bearer token.
- **Recommended: a dedicated least-privilege Postgres role.** Grant `SELECT` on the read tables and `api_tokens`, `SELECT/INSERT/UPDATE` on `message_annotations` and `message_classifications`, `SELECT/DELETE` on the annotation pass queue, and `EXECUTE` on `mail_lock_message_for_annotation` — nothing else (no other `DELETE`, no write on `messages`/`folders`, no blob access, no quota writes). The function is how an annotation write locks its message against a concurrent MOVE without `UPDATE` on `messages`, which a row lock would otherwise need (OPS-008); `deploy/README.md` has the exact grants. Then even an application bug cannot write outside the annotation sidecar, and can never touch message content, mailbox state, blobs, or quotas.
- **Login throttling.** Per-IP failure throttling on bad bearer tokens, with a fixed-delay response, to blunt token-guessing. A global semaphore additionally caps concurrent Argon2id verifications at ~`NumCPU`, returning `429` when saturated so a burst of distinct bad tokens can't exhaust memory before the throttle records them (R-016).
- **Per-token request rate limiting.** An optional token-bucket limiter keyed by token id (`[limits] per_token_rate` requests/sec + `per_token_burst`; `0` disables, the default) returns `429` when a single token exceeds its budget — for a compromised or runaway token hammering `/raw` or `limit=500` FTS pages. It is separate from the concurrent-export-stream cap and is set well above a worker's steady rate, so normal throughput is unaffected (R-017).

### Token-scope enforcement

Every content query is filtered by the token's mailbox scope at the SQL level (`WHERE mailbox_id = ANY($scope)`), never in application code after the fact. A `*`-scope token skips the filter; any scoped token can only ever address its own mailboxes, and a request for an out-of-scope mailbox returns `403` (RFC 7807), not `404` — the token-holder is authenticated, just not authorized.

**Accepted tradeoff — numeric message-id existence oracle (R-070).** Because `messages.id` is one global `BIGSERIAL` and an out-of-scope-but-present message id returns `403` while an absent id returns `404`, a scoped token can probe `/v1/messages/{n}` (also `/text`, `/raw`, annotation `PUT`) to distinguish "exists elsewhere" from "absent" and thereby estimate system-wide message-id density/arrival rate across all mailboxes. This is **accepted, not a defect**: the `403`-not-`404` rule is the intended contract for authenticated-but-unauthorized access (it is exercised by `TestAPIOutOfScopeIs403`), the leaked signal is coarse (a monotonic id counter, no content), and the worker only ever fetches in-scope ids returned by `/v1/export`, so it is unaffected. If cross-tenant id-density ever becomes sensitive, the change is to return `404` for out-of-scope *numeric message-id* routes (keeping `403` for *named-mailbox* routes) and update this doc plus `TestAPIOutOfScopeIs403`; until then the oracle is documented and accepted.

---

## Endpoints (v1)

All under `/v1`. JSON request/response; RFC 7807 on error. Cursor pagination everywhere a list can be large.

| Method & path | Purpose |
|---|---|
| `GET /v1/mailboxes` | Mailboxes visible to the token. |
| `GET /v1/mailboxes/{name}/folders` | Folders in a mailbox (name, uidvalidity, message count). |
| `GET /v1/mailboxes/{name}/folders/{folder}/messages` | Paginated message list. Filters: `since`, `before` (on `internal_date`), `sent_since`/`sent_before`, `flag`/`not_flag`, `larger`/`smaller`, `tag`, `category` (on a stored classification), `annotated_by`/`not_annotated_by` (by annotation model). Projection: `fields=metadata` (default, `read_metadata`); add `text` to inline `text_body` (**requires `read_content`**); add `annotation` to inline stored tags/category/summary — ranked, see "Annotations" — which **requires `read_content`** because `summary` is body-derived LLM prose (R-012). |
| `GET /v1/messages/{id}` | One message: subject, from/to/cc, dates, flags, headers (JSONB), text_body, html_body, attachment metadata, BODYSTRUCTURE. |
| `GET /v1/messages/{id}/text` | `text/plain` body only — the ready-to-summarize input (HTML→text projection already applied at ingest). |
| `GET /v1/messages/{id}/raw` | Original RFC 5322 bytes, streamed from the blob (`message/rfc822`). For reprocessing / re-parsing. |
| `GET /v1/search` | FTS over the `fts` tsvector (`q=`, optional `mailbox=`, `folder=`, `tag=`, `category=`, date filters). Cursor-paginated. **Requires `read_content`**: the tsvector indexes `text_body`, so a `q=` probe is a body-content oracle even though results are metadata-shaped (R-014). |
| `GET /v1/export` | Bulk **NDJSON stream** of messages matching a filter, for "summarize the whole archive." Streams row-by-row so the consumer pipelines without the server buffering the whole corpus. Annotations are always included when the sidecar table exists (not gated on `fields`). Each row also lists the message's attachments (the objects `GET /v1/messages/{id}` lists: part number, name, type, disposition, size, sha256), counted against the page byte budget, so a classifier sees what a message carries and not only how many attachments it has (OPS-009). Honors the token scope and a server-side hard cap on concurrent export streams. Note: each cursor batch materializes up to `export_page_size` full `text_body` values before streaming, so peak memory scales with `export_page_size × mean body size`; lower `export_page_size` for corpora with very large bodies (the LDA caps raw at 50 MiB). |
| `GET /v1/threads/{message_id}` | *(medium priority)* Thread reconstruction via `message_id` / `in_reply_to`. |
| `PUT /v1/messages/{id}/annotation` | *(see "Annotations")* Store/replace a message's annotation — `tags`, an advisory sort `category`, optional `summary`. Idempotent on `(message_id, model)`. Requires a `write_annotation`-scoped token. |
| `GET /v1/mailboxes/{name}/archive-categories` | *(see "Archive classification")* The mailbox's active archive categories (`key`, `folder`, `description`) and its `\Archive` folder. `read_metadata`. An empty list means archive sorting is not set up for the mailbox. |
| `PUT /v1/messages/{id}/classification` | *(see "Archive classification")* Store the classifier's choice: `{category, confidence, model}`. Requires `write_classification`. `422` unless `category` is an ACTIVE category of the message's own mailbox. One row per message; the latest wins. |
| `POST /v1/pass-required/prune` | *(see "Annotation pass queue")* `{model, require_classification, mailbox?}`: clear the markers of queued messages the pipeline has finished, in the token's scope. Returns `{pruned, remaining}`. Requires `write_annotation`. |

List, search and export also take `not_classified=true` (messages with no classification into an active category of their mailbox: the classifier's undone work; allowed to a metadata token, like `not_annotated_by`), `pass_required=true` (messages queued for the annotation pipeline, see below) and `sample_ppm=N` (a deterministic N-per-million sample keyed on the message id, for taxonomy sampling).

### Annotation pass queue

`not_annotated_by` and `not_classified` ask about an absence, so answering them
means probing every message even when there is no work. Epistula's database
migration 022 records the work as a presence instead:
every message is marked in `annotation_pass_required` in the transaction that
stores it, and epistula-imap's COPY and MOVE carry the marker. `pass_required=true`
reads the ids out of that queue and fetches the messages by primary key
(`m.id = ANY(ARRAY(SELECT ...))`; an `IN (SELECT ...)` let the planner walk every
older message first), so the work scales with the queue rather than the archive.
The worker's fast round asks for `pass_required` with its usual filters and then
calls prune, which deletes the markers of messages annotated by its model and,
with `require_classification`, classified into an active category (a mailbox
with no categories needs none). A message still missing either stays queued and
is retried. What no insert marks (a new model, a retired category, mail stored
before migration 022) is the worker's complete round, which still uses
`not_annotated_by` and `not_classified`. Both answer 503 until migration 022 and
this role's `SELECT, DELETE` on the table are in place.

### Pagination — never `OFFSET`

List and search responses carry an opaque, base64-encoded **cursor** over `(internal_date DESC, id DESC)` (or `(uid)` within a folder). The next page is `?cursor=<opaque>`. There is a configurable max page size; a consumer that wants everything uses `/v1/export` (streaming) rather than deep pagination. `OFFSET N` is never emitted — walking a 180k-message folder must stay O(page), not O(offset).

---

## Annotations (the first sidecar)

This is the motivating use case: an in-house model reads a message and decides its tags, a sort category, and a summary; `epistula-api` stores that so any consumer (or a later query here) can retrieve it. Storing the annotation is the **only** write `epistula-api` performs — and it is deliberately *not* a "core IMAP" change. **`epistula-api` does not call an LLM**; the intelligence lives in the caller, which reads `/v1/messages/{id}/text`, runs the in-house model, and `PUT`s the result back.

- **Schema:** a `message_annotations(message_id, model, tags TEXT[], category TEXT, summary TEXT, tokens_in, tokens_out, created_at)` table — defined by a `epistula-database` migration, because `epistula-database` owns the schema. Keyed by `(message_id, model)` so multiple models can each hold an independent annotation and re-running a model replaces rather than duplicates. `tags` is the label set (GIN-indexed for the `tag=` filter); `category` is an **advisory** sort label (a folder name or category key the consumer suggests — `epistula-api` stores and serves it but never acts on it); `summary` is the optional prose. It is a pure sidecar: the raw bytes stay immutable on disk and the message's IMAP state (`folder_id`, `uid`, `flags`) is untouched.
- **Endpoint:** `PUT /v1/messages/{id}/annotation` with `{model, tags, category, summary, tokens_in, tokens_out}`, gated on a `write_annotation`-scoped token, parameterized, idempotent on `(message_id, model)` via `ON CONFLICT DO UPDATE`. It writes only `message_annotations` — never `messages`, `folders`, blobs, or UIDs.
- **Ranking & alternatives (multi-model).** Because each model keeps its own row, a message accrues one summary per model — every model's take is retained, including across model versions, instead of one overwriting another. The optional `annotation_models(model, priority, display_name, retired_at)` registry (`epistula-database` migration 008, managed by `admin annotation-model-{set,list,retire}`) ranks them: read endpoints (`GET /v1/messages/{id}`, which always includes annotations; the list endpoint under `fields=annotation`; and `/v1/export`, which **always** includes annotations whenever the sidecar table exists — it does *not* require `fields=annotation`, and the belt-and-suspenders worker relies on that) return a message's annotations **ordered highest-priority-first**, each carrying its `priority`, with the winner flagged `primary` (a *retired* model is served as an alternative, never primary). Unregistered models sort as priority-0 alternatives; absent the registry, annotations are returned unranked with no `priority`/`primary` fields. This is how the "keep both a quality model's and a fast model's summary, show the preferred one" pattern maps onto the store — and how a second GPU box running a different model coexists: each box reads `not_annotated_by=<its own model>` on `/v1/export` to pull only its undone work, and `annotated_by=<model>` filters to a given model's coverage.
- **Why not file/move the message?** Moving a message into a folder is a core IMAP state change: a new per-folder UID, a `pg_notify` to wake IDLE clients, full `MOVE` semantics. Because the blobs are immutable and every reader coordinates through the database, this API doesn't need to own that — it records the *suggestion* (`category`), and the interactive client (over `epistula-imap`) acts on it if a human ever wants the message physically moved. Keeping mutation out of `epistula-api` means no UID-allocation path, no overlap with `epistula-imap`'s `MOVE`, no IDLE-notify coupling, and a strictly read-plus-sidecar DB grant. Consumers that sort by classification just filter on `tag=`/`category=` at read time.

This write is **v1 core** (it is why the project exists), gated only on the `epistula-database` `message_annotations` migration landing first. Until then `epistula-api` can ship its read surface; the annotation endpoint, the annotation filters, `fields=annotation`, and `GET /v1/messages/{id}` (whose document always includes annotations) all return `503` while the migration is absent (checked at `serve` startup; R-066).

---

## Archive classification (the second sidecar)

Archive sorting (`../ARCHIVE_SORTING.md`, epistula-database migration 020) files an
archived message into `Archive/<Category>`. epistula-api's part is only the label:
it serves the mailbox's approved category list and stores the classifier's
choice from it in `message_classifications`. It checks the key against the
mailbox's *active* list under the same parent-row lock an annotation write
takes (`mail_lock_message_for_annotation`). epistula-imap's sorter does the
moving; epistula-api still issues no message-state SQL.

It is separate from the annotation on purpose. An annotation's `category` is
advisory, any holder of `write_annotation` (epistula-mcp's `annotate` tool
included) may replace it, and nothing acts on it. A classification files mail,
so it has its own table and its own permission, `write_classification`, which
the interactive connector must never hold. The endpoints answer `503` until
migration 020 and its grants are in place (probed at startup, like the
annotation sidecar).

## What this project does NOT do

- **No message-state mutation at all.** It never moves a message between folders, allocates UIDs, sets/clears IMAP flags, expunges, or runs the interactive STORE/COPY/MOVE/APPEND surface — all of that ("core IMAP") belongs to `epistula-imap`. It never edits a message's bytes/headers/text, never writes blobs, never touches quotas. Its writes are the two derived sidecars (`message_annotations`, `message_classifications`) and clearing finished markers from the annotation pass queue (`annotation_pass_required`).
- **No IMAP, no SMTP, no POP3.** It is an HTTP/JSON query API, nothing more.
- **No schema migrations.** `epistula-database` owns and ships them.
- **No blob writes, no GC.** It reads blobs; it never participates in the GC advisory-lock protocol because it never creates references.
- **No mailbox/domain/alias/token CRUD.** Operator management is `epistula-database`'s `admin` CLI.
- **No LLM/embedding calls.** It does not summarize, embed, or classify — it serves the inputs and stores the outputs.

---

## Security posture (production defaults, non-negotiable)

- `sslmode=verify-full` required when `production=true`; refuses `disable`/`allow`.
- **Every** query is parameterized (`$1`, `$2`, …); no `fmt.Sprintf` into SQL, no identifier interpolation. Token mailbox-scope is an `= ANY($scope)` bind, never string-built.
- API tokens stored only as Argon2id PHC hashes; verified with `argon2.IDKey` + `crypto/subtle.ConstantTimeCompare`; bad-token attempts throttled per-IP and per-token.
- Blob reads are hash-only: the hex sha256 is validated `^[0-9a-f]{64}$` before any path is built (path-traversal eliminated as a class — inherited from the `blob` package).
- A `statement_timeout` is set per request transaction.
- Loopback bind by default; never exposed beyond Apache. mTLS at Apache for external consumers.
- Pagination caps and a concurrent-export-stream cap prevent a single token from pulling the whole store in one unbounded request.
- **Never log** message bodies, summaries, attachment content, or token values. Log: remote IP, token id (not the secret), endpoint, mailbox/message ids, byte counts, outcome. Redact `Authorization` and any `(?i)(token|secret|password|key)` header.
- Recommended dedicated least-privilege Postgres role (read grants + `INSERT/UPDATE` on `message_annotations` + `EXECUTE` on the annotation lock function, and `SELECT, DELETE` on the annotation pass queue; no other `DELETE`, no writes to `messages`/`folders`, no other tables) so the DB enforces the read-plus-sidecar boundary at the grant level, not just in the application.
- `epistula-api` issues **no** message-state SQL — no `UPDATE messages`, no UID allocation, no `pg_notify`. Its only mutating statements are the `message_annotations` and `message_classifications` upserts and the pass-queue prune (a DELETE from `annotation_pass_required`, never from anything else); moving/flagging/expunging is `epistula-imap`'s job.

## Error responses (RFC 7807)

All errors are `application/problem+json`, the error convention the root CLAUDE.md sets for the project's HTTP APIs:

```
{ "type": "https://api.ptudor.invalid/errors/forbidden",
  "title": "Forbidden",
  "status": 403,
  "detail": "Token is not scoped to mailbox 'jdoe'.",
  "instance": "/v1/mailboxes/jdoe/folders" }
```

`403` for out-of-scope (authenticated but not authorized), `404` for genuinely-absent resources, `401` for missing/invalid token, `429` for rate-limited, `422` for a malformed filter/cursor.

## Configuration

TOML, same precedence and search paths as the other Epistula daemons:

```bash
epistula-api serve        -config /usr/local/etc/epistula/epistula-api.toml
epistula-api check-config -config /usr/local/etc/epistula/epistula-api.toml
```

```
/usr/local/etc/epistula/epistula-api.toml
/etc/epistula/epistula-api.toml
./epistula-api.toml
```

Required keys: `[server]` (api listen addr, default `127.0.0.1:8784`), `[admin]` (admin listen addr, default `127.0.0.1:8785`), `[postgres]` (DSN, `sslmode=verify-full` in production; ideally the least-privilege role's DSN), `[storage]` (`root`, group-readable blob access), `[limits]` (max page size, per-token rate, max concurrent export streams, statement timeout), `[production]` (gates strict-mode checks).

## Subcommands

| Subcommand | Purpose |
|------------|---------|
| `serve` | Run the API + admin listeners. |
| `check-config` | Validate the TOML against the `production` ruleset and exit (systemd `ExecStartPre`). |
| `version` | Print version and exit. |

No `deliver`, `import`, `gc`, `admin`, `migrate` — those are `epistula-database`'s. Token CRUD is `epistula-database admin api-token-*`.

---

## Relationship to the rest of the store

```
                       ┌──────────────────────────┐
   Postfix ──pipe────► │   epistula-database (writes)  │  owns schema + migrations + admin CLI + GC
                       └──────────┬───────────────┘
                                  │  Postgres + raw/att blob trees
              ┌───────────────────┤
              ▼                   ▼
   ┌────────────────┐   ┌──────────────────┐
   │ epistula-imap  │   │   epistula-api   │
   │ IMAP4 clients  │   │ HTTP/JSON: read, │
   │ read + APPEND  │   │ search, export,  │
   └────────────────┘   │ annotate         │
                        └──────────────────┘
```

`epistula-api` is to machines what `epistula-imap` is to mail clients: a stateless reader — plus a narrow writer of one derived sidecar (tags/category/summary) — of the one schema `epistula-database` owns.
