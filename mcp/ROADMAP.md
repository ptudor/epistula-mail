# Epistula MCP — implementation roadmap

The checklist `epistula-mcp` was built against, to the contract in `CLAUDE.md`.
Every v1 item is done; "Later / maybe" holds what is deliberately left out.

## Scaffold

- [x] `go.mod` — module `github.com/ptudor/epistula-mail/mcp`,
      deps: `github.com/modelcontextprotocol/go-sdk`, `github.com/pelletier/go-toml/v2`.
      **No** `epistula-database` import, no `replace` directive — HTTP contract only.
- [x] `version.go` — `main.Version` / `main.BuildTime` vars (repo convention).
- [x] `Makefile` — copy the epistula-api shape: `BINARY := epistula-mcp`, ldflags stamp,
      `build`, `build-{linux,linux-arm64,darwin,darwin-arm64,freebsd}` into `build/`,
      `test`, `fmt`, `vet`, `check`, `clean`, `help`.
- [x] `main.go` — flags `-config`, `-http`, `-version`; stdio default; clean-EOF
      handling on stdio disconnect (don't report a client disconnect as a crash).
- [x] `.gitignore` — `epistula-mcp`, `build/`, local `*.toml`.
- [x] Skeleton builds and `./epistula-mcp -version` prints the stamp.

## Config & epistula-api client

- [x] `config.go` — TOML shape from CLAUDE.md (`[mailapi] base_url, token,
      request_timeout`; `[mcp] http_token, max_limit, max_text_bytes`); search path
      `/usr/local/etc/epistula/epistula-mcp.toml` → `/etc/epistula/epistula-mcp.toml` →
      `./epistula-mcp.toml`; `-config` overrides.
- [x] Env overrides win over file: `MAIL_MCP_BASE_URL`, `MAIL_MCP_TOKEN`,
      `MAIL_MCP_HTTP_TOKEN` (stdio path must run with zero files).
- [x] Validation: `token` required; `base_url` must parse, must be `https://` unless
      the host is loopback; positive timeout; defaults `request_timeout=30s`,
      `max_limit=200`, `max_text_bytes=65536`.
- [x] Secrets hygiene: config load errors never echo `token`/`http_token`; no secret
      ever enters an error string, log line, or URL.
- [x] `client.go` — one `http.Client` with the total-deadline timeout (no streaming
      endpoints in v1, so no exportClient split); sets
      `Authorization: Bearer <token>`, `Accept: application/json`,
      `User-Agent: epistula-mcp/<version>`.
- [x] RFC 7807 handling: on non-2xx, decode `application/problem+json` and surface
      `status` + `title` + `detail` as a bounded tool error; on undecodable bodies,
      bound the snippet (e.g. 300 bytes). Never dump full upstream bodies.
- [x] No client-side retries — the model (or the human) retries; keep 429/503
      passthrough honest.
- [x] `config_test.go` — precedence (env > file > default), https/loopback rule,
      secret-free error strings, cap defaults.

## Read tools

- [x] `tools.go` — `jsonResult` / `toolErr` helpers (correctable `{"error": …}`
      results, not protocol faults), typed input structs with `jsonschema` tags,
      `register()`.
- [x] Time-argument normalization: absolute RFC 3339 passthrough + relative
      lookbacks (`30m`, `6h`, `2d`, `1w` — Go durations plus day/week
      suffixes) converted to RFC 3339 for epistula-api's `since`/`before`/
      `sent_since`/`sent_before`.
- [x] `limit` clamp to `max_limit`; opaque cursor passthrough (`next_cursor` out,
      `cursor` in, never inspected).
- [x] `mailboxes` — `GET /v1/mailboxes`.
- [x] `folders` — `GET /v1/mailboxes/{m}/folders` (name, uidvalidity, counts).
- [x] `messages` — folder listing with the full filter set (`since`, `before`,
      `sent_since`, `sent_before`, `flag`, `not_flag`, `larger`, `smaller`, `tag`,
      `category`, `annotated_by`, `not_annotated_by`, `cursor`, `limit`). Metadata
      projection only — no inline bodies from the list tool.
- [x] `message` — `GET /v1/messages/{id}`; slim: drop `html_body`, omit
      nulls/empties, cap inline `text_body` at `max_text_bytes` with
      `truncated: true`, keep ranked annotations (`priority`/`primary`) intact.
- [x] `message_text` — `GET /v1/messages/{id}/text`; byte cap + `truncated` flag +
      `offset` argument so the model can continue a long body in slices.
- [x] `search` — `GET /v1/search` (`q` required; `mailbox`, `folder`, `tag`,
      `category`, `since`, `before`, `cursor`, `limit`).
- [x] Tool descriptions written for the model: say `mailboxes`/`folders` first for
      orientation, name the filter vocabulary, state defaults, note that a `403`
      means the token isn't scoped for that mailbox/permission (correct by asking
      about something in scope — not by retrying).
- [x] `tools_test.go` — window parsing, limit clamping, slimming/truncation logic,
      problem+json → tool-error mapping (pure logic, no network).

## The write tool

- [x] `annotate` — `PUT /v1/messages/{id}/annotation` with `{model, tags, category,
      summary, tokens_in, tokens_out}`; require `model` and at least one of
      `tags`/`category`/`summary`; document idempotency on `(message_id, model)`.
- [x] No MCP-side permission knob — epistula-api's `write_annotation` scope is the
      gate; its `403` surfaces as a correctable tool error.
- [x] Test: request-shaping + upstream-403 mapping against the mock.

## Streamable HTTP transport

- [x] `auth.go` — bearer middleware for `-http` (constant-time compare); 401 with
      `WWW-Authenticate: Bearer` on missing/bad token.
- [x] Loopback guard: refuse to bind a non-loopback address without `http_token`
      (fail loud at startup).
- [x] Suggested addr `127.0.0.1:8786` (8782–8785 and 8790 are taken).
- [x] `auth_test.go` — middleware accept/reject, loopback-addr detection.

## Integration tests & smoke

- [x] `httptest` mock of epistula-api (fixtures for each `/v1` route, problem+json
      error cases, cursor pages) — full tool-call round trips through the real
      client without a live store. Matches the repo's integration-test culture
      without needing pgtest (this project has no DB).
- [x] `make smoke` — stdio `initialize` → `notifications/initialized` → `tools/list`
      piped into the binary; assert all seven tools; trailing hold-open so the SDK
      flushes before EOF.
- [x] Verify end-to-end against a live epistula-api with a real minted token
      (isolated test environment): every read tool returns test data; `annotate`
      round-trips and re-`PUT` replaces rather than duplicates; an out-of-scope
      mailbox returns the 403 tool error. Recorded in CLAUDE.md's status line.

## Deploy & repo integration

- [x] `epistula-mcp.toml.example` — commented sample; token-minting recipe
      (`epistula-database admin api-token-add`, least scope, randomly generated
      secrets); **no literal placeholder secrets**.
- [x] `deploy/freebsd/epistula_mcp` — rc.d script under `daemon(8)`, dedicated user,
      `-http 127.0.0.1:8786`, matching the API script's shape.
- [x] `deploy/launchd/net.ptudor.epistula-mcp.plist` — macOS LaunchDaemon (install
      steps in the header).
- [x] `deploy/README.md` — matches the other components' deploy READMEs.
- [x] Root `Makefile`: add `mcp` to `PROJECTS`.
- [x] Root `CLAUDE.md`: add the project to "Projects" and the architecture diagram.
- [x] `.mcp.json` example documented (already sketched in CLAUDE.md) — confirm it
      against the real binary path and an env-injected token.

## Later / maybe (explicitly out of v1)

- [ ] `thread` tool — when epistula-api ships `GET /v1/threads/{message_id}`.
- [ ] `raw` tool — only if a real re-parsing-from-chat need appears; would need a
      hard byte cap and a strong "this is for machines" description.
- [ ] `overview` convenience tool — client-side fan-out of `folders` across all
      visible mailboxes for a one-call "what's the shape of the store" answer;
      skip unless orientation via `mailboxes`+`folders` proves clumsy in practice.
- [ ] Export-backed bulk operations — belongs in llm-worker or a purpose-built job,
      not in a chat connector.
