# Epistula — mcp — the mail store exposed over MCP

A small Go MCP server that lets an MCP client — Claude Desktop / Claude Code / the
claude.ai app, or any other host — ask questions about the mail store: "what came in
this week?", "find the invoice from the hosting company", "summarize this thread",
"which messages did the classifier tag `receipts`?". It is a **thin proxy over
`epistula-api`** and speaks slimmed JSON the model can reason about.

**Status: implemented and verified end-to-end against a live epistula-api with a
minted token.** All seven tools, both transports, config, and deploy artifacts
are in place; `go test -race ./...` passes (unit + httptest-mock integration
round trips for every tool) and the stdio `make smoke` handshake lists the seven
tools. This document is the contract the implementation is held to;
`ROADMAP.md` records what v1 covers and what is deliberately left for later.

## Why a proxy over epistula-api, not a fourth schema reader

`epistula-api` already exists specifically for programmatic
consumers, and re-reading the schema here would discard everything it provides:

1. **Scope enforcement lives in epistula-api.** Every content query is filtered by the
   bearer token's mailbox scope at the SQL level, with composable permission levels
   (`read_metadata`, `read_content`, `write_annotation`). A direct-DB MCP would need
   an all-mailboxes read role over private mail and would have to re-implement
   scoping in a second codebase. Instead, the MCP's capability *is its token*: mint a
   token scoped to exactly the mailboxes and permissions the MCP should have
   (`epistula-database admin api-token-add`), and even a bug here cannot exceed it.
2. **The schema contract stays "one writer, three readers."** `epistula-database` owns the
   schema; `epistula-imap` and `epistula-api` track it via `replace` directives and move
   in lockstep. epistula-mcp depends only on the stable `/v1` HTTP contract — no
   `replace` directive, no schema coupling, no lockstep rebuilds when a migration
   lands.
3. **The hard parts are already server-side.** FTS over the `fts` tsvector, cursor
   pagination (never `OFFSET`), annotation ranking (highest-priority model first,
   `primary` flag), rate limiting, RFC 7807 errors, statement timeouts. Keep it a
   proxy, not a second brain.

The security story is therefore "**guard an API token**," not "read-only by
construction." A read-only MCP is simply an MCP holding a read-only token;
epistula-api rejects anything beyond the token's scope regardless of what this
process asks for.

## What it is (and isn't)

- **A question-answering surface for a model.** Domain tools that speak the mail
  store's vocabulary (mailboxes, folders, messages, tags, categories, annotations),
  returning slimmed JSON.
- **A thin proxy.** Each tool call becomes one HTTPS/HTTP request to `epistula-api`'s
  `/v1` surface with a bearer token. No database driver, no local cache, no LLM
  calls, no business logic. All real logic (scoping, search ranking, annotation
  priority) stays in `epistula-api`.
- **Read-mostly, exactly like its upstream.** The read tools cover mailboxes →
  folders → messages → text → search. The single write tool, `annotate`, forwards to
  `PUT /v1/messages/{id}/annotation` — the one write epistula-api itself performs — and
  works only if the token carries `write_annotation`.
- **Not a bulk pipe.** `/v1/export` (NDJSON streaming for whole-archive jobs) is the
  llm-worker's interface, not a chat tool; the MCP does not wrap it. A model that
  wants "everything" pages with cursors like any other consumer.
- **Not an operator console.** No mailbox/domain/alias/token CRUD, no GC, no
  migrations — that is `epistula-database admin`'s job.

## Design principles (hold new code to these)

- **Stay a proxy.** Turn a tool call into an HTTP request and slim the response. No
  SQL, no caching layer, no local datastore, no LLM/Anthropic calls. If a tool needs
  data epistula-api doesn't serve, the feature belongs in epistula-api first.
- **Capability follows the token — and there are two of them.** Don't add MCP-side
  permission knobs that shadow epistula-api's token scopes; the deployment decides what
  the MCP can do by which tokens it mints. `token` authenticates the six read tools.
  `annotate_token` is a **separate, optional** credential used only by `annotate`,
  and **when it is absent the `annotate` tool is not registered at all** — with no
  write credential there is no capability to advertise. That is not a permission
  knob: there is no boolean, and this process still never decides what a token may
  do. Mint the read token *without* `write_annotation` so epistula-api enforces the
  split in SQL too. A config setting both to the same value is rejected at load —
  it would read as a split while the read tools still held write scope.

  **Why not simply let an unscoped `annotate` earn a 403:** every message body is
  attacker-controlled input, so a model reading mail can be instructed to write. A
  403 stops the write, but the tool is still reachable and the attempt still
  happens. An unregistered tool cannot be called.
- **Guard the secrets.** Two of them: the epistula-api bearer token and the `-http`
  transport's own `http_token`. Never log either, never echo one in an error, never
  put one in argv or a URL. `Authorization` headers never appear in logs.
- **Slim the payload.** Project responses to the fields that answer the question and
  omit nulls/empties. Never forward `html_body`. Cap message text at a configured
  byte budget with an explicit `truncated: true` marker. Cap every list with a
  clamped `limit`.
- **Fail loud, bounded, correctable.** A bad argument or an upstream RFC 7807 problem
  comes back as `{"error": …}` tool-result data the model can correct (status, title,
  detail — bounded, never a full upstream body dump), not a protocol fault. Timeouts
  are clean errors, not hangs.
- **Config is TOML, never `.env`.** Env vars (`MAIL_MCP_*`) override the
  file so the client-launched stdio path can run with zero files.
- **https only, except loopback.** `base_url` must be `https://` unless the host is
  loopback (epistula-api's native posture is plain HTTP on `127.0.0.1:8784` behind
  Apache). Never attach the token to a cleartext non-loopback request.
- **Use the SDK's grain.** Register tools with typed input structs and `jsonschema`
  tags; let the SDK generate schemas. Don't hand-write JSON Schema.
- **Keep stdout sacred.** Protocol JSON only on stdout in stdio mode; logs to stderr.
- **Scope discipline.** Build exactly the tools below. No gold-plating.

## Architecture

```
MCP client (Claude Desktop / Claude Code / claude.ai)
   │  stdio (subprocess)  — or —  Streamable HTTP + http_token bearer
   ▼
epistula-mcp                                    (this project; no DB, no schema import)
   │  HTTP + Authorization: Bearer <epistula-api token>
   ▼
epistula-api serve  (127.0.0.1:8784, or https:// via Apache from another host)
   │  token scope enforced at SQL level
   ▼
Postgres + content-addressed blob store     (schema owned by epistula-database)
```

## Tools

Seven tools. Names and argument names are chosen for the model to reason about, not
for parity with epistula-api's URL shapes. Prefer the domain tools; `search` is the
free-text entry point.

| Tool | Upstream call | What it answers |
|---|---|---|
| `mailboxes` | `GET /v1/mailboxes` | Which mailboxes can I see? **Run first.** |
| `folders` | `GET /v1/mailboxes/{m}/folders` | Folder names, uidvalidity, message counts — the store's shape and volume. |
| `messages` | `GET …/folders/{f}/messages` | Paginated message list in one folder. Filters: `since`/`before` (received), `sent_since`/`sent_before`, `flag`/`not_flag`, `larger`/`smaller`, `tag`, `category`, `annotated_by`/`not_annotated_by`; `cursor`, `limit`. |
| `message` | `GET /v1/messages/{id}` | One message in full: subject, from/to/cc, dates, flags, selected headers, attachment metadata, ranked annotations. `html_body` is dropped; `text_body` is capped. |
| `message_text` | `GET /v1/messages/{id}/text` | Just the ready-to-read plaintext body (HTML→text already applied at ingest), byte-capped with a `truncated` flag and an `offset` argument to continue. |
| `search` | `GET /v1/search` | Full-text search (`q=` over the tsvector) with optional `mailbox`, `folder`, `tag`, `category`, `since`/`before`; cursor-paginated. |
| `annotate` | `PUT /v1/messages/{id}/annotation` | Store/replace this model's annotation: `tags`, advisory `category`, optional `summary`. Idempotent on `(message_id, model)`. **Registered only when `annotate_token` is configured**, and the write rides that token, never the read one. |

Six tools by default, seven with an `annotate_token`. Read-only is the default
posture on purpose.

Deliberately excluded from v1 (see `ROADMAP.md` "Later / maybe"): `raw` (RFC 5322
bytes are for reprocessing pipelines, not context windows), `export` (bulk NDJSON),
`threads` (epistula-api hasn't shipped the endpoint yet — add the tool when it exists).

**Time arguments accept both forms.** Absolute RFC 3339 (`2026-07-01T00:00:00Z`) is
passed through; relative lookbacks (`30m`, `6h`, `2d`, `1w` — Go durations plus
day/week suffixes) are converted to RFC 3339 before the upstream call, because
epistula-api's `parseTimeParam` takes absolute times. Models are much better at
"2d" than at computing timestamps.

**Cursors are opaque.** List/search results return epistula-api's cursor verbatim as
`next_cursor`; the model passes it back via the `cursor` argument. The MCP never
inspects or fabricates cursors.

## Transports

- **stdio (default):** the MCP client launches the binary as a subprocess and speaks
  JSON-RPC over stdin/stdout. The normal per-client mode. Run it on a workstation with
  `base_url` pointing at the Apache-fronted `https://` epistula-api (bearer token rides
  inside TLS), or on the epistula-api host itself against `http://127.0.0.1:8784`.
- **Streamable HTTP (`-http <addr>`, suggested port `127.0.0.1:8786`):** a
  long-running daemon for the rc.d/launchd case or to give a remote MCP host one
  endpoint. The SDK transport has no auth of its own, so this server adds a **bearer
  token** (`http_token`): every HTTP request must carry
  `Authorization: Bearer <http_token>`, and the daemon **refuses to bind a
  non-loopback address unless the token is set** — you cannot accidentally expose an
  endpoint that holds a mail-reading credential. On loopback the token is optional.
  For anything public, TLS still goes in front (Apache, as everywhere else in this
  stack).

Port note: the stack already uses 8782/8783 (epistula-imap), 8784/8785 (epistula-api),
and 8790 (llm-worker admin); 8786 is free.

## Configuration — TOML, no `.env`

Default paths follow the repo convention: `/usr/local/etc/epistula/epistula-mcp.toml`,
then `/etc/epistula/epistula-mcp.toml`, then `./epistula-mcp.toml`; `-config` overrides.
`chmod 600` — it holds the epistula-api token.

```toml
[mailapi]
base_url        = "http://127.0.0.1:8784"   # https:// required for non-loopback
token           = ""                         # READ token: read_metadata + read_content
annotate_token  = ""                         # optional WRITE token: write_annotation only
request_timeout = "30s"

[mcp]
http_token     = ""    # bearer for -http; required to bind non-loopback
max_limit      = 200   # clamp for every list/search `limit`
max_text_bytes = 65536 # cap on message text returned to the model
```

Env overrides (highest precedence, so the stdio path can run with zero files):
`MAIL_MCP_BASE_URL`, `MAIL_MCP_TOKEN`, `MAIL_MCP_ANNOTATE_TOKEN`, `MAIL_MCP_HTTP_TOKEN`.

**Mint dedicated tokens for the MCP** — never reuse the llm-worker's. Token CRUD is
`epistula-database admin api-token-{add,list,revoke}`; each secret is printed once at
mint time. Generate every secret randomly — no placeholders, ever.

```sh
# read token — note the ABSENCE of write_annotation
epistula-database admin api-token-add -name epistula-mcp-read \
    -mailboxes "jdoe" -permission read_metadata -permission read_content

# write token — only needed if the annotate tool should exist at all
epistula-database admin api-token-add -name epistula-mcp-annotate \
    -mailboxes "jdoe" -permission write_annotation
```

Leaving `annotate_token` empty is the default and the safe posture: six read tools,
no write capability present for a model reading attacker-supplied mail to reach.
Setting both keys to the same value is rejected at config load.

## Layout

- `main.go` — flags (`-http`, `-config`, `-version`), config load, transport select.
- `config.go` — TOML + env overrides; https/loopback validation; caps.
- `client.go` — the epistula-api HTTP client: bearer header, timeouts, RFC 7807
  decoding into bounded tool errors, retry-free (the model retries).
- `tools.go` — the seven tools, typed input structs, window parsing, slimming.
- `auth.go` — `-http` bearer middleware + loopback-bind guard.
- `*_test.go` — pure-logic tests + `httptest`-mock-epistula-api integration tests.
- `version.go` — `main.Version`/`main.BuildTime` stamped via `-ldflags`
  (repo convention).
- `epistula-mcp.toml.example` — sample config + token-minting recipe.
- `deploy/freebsd/epistula_mcp` — rc.d script (repo convention);
  `deploy/launchd/net.ptudor.epistula-mcp.plist` — macOS LaunchDaemon.

## SDK & dependencies

- `github.com/modelcontextprotocol/go-sdk` (the `…/mcp` package) — the official
  MCP Go SDK. Register with
  `mcp.AddTool(server, &mcp.Tool{Name, Description}, handler)`;
  handler `func(ctx, *mcp.CallToolRequest, In) (*mcp.CallToolResult, any, error)`.
- `github.com/pelletier/go-toml/v2` — config.
- Standard library `net/http` — no framework, no pgx, **no epistula-database import**.

## Build & run

Same Makefile shape as the other components (`BINARY := epistula-mcp`,
`build/`-namespaced cross-compiles, `make build-freebsd`, ldflags version
stamp), and listed in the root Makefile's `PROJECTS`.

Example Claude Code `.mcp.json` (stdio; secrets via env so none land in the JSON…
which still means treating `.mcp.json` itself as sensitive if you inline the token):

```json
{
  "mcpServers": {
    "mail": {
      "command": "/absolute/path/to/epistula-mcp",
      "env": {
        "MAIL_MCP_BASE_URL": "https://epistula-api.ptudor.invalid",
        "MAIL_MCP_TOKEN": "MINTED_BY_epistula-database_admin_api-token-add"
      }
    }
  }
}
```

Smoke test: pipe an `initialize` → `notifications/initialized` → `tools/list`
JSON-RPC sequence into the binary and confirm the seven tools come back
(`make smoke`; hold stdin open briefly so the SDK flushes before EOF).

## Security posture

- The epistula-api token is the perimeter: least-scope minting, Argon2id-hashed at rest
  upstream, revocable with `epistula-database admin api-token-revoke`.
- Never log tokens, message bodies, summaries, or `Authorization` headers. Log:
  tool name, upstream status, byte counts, durations.
- Upstream error passthrough is bounded: status + problem `title`/`detail`, never a
  raw body dump.
- `-http` refuses non-loopback binds without `http_token`; TLS is Apache's job.
- No SQL surface at all — injection, scope-bypass, and read-role questions are
  epistula-api's, answered once, there.

## Relationship to the rest of the store

epistula-mcp consumes the store but adds no schema reader: it reads through
`epistula-api`, so "one writer, three readers" stays true. llm-worker is the
batch consumer of epistula-api; epistula-mcp is the interactive one.

```
                        ┌──────────────────────────┐
    Postfix ──pipe────► │  epistula-database (writes)   │  owns schema + admin CLI
                        └──────────┬───────────────┘
                                   │ Postgres + blob store
               ┌───────────────────┤
               ▼                   ▼
    ┌────────────────┐   ┌──────────────────┐
    │ epistula-imap  │   │    epistula-api      │
    └────────────────┘   └───────┬──────────┘
                                 │ HTTP + bearer token
                       ┌─────────┴─────────┐
                       ▼                   ▼
              ┌────────────────┐  ┌────────────────┐
              │ epistula-llm-worker│  │   epistula-mcp     │
              │ (batch: export │  │ (interactive:  │
              │  + annotate)   │  │  MCP tools)    │
              └────────────────┘  └────────────────┘
```
