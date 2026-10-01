# llm-worker

`epistula-llm-worker` is Epistula's GPU-side batch worker for local LLM annotation. It runs well on Windows or Fedora because it is a single Go binary, while LM Studio provides the local model server.

## Flow

```text
epistula-llm-worker on RTX 3090 host
  -> HTTPS epistula-api /v1/export
  -> local LM Studio http://127.0.0.1:1234/v1/chat/completions
  -> HTTPS epistula-api PUT /v1/messages/{id}/annotation
```

Use `epistula-api`, not IMAP, for this. IMAP would force stateful mailbox walks and per-message FETCHes; `epistula-api` already has decoded text, cursor-safe NDJSON export, service-token scoping, and the annotation sidecar.

## First Setup

1. Mint a `epistula-api` token on the mail host:

```bash
epistula-database admin api-token-add \
  -name llm-worker-3090 \
  -mailboxes "*" \
  -permission read_content \
  -permission write_annotation
```

2. Copy `epistula-llm-worker.toml.example` to a private config file and set:

```toml
[mail_api]
base_url = "https://epistula-api.example.invalid"
token = "mapi_..."

[lmstudio]
base_url = "http://127.0.0.1:1234/v1"
model = "qwen/qwen3.6-35b-a3b"
```

3. Start with a dry run and a small batch:

```toml
[worker]
dry_run = true
batch_limit = 25
```

4. Validate and run:

```bash
epistula-llm-worker check-config -config ./epistula-llm-worker.toml
epistula-llm-worker run-once -config ./epistula-llm-worker.toml
```

5. Set `dry_run = false` when the labels look right.

## LM Studio on Windows

Install either the desktop app from `https://lmstudio.ai/download` or headless `llmster` from PowerShell:

```powershell
irm https://lmstudio.ai/install.ps1 | iex
lms daemon up
lms get qwen/qwen3.6-35b-a3b
lms server start
curl http://localhost:1234/v1/models
```

For the desktop app, use the Developer tab, start the server, and leave it bound to localhost. If you expose it on the LAN, enable API token authentication in Server Settings and put that token in `lmstudio.api_token`.

## LM Studio on Fedora

For a server-style Fedora GPU box, use `llmster`:

```bash
curl -fsSL https://lmstudio.ai/install.sh | bash
~/.lmstudio/bin/lms daemon up
~/.lmstudio/bin/lms get qwen/qwen3.6-35b-a3b
~/.lmstudio/bin/lms server start
curl http://localhost:1234/v1/models
```

For autostart, adapt the systemd unit from LM Studio's headless docs:

```ini
[Unit]
Description=LM Studio Server

[Service]
Type=oneshot
RemainAfterExit=yes
User=YOUR_USERNAME
Environment="HOME=/home/YOUR_USERNAME"
ExecStartPre=/home/YOUR_USERNAME/.lmstudio/bin/lms daemon up
ExecStartPre=/home/YOUR_USERNAME/.lmstudio/bin/lms load qwen/qwen3.6-35b-a3b --yes
ExecStart=/home/YOUR_USERNAME/.lmstudio/bin/lms server start
ExecStop=/home/YOUR_USERNAME/.lmstudio/bin/lms daemon down

[Install]
WantedBy=multi-user.target
```

## Model Advice for RTX 3090

Start with `qwen/qwen3.6-35b-a3b`. LM Studio lists it as a 20.40 GB model, Apache 2.0 licensed, with tool use, vision input, and reasoning. On a 24 GB RTX 3090 it should be the quality-first single-GPU choice for summary/tag/category annotation, though you may need modest context sizes.

For faster first passes, use `openai/gpt-oss-20b` or `google/gemma-4-26b-a4b`. LM Studio lists `gpt-oss-20b` at 12 GB with structured-output and reasoning support, and Gemma 4 26B-A4B at 15.60 GB with long-context reasoning. If structured JSON output gets flaky with any model, keep the worker's JSON schema enabled and reduce `temperature` toward `0.0`.

Avoid `openai/gpt-oss-120b` on a 3090; LM Studio lists it at 65 GB, which is beyond a single 24 GB card.
