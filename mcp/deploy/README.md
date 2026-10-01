# Epistula — Deployment artifacts for epistula-mcp

epistula-mcp is a thin MCP proxy over [epistula-api](../../api/)'s `/v1` HTTP contract. It has no
database, no schema import, and no LLM calls — its only outbound dependency is a
reachable epistula-api and a bearer token. Two transports:

| Mode              | Lifecycle    | Surface                                                       |
|-------------------|--------------|--------------------------------------------------------------|
| stdio (default)   | per-client   | MCP client launches the binary as a subprocess (stdin/stdout) |
| `-http <addr>`    | long-running | Streamable HTTP on `127.0.0.1:8786` (suggested), bearer-gated |

Prefer **stdio** for a local Claude Code / Desktop / claude.ai connection — the
client launches the binary and injects `MAIL_MCP_BASE_URL` / `MAIL_MCP_TOKEN` via
env, so no config file is needed (see the `.mcp.json` example in `../CLAUDE.md`).
Use **`-http`** only for a persistent daemon or to give a remote MCP host one
endpoint.

Port note: the stack already uses 8782/8783 (epistula-imap), 8784/8785 (epistula-api),
and 8790 (llm-worker admin); **8786** is free and is the suggested `-http` port.

## Files

- `freebsd/epistula_mcp` — rc.d script for the `-http` daemon. Copy to
  `/usr/local/etc/rc.d/epistula_mcp`, `chmod 555`, then `sysrc epistula_mcp_enable=YES`
  and `service epistula_mcp start`. Binds `127.0.0.1:8786` by default (loopback, so
  `http_token` is optional); it refuses to bind a non-loopback `epistula_mcp_addr`
  without `http_token` set in the config.
- `launchd/net.ptudor.epistula-mcp.plist` — macOS LaunchDaemon for the same `-http`
  daemon. Install steps are in the plist header.

## The epistula-api tokens (the whole security perimeter)

epistula-mcp holds a epistula-api **read** token and, optionally, a **separate** token
used only by the `annotate` tool. **Mint dedicated ones — never reuse the
llm-worker's** — each scoped to the least it needs:

```sh
# READ token — note the deliberate absence of write_annotation
epistula-database admin api-token-add -name epistula-mcp-read \
    -mailboxes "jdoe" -permission read_metadata -permission read_content

# WRITE token — only if the annotate tool should exist at all
epistula-database admin api-token-add -name epistula-mcp-annotate \
    -mailboxes "jdoe" -permission write_annotation
```

Put them in the `0600` config (`/usr/local/etc/epistula/epistula-mcp.toml`) as `token`
and `annotate_token`, or pass `MAIL_MCP_TOKEN` / `MAIL_MCP_ANNOTATE_TOKEN`. Each
secret is printed once at mint time. Revoke with
`epistula-database admin api-token-revoke`. Even a bug in this process cannot exceed a
token's scope — epistula-api enforces it at the SQL level.

**Leaving `annotate_token` empty is the default and the recommended posture.** With
no write credential the `annotate` tool is not registered at all, so the connector
advertises six read tools and nothing that writes. That matters because message
bodies are attacker-controlled: anyone who can email you can put instructions in
one, aimed at the model that reads it. Splitting the credential means the token
exposed to that content cannot write; a single token carrying `write_annotation`
would leave the write reachable from the same session that read the hostile text.

Setting `token` and `annotate_token` to the same value is rejected at startup — it
would read as a split while the read tools still carried write scope.

The `-http` transport's own `http_token` is a *separate* secret (a bearer gating
the HTTP endpoint, not a mail credential); generate a random one, for example
`openssl rand 24 | base64 | sed 's/[/10lO#+=]//g'`, when binding anything but
loopback.

## Smoke test after install

```sh
# stdio: the SDK handshake should list all seven tools.
MAIL_MCP_BASE_URL=http://127.0.0.1:8784 MAIL_MCP_TOKEN=... make smoke

# -http daemon: point an MCP client at http://127.0.0.1:8786 (Authorization:
# Bearer <http_token> if set). tools/list should return:
#   mailboxes folders messages message message_text search annotate
```

Omitted, zero and negative tool list/search limits use the API's configured default capped by MCP `max_limit`, via `limit=default:<cap>`. Explicit positive values are clamped normally.

The message tool returns bounded inspection metadata with explicit continuation. Supply `attachment_after`/`annotation_after` from `next_attachment`/`next_annotation` to inspect later parts/models. For a long summary, supply `annotation_model` and that model's `summary_next_offset` as `summary_offset` (UTF-8 bytes). All models remain accessible and primary/ranking information remains available. `metadata_truncated` identifies more attachment/model pages; individual summaries carry their own continuation. Use message_text for remaining body text. API consumers that omit the inspection parameters still get the full document.
