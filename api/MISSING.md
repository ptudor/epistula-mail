# epistula-api — limitations and future work

## Thread reconstruction

`GET /v1/threads/{message_id}` (reconstructing a thread from `message_id` /
`in_reply_to`) is designed but not implemented. It is medium priority: add it
when a consumer needs it, and epistula-mcp's `thread` tool with it.

## Embeddings and vector search

Out of scope for v1. A `message_embeddings` table would be an
`epistula-database` migration, with a read endpoint here; epistula-api itself
would still make no embedding or LLM calls.

## Acting on `category`

An annotation's `category` stays advisory: epistula-api stores and serves it
but never files mail by it. Physical filing belongs in epistula-imap (core IMAP
MOVE); archive sorting already works that way, with epistula-api storing only
the classifier's choice in `message_classifications`.

## Deployment

Creating the service user and blob-group membership, the least-privilege
Postgres role, the rc.d service, the Apache front (with mTLS for consumers on
other hosts), and the Prometheus scrape of `127.0.0.1:8785/metrics` are
operator tasks; see `QUICKSTART-FREEBSD.md` and `deploy/README.md`.
