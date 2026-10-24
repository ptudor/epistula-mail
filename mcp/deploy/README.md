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

Port note: the stack already uses 8782 (epistula-database admin), 8783 (epistula-imap admin), 8784/8785 (epistula-api),
and 8790 (llm-worker admin); **8786** is free and is the suggested `-http` port.

## Files

- `freebsd/epistula_mcp` — rc.d script for the `-http` daemon. Copy to
  `/usr/local/etc/rc.d/epistula_mcp`, `chmod 555`, then `sysrc epistula_mcp_enable=YES`
  and `service epistula_mcp start`. Binds `127.0.0.1:8786` by default and requires
  `http_token` for every HTTP listener, including loopback.
- `launchd/net.ptudor.epistula-mcp.plist` — macOS LaunchDaemon for the same `-http`
  daemon. Install steps are in the plist header.

## The epistula-api tokens (the whole security perimeter)

epistula-mcp holds a epistula-api **read** token and, optionally, **separate** tokens
used only by the `annotate` and `send` tools. **Mint dedicated ones — never reuse the
llm-worker's** — each scoped to the least it needs:

```sh
# READ token — note the deliberate absence of write_annotation
epistula-database admin api-token-add -name epistula-mcp-read \
    -mailboxes "jdoe" -permission read_metadata -permission read_content

# WRITE token — only if the annotate tool should exist at all
epistula-database admin api-token-add -name epistula-mcp-annotate \
    -mailboxes "jdoe" -permission write_annotation

# SEND token — only if the send tool should exist at all
epistula-database admin api-token-add -name epistula-mcp-send \
    -mailboxes "jdoe" -permission submit_message
```

Put them in the protected config (`/usr/local/etc/epistula/epistula-mcp.toml`) as `token`,
`annotate_token` and `send_token`, or pass `MAIL_MCP_TOKEN` / `MAIL_MCP_ANNOTATE_TOKEN` /
`MAIL_MCP_SEND_TOKEN`. Each
secret is printed once at mint time. Revoke with
`epistula-database admin api-token-revoke`. Even a bug in this process cannot exceed a
token's scope — epistula-api enforces it at the SQL level.

**Leaving `annotate_token` and `send_token` empty is the default and the recommended
posture.** With no write credential the `annotate` tool is not registered at all, so the connector
advertises seven read tools and nothing that writes. That matters because message
bodies are attacker-controlled: anyone who can email you can put instructions in
one, aimed at the model that reads it. Splitting the credential means the token
exposed to that content cannot write; a single token carrying `write_annotation`
would leave the write reachable from the same session that read the hostile text.

Setting any two of `token`, `annotate_token` and `send_token` to the same value is
rejected at startup — it would read as a split while the read tools still carried
the other scope.

`send` sends real mail as the token's mailboxes, to anyone, through epistula-api's
submission relay, which also needs a `[[submission.accounts]]` entry for the
mailbox (see api/deploy/README.md). Configure `send_token` only for a connector
whose MCP client asks the user before each tool call, and revoke it when sending
is no longer needed. Keep `request_timeout` above epistula-api's
`[submission] timeout` (30s against 20s by default) so the connector hears the
outcome.

The `-http` transport's own `http_token` is a *separate* secret (a bearer gating
the HTTP endpoint, not a mail credential); generate a random one, for example
`openssl rand 24 | base64 | sed 's/[/10lO#+=]//g'`, for every HTTP deployment.
Without this credential, other local processes could use the daemon's mail access.

## Service account and config access

For FreeBSD, create the dedicated account, allow traversal of the shared config
parent, then install a group-readable config. This also works when the database
quickstart created the parent as `root:epistula-database 0750`:

```sh
pw groupadd epistula-mcp
pw useradd epistula-mcp -g epistula-mcp -d /nonexistent -s /usr/sbin/nologin
chmod 755 /usr/local/etc/epistula
install -o root -g epistula-mcp -m 640 epistula-mcp.toml.example /usr/local/etc/epistula/epistula-mcp.toml
```

Set `token` and `http_token` before starting. The rc.d script checks config
readability as the service user and provisions its logfile before dropping
privileges. It does not restart a failed process automatically.

For the macOS LaunchDaemon, create `_epistula-mcp` with an unused user/group ID.
Inspect both lists first, and replace `600` below if that ID is already used:

```sh
dscl . -list /Users UniqueID
dscl . -list /Groups PrimaryGroupID
MCP_ID=600
sudo dscl . -create /Groups/_epistula-mcp
sudo dscl . -create /Groups/_epistula-mcp PrimaryGroupID "$MCP_ID"
sudo dscl . -create /Users/_epistula-mcp
sudo dscl . -create /Users/_epistula-mcp UniqueID "$MCP_ID"
sudo dscl . -create /Users/_epistula-mcp PrimaryGroupID "$MCP_ID"
sudo dscl . -create /Users/_epistula-mcp NFSHomeDirectory /var/empty
sudo dscl . -create /Users/_epistula-mcp UserShell /usr/bin/false
sudo dscl . -create /Users/_epistula-mcp Password '*'
sudo dscl . -create /Users/_epistula-mcp IsHidden 1
```

Then follow the plist's install commands: config `root:_epistula-mcp 0640`, log
`_epistula-mcp:_epistula-mcp 0640` at the existing path, and process umask `077`.
A client-launched stdio process instead uses a client-owned `0600` config.

## Smoke test after install

```sh
# stdio: the SDK handshake should list the seven read tools, plus annotate when an
# annotate_token is configured and send when a send_token is.
MAIL_MCP_BASE_URL=http://127.0.0.1:8784 MAIL_MCP_TOKEN=... make smoke

# -http daemon: point an MCP client at http://127.0.0.1:8786 (Authorization:
# Bearer <http_token>). tools/list should return:
#   mailboxes folders messages message message_text attachment search annotate
```

Omitted, zero and negative tool list/search limits use the API's configured default capped by MCP `max_limit`, via `limit=default:<cap>`. Explicit positive values are clamped normally.

The message tool returns bounded inspection metadata with explicit continuation. Supply `attachment_after`/`annotation_after` from `next_attachment`/`next_annotation` to inspect later parts/models. For a long summary, supply `annotation_model` and that model's `summary_next_offset` as `summary_offset` (UTF-8 bytes). All models remain accessible and primary/ranking information remains available. `metadata_truncated` identifies more attachment/model pages; individual summaries carry their own continuation. Use message_text for remaining body text. API consumers that omit the inspection parameters still get the full document.

The attachment tool fetches one attachment's file by message id and the `part_number` the message tool lists. A PNG, JPEG, GIF or WebP image is returned as an MCP image, a UTF-8 `text/*` file as an embedded text resource, and anything else (PDFs included) as an embedded blob resource with its MIME type. What the model sees of a blob is up to the MCP client: some save it to disk and report the path, others may not pass it to the model at all. Check with your client. A file over `max_attachment_bytes` (default 10 MiB) is refused, not truncated. The read token needs `read_content`, which it already has for message bodies.

For the rc.d defaults, use this `newsyslog.conf` entry to rotate the log and
send HUP (signal 1) to the supervisor, which reopens its output file:

```text
/var/log/epistula-mcp.log epistula-mcp:epistula-mcp 640 7 1000 * Z /var/run/epistula-mcp/supervisor.pid 1
```

Keep the child pidfile for service status and application reloads.
