# Database operations

The physical layout of the mail store, what each part costs, which reader
needs it, and how to keep it that way. Numbers are from a production store on
2026-11-02 (PostgreSQL 16.15, 630,391 messages, 13 GB) unless stated; the SQL
that produced them is at the end so the next reading is comparable. The
schema is owned by epistula-database (`database/schema.sql`); this document
explains the shape, the migrations carry the change history.

## Where the bytes are

`messages` is the database: 12 GB of the 13 GB total. Of it, before
migrations 027–030:

| Part | Size | Read by |
|---|---|---|
| heap (the row proper) | 814 MB | everything |
| `html_body` (TOAST) | 2,946 MB | nothing in production: epistula-api's single-message document offered it, its only consumer (epistula-mcp) asked for `html=false`; IMAP SEARCH BODY LIKEd over it |
| `fts` stored tsvector (TOAST) | 2,603 MB | nothing: every query used the GIN index; the column was consulted only for a lossy-page recheck no `plainto_tsquery` needs |
| `idx_messages_fts` GIN | 2,127 MB | `/v1/search`, `q=` on `/v1/export` |
| `headers` JSONB (TOAST) | 1,259 MB | ENVELOPE (Sender/Reply-To/Bcc), the API document, IMAP SEARCH HEADER/TEXT |
| `text_body` (TOAST) | 1,244 MB | the LLM pipeline, `/text`, IMAP SEARCH, the search index |
| `idx_messages_headers` GIN | 741 MB | nothing: 0 scans since the cluster was created |
| `bodystructure` JSONB | 186 MB | FETCH BODYSTRUCTURE |
| other indexes | ~330 MB | see the inventory |

The raw message is also on disk as a blob (45 KB average), deduplicated by
content hash. Every column above is a projection of it; the question for each
is whether a reader needs it *as a column*.

### What changed and why

**The search vector is an expression index (027).** `mail_fts(subject,
text_body)` is an IMMUTABLE SQL function; `idx_messages_search` is a GIN over
it; queries say `mail_fts(m.subject, m.text_body) @@ plainto_tsquery(...)` and
the planner inlines the function on both sides and matches. Same plan as
before (a bitmap index scan; 226 ms for a 4,388-hit query, unchanged), 2.6 GB
less TOAST, and one definition of what search covers instead of a column
expression and a comment agreeing with each other. The cost moved: a non-HOT
update of a row (every flag change, since `mod_seq` is indexed) recomputes the
vector for the new GIN entries instead of copying it, about a hundred
microseconds for the median 3.7 KB body. `fts_index_match_test.go` asserts
that the predicate the readers write matches the index.

**`html_body` is derived, not stored (030).** The one judgment call in this
set. Kept: the API contract (`GET /v1/messages/{id}` still returns
`html_body` by default, byte-for-byte what ingest produced, from one blob
read and parse; `html=false` skips it, and epistula-mcp always sends that).
Changed: IMAP `SEARCH BODY`/`TEXT` match the text projection only. For the
0.2% of messages that are HTML-only, `text_body` *is* the HTML's text, so a
search of what the message says still finds it; what is lost is a word that
appears only in the HTML alternative of a multipart/alternative message, or
only in markup or a link target — which is also what made `BODY "div"` match
most of the folder. Not done: storing a second text projection of the HTML
(nothing reads it) or a per-deployment knob (two behaviours to test for one
reader). If a consumer ever needs HTML at scale, it is one parse per message
from the blob the API already serves.

**The index inventory follows the readers (028).** `pg_stat_user_indexes`
since the cluster was created:

| Index | Size | Scans | Verdict |
|---|---|---|---|
| `messages_pkey` | 19 MB | 6.2 billion | keep (the pass-queue prune joined every marker to it on every call; see below) |
| `messages_folder_id_uid_key` → `messages_folder_uid_key` INCLUDE (mod_seq) | 19 → ~22 MB | 54 million | replaced: the session view's `(uid, mod_seq) ORDER BY uid` read becomes an index-only ordered scan (was a heap scan plus sort, 84 ms / 19,308 buffers on a 46K folder) |
| `idx_messages_folder_modseq` | 20 MB | 17 million | keep: CONDSTORE range queries, and the planner's favourite covering index for `folder_id = $1` counts |
| `idx_messages_date_id` | 26 MB | 35,865 | keep: the global `(internal_date, id)` cursor for search/export and the complete-round sweeps |
| `idx_messages_folder_date` | 29 MB | 432 | keep: the reorganization's per-folder chronological walk |
| `idx_messages_sha_date_folder` | 54 MB | 1.8 million | keep: the GC reference check |
| `idx_messages_folder_sha` | 53 MB | 12 | keep: the delivery dedup lookup (`indexes_rm51x007_test.go` pins it) |
| `idx_messages_folder_id_id` | 19 MB | 1.8 million | keep: the live sorter's candidate scan (migration 026) |
| `idx_messages_message_id` | 65 MB | 0 | keep: no reader today, but the natural key for threading; revisit if still 0 next year |
| `idx_messages_sent_date_local` | 6 MB | 0 | keep: tiny; SENTSINCE is folder-scoped so the planner prefers the folder key |
| `idx_messages_headers` GIN | 741 MB | 0 | dropped: only `SEARCH HEADER x ""` (field exists) could use it; never sent; every delivery and flag change indexed every header value |
| `idx_messages_deleted_folder` (partial) | 16 KB | 7,075 | keep (migration 024): STATUS DELETED, the live sorter, the Trash purge |
| `idx_messages_unseen_folder` (partial) | new | — | STATUS UNSEEN as an index-only count |
| `idx_delivery_log_{message,mailbox,alias}` (partial) | 13 MB, 5 MB, 72 KB | 1,733 / 6 / 2 | keep (migration 023): they back `ON DELETE SET NULL` foreign keys; without them every EXPUNGE scanned the log |
| `idx_attachments_sha256` | 3 MB | 0 | dropped: prefix of `idx_attachments_sha_date`, which the GC reference check asks for |

**STATUS computes only what the client asked for.** Mail clients poll
`STATUS <folder> (MESSAGES UNSEEN)` for every folder they show. The server
answered with one scan of the folder's rows for all four aggregates: 121 ms
and 19,308 buffers on the 46K-message INBOX, the largest read on the database
(17M scans fetching 640M heap tuples). Now MESSAGES is an index-only count,
UNSEEN and DELETED are index-only counts of the partial indexes (9 ms / 231
buffers for the count), and SIZE — the one that needs `raw_size` from every
row — runs only when requested. The predicates are spelled exactly as the
indexes' (`NOT ('\Seen' = ANY(flags))`); `status_indexes_test.go` asserts the
plan, because a rephrasing would be correct and silently slow.

**The pass queue is scoped (029).** `annotation_pass_required` carries each
marker's `mailbox_id`, indexed. `pass_required=true` is a semi-join from the
page's messages to the queue (RM51X-114) with the scope inside it, so a worker
whose mailbox has nothing queued reads its empty slice of the queue through
`idx_annotation_pass_required_mailbox` and stops (4 buffers), where before it
walked every message in date order probing a queue of 357K markers (mostly
for mailboxes imported after migration 022 that no worker annotates): 1.5 s
and 1.17M buffers per round to find nothing. Prune is one set of hash joins
over the scoped queue; it was a correlated `EXISTS` per marker joined through
`messages` and `folders`, which had run 78 million sequential scans of the
157-row `archive_categories` table (4.6 billion rows read) in a month.
`passqueue_scale_rm51x114_test.go` asserts both plans.

## Rolling the schema change out

027–029 are additive and build their indexes `CONCURRENTLY` (the migration
runner's `-- migrate: no-transaction` mode); 030 drops what only the old
binaries used. The order that keeps every daemon serving:

```sh
epistula-database migrate up -to 29 -timeout 0   # minutes to tens of minutes: the GIN build
# deploy epistula-database, epistula-imap, epistula-api (all three name the new objects)
epistula-database migrate up                     # 030, instant
```

Between the two `migrate up`s both generations of binary work: the old ones
still find `fts` and `html_body`, the new ones use `mail_fts` and the queue's
`mailbox_id` (nullable until 030; the second backfill catches a marker an old
binary wrote in the window). The three daemons probe at startup and say what
they are missing: epistula-api answers 503 on search and the pass queue until
027/029 exist; epistula-imap logs an ERROR that COPY and MOVE will fail until
029 exists.

A `CREATE INDEX CONCURRENTLY` interrupted part-way leaves an INVALID index
under its name; the runner drops it and builds again on the next run. It
waits for every transaction older than its own snapshot, so a long-running
export or an idle-in-transaction session delays it; `pg_stat_activity` shows
what it is waiting on, `pg_stat_progress_create_index` how far it is.

An operator who must run the index builds by hand (a superuser session, a
host where the binary cannot be promoted first) can execute the three
no-transaction files with `psql -f` and record each in `schema_versions`
afterwards; `ALTER FUNCTION mail_fts(text, text) OWNER TO <schema owner>` then
keeps the runner able to replace the function later. Indexes always belong to
the table's owner, so they need nothing.

## Reclaiming the space

`DROP COLUMN` marks the column dropped and leaves its TOAST chunks where they
are. The 5.5 GB that `fts` and `html_body` occupied is freed only when the
rows are rewritten:

- **`VACUUM FULL messages`** rewrites heap, TOAST and every index, holding an
  ACCESS EXCLUSIVE lock for the duration (deliveries defer, IMAP and the API
  wait; expect the time of the GIN build plus the copy, so a maintenance
  window) and needing free space for the new copy — about half the current
  size. Check `df` first. Raise `maintenance_work_mem` for the session.
- **`pg_repack`** (`databases/pg_repack` on FreeBSD) does the same online with
  a brief lock at each end; it needs the extension installed and the same free
  space. Preferred if the port is available.
- Doing nothing: plain VACUUM makes the dead space reusable inside the table,
  so the database stops growing until new mail has filled 5.5 GB. It does not
  shrink, and the backups keep carrying the dead pages.

Afterwards `ANALYZE messages` so the planner sees the new index set, and read
`pg_stat_user_indexes` again in a month.

## Large reorganizations and the search index

A one-time `archive-apply` over a whole mailbox rewrites every moved row and
re-inserts its entries into every index, `idx_messages_search` included
(recomputing `mail_fts` per row). With the index in place that ran at about
6 messages a second on a 271K-message mailbox and pending-list merges pushed
single batches past the 30 s `statement_timeout`; without it, 240–330 a
second. For a reorganization of that size, drop the index first and rebuild
it afterwards (full-text search is slow in between; epistula-api answers
`/v1/search` with 503 only while the `mail_fts` function is absent, not while
the index is):

```sh
psql -d epistula_database -c 'DROP INDEX CONCURRENTLY public.idx_messages_search'
epistula-database admin archive-apply -mailbox <name> -batch-size 100 -yes
psql -d epistula_database \
  -c 'CREATE INDEX CONCURRENTLY idx_messages_search ON public.messages USING gin (mail_fts(subject, text_body))'
# then confirm pg_index.indisvalid
```

## Keeping it healthy

- `autovacuum` is on with the defaults; the visibility map stays fresh enough
  for index-only scans (91 heap fetches in 46K on the measured folder). Do not
  lower `autovacuum_vacuum_scale_factor` for `messages` without reading the
  WAL cost.
- Flag changes are non-HOT updates by design: `mod_seq` is indexed for
  CONDSTORE and `flags` feeds two partial indexes, so every STORE writes a new
  tuple and one entry per index. That is the price of the covering indexes
  and it is paid once per read of a message. `fillfactor` on `messages` is the
  default; lowering it would buy nothing while `mod_seq` is indexed.
- The one GIN index gets an entry per lexeme per non-HOT update, buffered in
  its pending list (`gin_pending_list_limit` 4 MB) and merged by autovacuum
  or by whichever statement overflows it. A bulk move flushes it between
  batches (`archive/ginflush.go`).
- On the reference store `shared_buffers` is 6 GB, `work_mem` 4 MB (the old
  prune's hash spilled to disk at 357K rows; the scoped one does not),
  `maintenance_work_mem` 64 MB (raise to 1 GB for the session when building
  the GIN), `jit` on, `random_page_cost` 4 (the spinning-disk default; set
  1.1 if the pool is on SSD, though every plan measured here was already an
  index plan).
- `pg_stat_statements` is not loaded there. It would have answered "which
  query scans `idx_messages_folder_modseq` 17 million times" directly instead
  of by inference; adding it to `shared_preload_libraries` needs a restart and
  is worth doing at the next one.
- The schema parity test (`TestSchemaSQLMatchesMigrations`) cannot see a live
  server and `migrate status` cannot see an index. After any manual index work
  on a deployment, add it to a migration or drop it, and say which in the
  commit; an index that exists only on one host is invisible to every test.

## The measurements

Run as a role that can read the catalog; nothing here writes.

```sql
-- table, TOAST and index sizes
SELECT c.relname, pg_size_pretty(pg_relation_size(c.oid)) heap,
       pg_size_pretty(COALESCE(pg_relation_size(c.reltoastrelid),0)) toast,
       pg_size_pretty(pg_indexes_size(c.oid)) idx,
       pg_size_pretty(pg_total_relation_size(c.oid)) total
  FROM pg_class c WHERE c.relkind='r' AND c.relnamespace='public'::regnamespace
 ORDER BY pg_total_relation_size(c.oid) DESC;

-- what each column of messages costs on disk (compressed), whole table
SELECT pg_size_pretty(sum(pg_column_size(text_body))) text_body,
       pg_size_pretty(sum(pg_column_size(headers)))   headers,
       pg_size_pretty(sum(pg_column_size(bodystructure))) bodystructure
  FROM messages;

-- which indexes are read, and how much they return
SELECT indexrelname, pg_size_pretty(pg_relation_size(indexrelid)) size,
       idx_scan, idx_tup_read, idx_tup_fetch
  FROM pg_stat_user_indexes WHERE schemaname='public'
 ORDER BY pg_relation_size(indexrelid) DESC;

-- sequential scans and HOT updates per table
SELECT relname, n_live_tup, n_dead_tup, n_tup_upd, n_tup_hot_upd, seq_scan,
       seq_tup_read, last_autovacuum
  FROM pg_stat_user_tables WHERE schemaname='public' ORDER BY n_live_tup DESC;

-- the STATUS poll on the biggest folder, before and after
EXPLAIN (ANALYZE, BUFFERS)
SELECT (SELECT count(*) FROM messages WHERE folder_id = $1),
       (SELECT count(*) FROM messages WHERE folder_id = $1 AND NOT ('\Seen' = ANY(flags)));

-- invalid indexes (an interrupted CONCURRENTLY build)
SELECT indexrelid::regclass FROM pg_index WHERE NOT indisvalid;
```
