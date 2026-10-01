# Epistula — llm-worker

A remote GPU annotation worker for the Postgres/blob mail store. It connects to `api`, streams message text from `/v1/export`, sends each message to LM Studio's OpenAI-compatible chat-completions endpoint with a JSON schema, and writes the resulting summary/tags/category back through `PUT /v1/messages/{id}/annotation`.

## Boundary

Use `epistula-api`, not IMAP, for this daemon. IMAP is for interactive clients and mailbox state. Bulk LLM processing wants decoded `messages.text_body`, cursor-safe export, token scoping, and the annotation sidecar, all of which are already the `epistula-api` contract.

The worker never connects to Postgres, never reads blob files, never moves messages, never changes flags, and never stores secrets outside its TOML/environment. All message access and annotation writes are mediated by `epistula-api`.

## Multiple models / multiple GPU boxes

Annotations are keyed `(message_id, model)` in the store, so each model holds its
own summary/tags/category and they never overwrite each other. Run a second box
by installing this worker there with a different `lmstudio.model` (e.g.
`openai/gpt-oss-20b`) and its own scoped `epistula-api` token — no coordination is
needed:

- `worker.annotation_model` (default `lmstudio:<model>`) is the per-model key. Keep
  it distinct per model/version so a re-summarization is a new alternative, not an
  overwrite.
- `skip_annotated = true` skips only *this* model's already-done messages, and the
  worker sends `not_annotated_by=<annotation_model>` to `/v1/export` so the server
  streams only this box's undone work.
- Rank the models with `epistula-database admin annotation-model-set -model <m>
  -priority <n>`; `epistula-api` then serves each message's annotations
  highest-priority-first with the winner flagged `primary`, the rest retained as
  alternatives.

## Archive classification (`[classify]`)

With `[classify] enabled = true`, a mailbox that has archive categories
(`../ARCHIVE_SORTING.md`; served by `GET /v1/mailboxes/{m}/archive-categories`)
gets its category chosen in the SAME model request as its annotation. The
schema's `archive_category` is an enum over the mailbox's keys, plus
`archive_confidence` from 0 to 1. The worker writes it to
`PUT /v1/messages/{id}/classification`, after the annotation. A second pass
per such mailbox then streams `not_classified=true` and asks for the category
alone (`classify.max_body_chars` of text). That covers messages annotated
before classification was on, or classified under a key since retired. A
mailbox without categories is annotated exactly as before. Each mailbox's list
is re-fetched every 10 minutes, so a re-import (`admin archive-category-import`)
takes effect during a days-long backfill without a restart. A failed refresh
keeps the list already held.

The token needs `write_classification` too. The worker still moves nothing:
epistula-imap files a message by its classification only after its owner
archives it. If the category endpoint is unavailable, annotation goes ahead
without categories, and the pass reports the failure so it is not mistaken for
a clean one.

## Context window and thinking

A prompt longer than the loaded model's context window comes back from LM
Studio as a 400. The worker reads that error body only to recognise the
refusal (never into a log or an error; it may echo the mail). It then retries
the message with half as much of the body, down to 2,000 characters, instead
of repeating the request. URL-heavy newsletters and non-Latin text run at
about 1.3 characters a token, so `max_body_chars` that suits most mail can
overflow for them.

`lmstudio.reasoning_effort = "none"` requests that Qwen3 disable thinking.
Support depends on the model and server configuration. Without it, Qwen3
under a strict schema can return its answer in
`reasoning_content` with `content` empty. The decoder accepts that too.

## A cut export stream is reopened

Behind reverse proxy and Apache, a days-long `/v1/export` gets cut: the proxies
buffer about an hour ahead of a 1.5 s/message worker, the backend connection
idles behind the buffer, and Apache's route timeout (1 h) closes it. The
worker drains the buffered rows and then reads an unexpected EOF. When that
happens after the stream delivered rows, and the filter excludes finished work
(`not_annotated_by`, `not_classified`), the worker opens a fresh export in the
same pass, which resumes rather than repeats. At most 50 reopens per pass;
`mail_llm_worker_stream_reconnects_total` counts them.

## Fast and complete rounds

`not_annotated_by` and `not_classified` ask about an absence, so epistula-api answers
them by probing every message whether there is work or not. Epistula's database
migration 022 queues every message
in `annotation_pass_required` in the transaction that stores it (IMAP COPY and
MOVE carry the marker), so `serve` alternates two rounds:

- A **fast round** (every `interval_seconds`) runs the same two passes with
  `pass_required=true`, which epistula-api reads through the queue: its cost is what
  is queued. It ends with `POST /v1/pass-required/prune`,
  which clears the markers of messages this model has annotated and, with
  `[classify]`, classified; anything unfinished stays queued. A message that
  fails on its own content is deferred, 5 minutes doubling per failure up to the
  complete-round interval, so a message the model cannot handle does not cost a
  model call every interval.
- A **complete round** (on start, and every `complete_round_interval_seconds`,
  default 6 h) is the full sweep as before. It finds what no insert queues: a new
  `annotation_model`, a category retired and re-imported, mail stored before
  migration 022, deferred messages. It prunes too, and clears the deferrals.

`complete_round_interval_seconds = 0` makes every round complete. A epistula-api
without the queue answers `pass_required` with its own 503; the worker then runs
complete rounds, tries the queue again after each successful one, and logs the
loss once. `run-once` runs one complete round.
`mail_llm_worker_complete_rounds_total`, `..._pass_markers_pruned_total` and the
`..._pass_queue_remaining` gauge show the rounds at work.

## Coverage guarantee — why `skip_annotated` is load-bearing

`/v1/export` is a **snapshot-free stream**. Its keyset cursor is
`(internal_date, id)` with `ORDER BY internal_date DESC, id DESC`, which is a
total order (id is unique), so a single pass never loses or duplicates a row —
including when many messages share one `internal_date`, which is verified in
`epistula-api`'s `TestExportCursorHandlesInternalDateTies`.

What a pass does **not** see is concurrent delivery. A message inserted with an
`internal_date` older than the cursor's current position is skipped by the
in-flight export, and one inserted newer than the pass started is never
reached. That is expected for a stream with no snapshot — but the worker treats
a completed pass as "everything is annotated", so something has to close the
gap on the next pass.

**`skip_annotated = true` is that something.** With it set, the worker sends
`not_annotated_by=<annotation_model>` and epistula-api streams only this model's
undone work, so anything a previous pass missed is picked up by the next one.

**If `skip_annotated` is turned off, the gap becomes permanent**: each pass
re-streams from the beginning and re-annotates everything it sees, but a
message that was invisible to every pass it raced is never singled out — there
is no "what did I miss" query, only "everything, again". Leave it on unless you
are deliberately re-annotating a corpus in one shot on a quiet store
(RO5X-046).

## Commands

```bash
epistula-llm-worker check-config -config ./epistula-llm-worker.toml
epistula-llm-worker run-once      -config ./epistula-llm-worker.toml
epistula-llm-worker serve         -config ./epistula-llm-worker.toml
```

`serve` runs a round every `worker.interval_seconds`, fast or complete (see above). `run-once` ignores the interval and exits after one complete round.

## Security

- The `epistula-api` token must be scoped to only the mailboxes the GPU host may read and must carry `read_content` plus `write_annotation` (and `write_classification` when `[classify]` is enabled).
- Use HTTPS or a private tunnel from the GPU workstation to `epistula-api`.
- LM Studio should normally stay on `127.0.0.1` because this worker runs on the same GPU box. If LM Studio binds to the LAN, enable LM Studio API authentication and set `lmstudio.api_token`. In **production mode** an off-box (non-loopback) `lmstudio.base_url` must additionally be `https` (both the token and the full message text would otherwise cross the LAN in cleartext). LM Studio has no native TLS, so front it with a tunnel/reverse proxy that terminates https and point `base_url` at that endpoint — loopback `http` stays allowed. `check-config` rejects a non-loopback `http` LM Studio when `production = true`.
- The prompt includes message text. Logs must not include message bodies or summaries; current logs only include ids, counts, labels, and error classes.

## Build

```bash
make build
make build-linux
make build-windows
make test
```
