# Epistula architecture and reading guide

Epistula coordinates through PostgreSQL and an immutable blob store. The database
component owns schema migrations and the shared ingest path. Delivery writes and
syncs the original message and attachment blobs before committing their database
references. Per-mailbox tenant trees and date buckets provide storage isolation.

Read [database/storage/ingest.go](../database/storage/ingest.go) alongside
[database/blob/blob.go](../database/blob/blob.go) for the durability contract,
and [database/ingest](../database/ingest) for MIME parsing, decoded projections,
and bounded recovery of malformed mail. [database/migrations](../database/migrations)
contains the incremental schema and its embedded migration runner.

The IMAP server shares these packages for APPEND and maintains transactional
mailbox state for flags, copies, moves, quotas, UIDs, and expunge. Start with
[imap/imapsess/session.go](../imap/imapsess/session.go), then selection, view,
mutations, and fetch streaming. Raw FETCH preserves stored message bytes;
the API's text projection is a separate consumer representation.

The HTTP API authenticates scoped bearer tokens, filters access by durable
mailbox IDs, and exposes metadata, text, raw content, cursor pagination,
search, and NDJSON export. Begin with [api/authz.go](../api/authz.go) and
[api/handlers.go](../api/handlers.go). Annotation and classification permissions
are separate. The API leaves IMAP message-state operations to IMAP.

The worker consumes API exports and writes model-specific sidecars. Its fast
rounds read a pass-required queue; complete rounds find older or deferred work.
Read [llm-worker/worker.go](../llm-worker/worker.go) and
[llm-worker/schedule.go](../llm-worker/schedule.go). Model errors, export idle
timeouts, context limits, and reconnects have explicit handling.

The MCP connector is an API proxy. It holds separate read and optional annotation
credentials, caps results, and registers the write tool only when its credential
exists. Read [mcp/tools.go](../mcp/tools.go) with [mcp/client.go](../mcp/client.go).

The archive package combines approved categories, annual folders, live sorting,
and planned reorganization with undo. [ARCHIVE_SORTING.md](../ARCHIVE_SORTING.md)
is the design and operator runbook. Sorting and Trash purge are opt-in.
