# LM Studio Getting Started

This worker should use `epistula-api`, not IMAP:

```text
epistula-llm-worker -> epistula-api /v1/export
epistula-llm-worker -> LM Studio /v1/chat/completions
epistula-llm-worker -> epistula-api PUT /v1/messages/{id}/annotation
```

## Mail API Token

Mint the mail token on the mail host:

```bash
epistula-database admin api-token-add \
  -name llm-worker-3090 \
  -mailboxes "*" \
  -permission read_content \
  -permission write_annotation
```

Use narrower `-mailboxes` scope instead of `"*"` if the GPU host should only process selected mailboxes.

## Windows

PowerShell headless install:

```powershell
irm https://lmstudio.ai/install.ps1 | iex
lms daemon up
lms get qwen/qwen3.6-35b-a3b
lms server start
curl http://localhost:1234/v1/models
```

Desktop path: install LM Studio from the official download page, open the Developer tab, start the server, and keep it on `localhost:1234` unless LAN access is explicitly needed.

## Fedora

Headless install:

```bash
curl -fsSL https://lmstudio.ai/install.sh | bash
~/.lmstudio/bin/lms daemon up
~/.lmstudio/bin/lms get qwen/qwen3.6-35b-a3b
~/.lmstudio/bin/lms server start
curl http://127.0.0.1:1234/v1/models
```

Then configure the worker from:

```bash
epistula-llm-worker.toml.example
QUICKSTART-FEDORA.md
```

Start with a dry run:

```toml
[worker]
dry_run = true
batch_limit = 25
```

Validate and run one pass:

```bash
epistula-llm-worker check-config -config ./epistula-llm-worker.toml
epistula-llm-worker run-once -config ./epistula-llm-worker.toml
```

Set `dry_run = false` only after the first labels and summaries look right.

## Model Recommendations for RTX 3090

Primary choice:

```bash
lms get qwen/qwen3.6-35b-a3b
```

LM Studio listed `qwen/qwen3.6-35b-a3b` at 20.40 GB when these notes were written. It is the quality-first fit for a 24 GB RTX 3090, assuming modest context sizing.

Faster or cheaper first passes:

```bash
lms get openai/gpt-oss-20b
lms get google/gemma-4-26b-a4b
```

LM Studio listed `openai/gpt-oss-20b` at 12 GB and `google/gemma-4-26b-a4b` at 15.60 GB when these notes were written.

Avoid `openai/gpt-oss-120b` on one RTX 3090; LM Studio listed it at 65 GB.

Keep `temperature` low, around `0.0` to `0.2`, for summary/tag/category consistency. The worker uses LM Studio structured output via JSON schema.

## Useful Official References

- https://lmstudio.ai/download
- https://lmstudio.ai/docs/developer/core/headless
- https://lmstudio.ai/docs/developer/core/server
- https://lmstudio.ai/docs/developer/core/authentication
- https://lmstudio.ai/docs/developer/openai-compat/structured-output
- https://lmstudio.ai/models/qwen3.6
- https://lmstudio.ai/models/gpt-oss
- https://lmstudio.ai/models/gemma-4
