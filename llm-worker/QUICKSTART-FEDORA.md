# Fedora Quick Start — epistula-llm-worker

This takes a Fedora GPU host to a **running annotation worker**: LM Studio
serving a local model, the worker binary installed, a scoped `epistula-api` token
in place, a dry-run proving the wiring, and finally a hardened systemd service
annotating mail on an interval.

The worker is a single static Go binary. It has **no database, no blob store,
and no schema of its own** — every read and every annotation write goes through
`epistula-api` over HTTPS. So this guide assumes the mail store
(`epistula-database` + `epistula-api`) is already standing on its own host; here you
only provision the GPU-side consumer.

```text
epistula-llm-worker (this Fedora box)
  ── HTTPS ──► epistula-api  /v1/export                         (read message text)
  ── HTTP  ──► LM Studio http://127.0.0.1:1234/v1/...        (annotate, local)
  ── HTTPS ──► epistula-api  PUT /v1/messages/{id}/annotation    (write the sidecar)
```

> **Scope:** GPU-side only. The mail host owns the schema, the blob store, and
> token lifecycle (`epistula-database admin api-token-*`). Nothing here touches
> Postgres or disk blobs.

## Prerequisites

- Fedora (Workstation or Server), 40+; NVIDIA driver installed if you want GPU
  inference (an RTX 3090 is the reference card).
- Network reach to the `epistula-api` host over HTTPS.
- Shell access on the **mail host** to mint a token (step 1), and root/`sudo`
  on this Fedora box for the rest.

---

## 0. Set variables once — every later block reuses them

Paste into a shell on the **Fedora box** and keep it open.

```sh
SVC_USER="epistula-llm-worker"                               # system account the daemon runs as
SVC_GROUP="epistula-llm-worker"
CONFIG="/etc/epistula/epistula-llm-worker.toml"              # Linux default search path
BIN="/usr/local/bin/epistula-llm-worker"
UNIT="/etc/systemd/system/epistula-llm-worker.service"

MAIL_API_URL="https://epistula-api.example.invalid"          # EDIT: the epistula-api front door
LMSTUDIO_MODEL="qwen/qwen3.6-35b-a3b"                     # EDIT: model to pull + serve
```

---

## 1. Mint a scoped `epistula-api` token — on the MAIL host

Token CRUD lives in `epistula-database`, not here. On the mail host, mint a token
carrying exactly the two permissions this worker needs, scoped to only the
mailboxes the GPU box may read:

```sh
epistula-database admin api-token-add \
  -name llm-worker-3090 \
  -mailboxes "*" \
  -permission read_content \
  -permission write_annotation
```

It prints the secret (`mapi_...`) **once**. Copy it to the Fedora box for
step 4; it is never recoverable. Narrow `-mailboxes` to a comma-separated set
instead of `"*"` if this GPU host should not see every mailbox.

---

## 2. LM Studio — install, pull the model, serve on loopback

Headless `llmster` is the server-friendly path:

```sh
curl -fsSL https://lmstudio.ai/install.sh | bash
~/.lmstudio/bin/lms daemon up
~/.lmstudio/bin/lms get "${LMSTUDIO_MODEL}"
~/.lmstudio/bin/lms server start                  # binds 127.0.0.1:1234
curl -s http://127.0.0.1:1234/v1/models | head    # expect the model listed
```

For autostart across reboots, install LM Studio's headless user unit (it runs
as **your** login account, which owns the GPU session and the `~/.lmstudio`
models — keep it separate from the worker's system account):

```ini
# ~/.config/systemd/user/lmstudio.service  (run: systemctl --user enable --now lmstudio)
[Unit]
Description=LM Studio Server
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStartPre=%h/.lmstudio/bin/lms daemon up
ExecStartPre=%h/.lmstudio/bin/lms load qwen/qwen3.6-35b-a3b --gpu max -c 16384 --yes
ExecStart=%h/.lmstudio/bin/lms server start
ExecStop=%h/.lmstudio/bin/lms daemon down
[Install]
WantedBy=default.target
```

> Enable lingering (`sudo loginctl enable-linger $USER`) so the user unit — and
> thus LM Studio — starts at boot without an interactive login.

**Model sizing for a 24 GB 3090:** `qwen/qwen3.6-35b-a3b` (~20 GB) is the
quality-first single-GPU pick. For faster passes drop to `openai/gpt-oss-20b`
(~12 GB) or `google/gemma-4-26b-a4b` (~15.6 GB). Avoid `gpt-oss-120b` (~65 GB,
beyond one card). If structured JSON gets flaky, keep the worker's schema on and
push `lmstudio.temperature` toward `0.0`.

---

## 3. Build & install the worker binary

The binary is fully static — nothing to install on the GPU box at runtime.
Build it **CGO-disabled** so it uses Go's pure resolver (this keeps the systemd
sandbox in step 6 from needing extra socket families for glibc NSS):

```sh
# On a build host with Go >= 1.25 (go.mod pins the toolchain), from the project root:
CGO_ENABLED=0 make build-linux        # produces ./epistula-llm-worker-linux-amd64
```

Copy it over (your transfer of choice), then on the Fedora box:

```sh
sudo install -m 755 epistula-llm-worker-linux-amd64 "${BIN}"
"${BIN}" version
```

> No Go ≥ 1.25 anywhere? `sudo dnf install -y golang git make` and build on the
> box; with `GOTOOLCHAIN=auto` (the default) `go` fetches the pinned toolchain.
> Build static there too: `CGO_ENABLED=0 make build && sudo install -m 755 epistula-llm-worker "${BIN}"`.

---

## 4. Service account + config

```sh
# Dedicated, unprivileged system account — no home, no shell, no GPU access.
sudo useradd --system --no-create-home --shell /usr/sbin/nologin "${SVC_USER}" 2>/dev/null || true

# Config dir (root-owned) + the config file, mode 0640 so only the worker's
# group can read the embedded token.
sudo install -d -o root -g "${SVC_GROUP}" -m 750 /etc/epistula
sudo install -o root -g "${SVC_GROUP}" -m 640 epistula-llm-worker.toml.example "${CONFIG}"
```

Edit `${CONFIG}` and set at least the three things that have no safe default:

```toml
[mail_api]
base_url = "https://epistula-api.example.invalid"   # = MAIL_API_URL
token    = "mapi_..."                            # the secret from step 1

[lmstudio]
base_url = "http://127.0.0.1:1234/v1"
model    = "qwen/qwen3.6-35b-a3b"                # = LMSTUDIO_MODEL
```

Leave `production = false` and `dry_run = true` for now — step 5 proves the
wiring before anything is written back. The rest of the file (categories,
char caps, retries, the loopback admin listener on `127.0.0.1:8790`) ships with
sane defaults; see `epistula-llm-worker.toml.example` for the full reference.

> Prefer the token out of the file? Drop it from the TOML and instead set
> `MAIL_LLM_MAIL_API_TOKEN=` in a `0600` `EnvironmentFile=` the unit reads. The
> config falls back to that env var.

---

## 5. Validate, then dry-run a small batch

```sh
sudo -u "${SVC_USER}" "${BIN}" check-config -config "${CONFIG}"   # expect: config ok
```

Cap the first pass and keep it read-only. In `${CONFIG}`:

```toml
[worker]
dry_run     = true
batch_limit = 25
```

Then run one pass in the foreground and read the summary line:

```sh
sudo -u "${SVC_USER}" "${BIN}" run-once -config "${CONFIG}"
# logs end with: run-once complete  scanned=.. skipped=.. annotated=.. failed=..
```

`dry_run = true` means `annotated` counts what *would* be written without
calling `PUT .../annotation`. When the categories/tags look right, set
`dry_run = false` (and clear `batch_limit` back to `0` for the full corpus, or
leave a cap while you watch it).

---

## 6. Install the systemd service

The unit ships in `deploy/linux/`. It runs `serve` as `${SVC_USER}` under a
tight sandbox (read-only FS, restricted syscalls, private `/tmp`, outbound TCP
only). `serve` validates the config at startup and exits `78` on a bad one;
`RestartPreventExitStatus=64 78` keeps that exit down (`failed`, no restart
loop) while transient failures still restart under `on-failure`. There is
deliberately no `check-config` `ExecStartPre` — systemd matches
`RestartPreventExitStatus` only against the main process, so a failing
`ExecStartPre` would crash-loop anyway. Run `check-config` by hand after
editing the TOML (step 4).

```sh
sudo install -m 644 deploy/linux/epistula-llm-worker.service "${UNIT}"
sudo restorecon -v "${BIN}"                      # label the binary for SELinux
sudo systemctl daemon-reload
sudo systemctl enable --now epistula-llm-worker

systemctl status epistula-llm-worker --no-pager
curl -s http://127.0.0.1:8790/healthz            # expect: ok
curl -s http://127.0.0.1:8790/status             # running / last_started / last_error
curl -s http://127.0.0.1:8790/metrics | grep messages_annotated
journalctl -u epistula-llm-worker -f                 # watch passes (no message bodies are logged)
```

`serve` runs a round every `worker.interval_seconds` (default 900s). Most are
fast rounds over the messages epistula-database queued for the pipeline; on start and
every `worker.complete_round_interval_seconds` (default 6 h) a round is a complete
sweep instead (CLAUDE.md, "Fast and complete rounds"). A round that can't reach
LM Studio or epistula-api is logged and retried next tick — the service stays up.

---

## 7. Going to production

Once a dry-run and a few real passes look right, harden the config:

```toml
# In ${CONFIG}:
production = true
[worker]
dry_run = false
```

`production = true` enforces, at startup (and via `check-config`):

- `mail_api.base_url` must be **https** (unless it's a loopback host).
- `admin.listen_addr` must stay **loopback** — never expose the metrics/status
  listener off-box.
- `lmstudio.api_token` is **required if LM Studio is not loopback**. Keep LM
  Studio on `127.0.0.1`; if you must bind it to the LAN, turn on LM Studio's API
  authentication and set `lmstudio.api_token`.

```sh
sudo -u "${SVC_USER}" "${BIN}" check-config -config "${CONFIG}"
sudo systemctl restart epistula-llm-worker
```

---

## Troubleshooting

**`config error: ...` on start** — `serve` validated the config at startup and
exited `78` (the unit stays `failed` without restart-looping); the message
names the offending key. Re-run the check by hand:
```sh
sudo -u "${SVC_USER}" "${BIN}" check-config -config "${CONFIG}"
```

**epistula-api returns 401 / 403** — 401 is a bad/expired token (re-mint on the mail
host, step 1); 403 is an out-of-scope mailbox — the token isn't scoped to what
the export filter asked for, or lacks `read_content`/`write_annotation`.

**LM Studio unreachable** — `curl http://127.0.0.1:1234/v1/models`. If empty,
`~/.lmstudio/bin/lms server start` and confirm the model is loaded. The worker
keeps serving and retries; check `mail_llm_worker_runs_failed_total`.

**Reasoning models (Qwen3.x) and empty `content`** — handled automatically, no
action needed. A reasoning model constrained by the strict JSON schema returns
an empty `message.content` and places the annotation JSON in
`message.reasoning_content`; the worker prefers `content`, falls back to the
reasoning channel, and tolerates a JSON object wrapped in `<think>`/markdown
fences. The API-level `/no_think` and `chat_template_kwargs:{enable_thinking:false}`
switches are ignored by current LM Studio builds, so don't rely on them. To skip
the few extra reasoning tokens per message, pick a non-reasoning model
(`gpt-oss-20b`, `gemma`), which returns the JSON in `content` directly.

**SELinux denials** — Fedora runs SELinux enforcing. A static binary in
`/usr/local/bin` should run fine after `restorecon`; if a pass is blocked,
inspect: `sudo ausearch -m avc -ts recent`. Outbound TCP and the loopback
listener are normally allowed without a custom policy module.

**Sandbox blocks DNS / network** (only if built with CGO) — the unit restricts
socket families to `AF_INET AF_INET6`. A CGO build resolves via glibc NSS and
may also need `AF_UNIX`/`AF_NETLINK`. The fix is to build static
(`CGO_ENABLED=0`, step 3) so Go's pure resolver is used; don't widen the
sandbox.

---

## Useful commands

```sh
systemctl {status,restart,stop} epistula-llm-worker
journalctl -u epistula-llm-worker -f

# One-off pass outside the timer (honors dry_run/batch_limit in the config):
sudo -u epistula-llm-worker /usr/local/bin/epistula-llm-worker run-once -config /etc/epistula/epistula-llm-worker.toml

# Observability (loopback):
curl -s http://127.0.0.1:8790/healthz
curl -s http://127.0.0.1:8790/status
curl -s http://127.0.0.1:8790/metrics
```
