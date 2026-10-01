# Epistula

Epistula provides Go services around a PostgreSQL database and content-addressed blob store. The database component owns the schema; IMAP and API share it, while MCP and the worker consume the API.

Epistula starts with a fresh, file-by-file 1.0 baseline. See `README.md` for
installation and `docs/baseline.md` for the history convention. These retained
engineering notes describe the implementation and its component boundaries.

## Projects

### database (the writer)
Postfix LDA that ingests RFC 5322 mail into the Postgres-backed store. **Owns the schema and the parsing/ingest code path.** Runs in `deliver` mode (Postfix pipe transport) plus a `serve` mode exposing a loopback HTTP listener for metrics/health. Reject rules are exported to Postfix via `epistula-database admin reject-export`.

### imap (interactive reader)
Read-mostly IMAP4 server over the same store. Shares the schema (owned by epistula-database) and vendors its ingest path for the one write operation IMAP supports: `APPEND`. Owns all IMAP message-state changes (moving, flagging, expunging), including archive sorting's two server-side jobs: the live sorter that files what users archive and the Trash purge that destroys what they delete (`[archive]`, off by default; see `ARCHIVE_SORTING.md`).

### api (programmatic reader)
Read-mostly HTTP/JSON API — the third reader of the schema — built for programmatic consumers: LLM summarization/tagging, indexing, analytics, archival export, ad-hoc search. Its writes are derived sidecars: the annotation (tags, advisory sort category, optional summary) and the archive classification (one key from the mailbox's approved category list, under the separate `write_classification` permission). It performs no IMAP message-state changes.

### llm-worker
Remote GPU annotation worker. Connects to epistula-api, streams message text from `/v1/export`, sends each message to LM Studio's OpenAI-compatible chat-completions endpoint with a JSON schema, and writes the resulting summary/tags/category back through `PUT /v1/messages/{id}/annotation`. With `[classify]` enabled it also chooses each message's archive category in the same request and writes it through `PUT /v1/messages/{id}/classification`.

### mcp (interactive reader over MCP)
A thin [MCP](https://modelcontextprotocol.io) server that exposes the mail store to an MCP client (Claude Desktop / Claude Code / claude.ai). It is a **proxy over epistula-api's `/v1` HTTP contract** — no database driver, no schema import, no `replace` directive, no LLM calls. Seven tools (`mailboxes`, `folders`, `messages`, `message`, `message_text`, `search`, `annotate`) turn into one epistula-api request each and slim the response for a model. Its capability is its token: it holds a least-scope epistula-api bearer token, so even a bug here cannot exceed what epistula-api's SQL-level scope enforcement allows. epistula-llm-worker is the *batch* consumer of epistula-api; epistula-mcp is the *interactive* one.

## Cross-project contract

`imap/go.mod` and `api/go.mod` both contain:

```
replace github.com/ptudor/epistula-mail/database => ../database
```

The sibling directory layout is load-bearing — don't rename or move the project dirs without updating those replace directives. Blobs are immutable and every reader coordinates through the database.

`mcp` deliberately has **no** `replace` directive and no `epistula-database` import: it depends only on epistula-api's stable `/v1` HTTP contract, so a schema migration never triggers a lockstep rebuild there.

```
┌──────────────┐    ┌─────────────────────────┐    ┌─────────────────────────┐
│   Postfix    │───▶│    epistula-database    │───▶│  Postgres + blob store  │
│  (receives)  │    │  (deliver → Postgres)   │    │                         │
└──────────────┘    └─────────────────────────┘    └────────────┬────────────┘
                                                                │ read
┌──────────────┐    ┌─────────────────────────┐                 │
│ IMAP clients │───▶│      epistula-imap      │◀────────────────┤
└──────────────┘    └─────────────────────────┘                 │
┌──────────────┐    ┌─────────────────────────┐                 │
│ LLM pipeline │───▶│      epistula-api       │◀────────────────┘
│ (llm-worker) │    │  (read/search/export +  │
└──────────────┘    │   annotation sidecar)   │
┌──────────────┐    └────────────▲────────────┘
│  MCP client  │                 │ HTTP + bearer token (/v1)
│   (Claude)   │                 │
└──────┬───────┘                 │
       │ MCP (stdio/HTTP)        │
       ▼                         │
┌─────────────────────────┐      │
│      epistula-mcp       │──────┘  thin proxy: MCP tools → /v1, no DB
└─────────────────────────┘
```

## Common patterns

| Pattern | Implementation |
|---------|----------------|
| **Logging** | Structured logging via `log/slog` (JSON or text) |
| **Metrics** | Prometheus + expvar endpoints |
| **Graceful Shutdown** | SIGINT/SIGTERM with request draining |
| **Configuration** | TOML config file (preferred) or environment variables |
| **Health Checks** | `/health` and `/healthz` endpoints |
| **Database** | PostgreSQL via `github.com/jackc/pgx/v5` |
| **Password hashing** | Argon2id via `golang.org/x/crypto` |
| **Errors** | RFC 7807 `application/problem+json` (APIs); sysexits codes (LDA path) |

Default TOML paths are `/usr/local/etc/epistula/{project}.toml` and `/etc/epistula/{project}.toml`.

## Build commands

The root Makefile iterates every project; each project Makefile has the same targets individually.

```bash
make build              # Build all for current platform
make build-freebsd      # FreeBSD amd64
make build-linux        # Linux amd64
make build-darwin-arm64 # macOS Apple Silicon
make test               # Run tests in all projects
make clean              # Remove build artifacts
make deps               # go mod tidy in all projects
```

## Archive sorting

`ARCHIVE_SORTING.md` is the design and runbook for Archive-as-default: the
approved category list (epistula-database migration 020, `admin
archive-category-*`), the classifier (epistula-llm-worker `[classify]`), the
live sorter and Trash purge (epistula-imap `[archive]`), and the one-time
reorganization (`admin archive-plan / archive-apply / archive-undo`). Shared
logic lives in epistula-database's `archive` package and `storage/archive.go`.

See each project's own `CLAUDE.md` and README for deployment details.
