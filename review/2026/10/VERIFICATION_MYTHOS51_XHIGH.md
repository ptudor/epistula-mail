# Verification — REVIEW_MYTHOS51_XHIGH

Verified 2026-10-14 against review baseline `be04ae7` and the resulting fix series through `abd92d2` (the verification report itself is committed afterward).

## Result

All 157 findings pass verification. No finding remains failed or unintentionally skipped.

- **PASS:** 157
- **FAIL:** 0
- **New regressions found:** 0 after the verification fixes below

Verification found three actionable gaps in the original fix log and corrected them:

- **RM51X-005:** replaced the ineffective directory marker/memo with a bounded open-handle/mtime identity cache. It avoids redundant ancestry syncs while detecting peer changes and prune/recreate replacement. The post-link and dedup fsyncs remain.
- **RM51X-071 / RM51X-093:** made connection-class startup failures retryable after the listener opens. Readiness and LOGIN stay unavailable until maintenance and authentication-catalog checks succeed; real maintenance/configuration results remain fatal.
- **RM51X-096:** added the standard charset-label fallback, including the GB2312-to-GBK mapping, to the shared word decoder.

## Regression and constraint checks

The final tree passed these checks:

- `go test -race ./... -count=1` in `database`, `imap`, `api`, `llm-worker`, and `mcp`. The database run included the configured PostgreSQL integration tests.
- `go vet ./...` in all five modules.
- `bash scripts/check-toolchains.sh`, `python3 scripts/check-config-keys.py`, and `git diff --check`.
- Focused subprocess, protocol, race, scale, failure-injection, and EXPLAIN tests described per finding below.

The review's explicit preservation constraints were rechecked. The series retains delivery-log `SET NULL` audit history and schema columns; raw blobs/headers and persisted MIME JSON; content-addressed layout and lock ordering; the pre-commit blob durability invariant; import-only folder-scoped dedup; maintenance/exclusive-lease behavior; archive journal and purge contracts; IDLE wire and retry behavior; successful FETCH wire shape, Seen ordering, and one-message cache bound; atomic MOVE/COPYUID and advisory-lock order; API response/status/offset contracts; packaging paths, service names, group, mode `2770`, and the FreeBSD spool layout; Go language floor; and existing configuration key names/defaults.

Runtime verification that requires Linux package containers or FreeBSD services was unavailable on this macOS host. Those rows pass from artifact inspection, shell/plist validation, package snapshot checks, and targeted unit/subprocess coverage, and are marked below. The Linux package container remains the only implemented test that could not be executed because no supported container runtime is installed.

## Per-finding results

### RM51X-001–040

| Finding | Result | Verification |
|---|---|---|
| RM51X-001 | PASS | Migration/schema parity and EXPLAIN use the three new partial FK indexes; timed deletion preserves audit rows with NULL references. |
| RM51X-002 | PASS | Year-zero dates store NULL convenience dates while raw headers remain; parser and PostgreSQL ingest tests pass. |
| RM51X-003 | PASS | Absorbable top-level multipart defects are salvaged with the exact defect label; header and resource-limit refusals remain. |
| RM51X-004 | PASS | Fresh writes use stat-based verification, while dedup/rewrite paths retain hashing and the existing lock order; sweep races pass. |
| RM51X-005 | PASS | Verification fix now gives exactly one directory fsync on a repeat write and re-syncs peer-created or replaced chains without weakening durability. |
| RM51X-006 | PASS | Shared folder validation, INBOX canonicalization, config refusal, and no-side-effect import failures pass; existing rows are untouched. |
| RM51X-007 | PASS | Redundant indexes are removed by migration 024; retained indexes serve GC and duplicate lookup in EXPLAIN tests. |
| RM51X-008 | PASS | `VerifyOff` still requires an existing regular file; missing and present cases pass. |
| RM51X-009 | PASS | GC removes and counts old `.repair-*` files while preserving young in-flight files. |
| RM51X-010 | PASS | Corrupt-file repair uses atomic staged replacement; concurrent readers never observe a missing destination. |
| RM51X-011 | PASS | Existing-root sync starts at the storage root; only newly created root ancestry is parent-synced. Permission injection passes. |
| RM51X-012 | PASS | Root-level tenant symlinks participate in walk, permissions, and stale-file sweeps; unsafe/dangling targets are reported. |
| RM51X-013 | PASS | An ENOENT caused by concurrent pruning retries the slow creation path and closes the writer successfully. |
| RM51X-014 | PASS | `log-prune` requires a positive age and explicit dry-run/yes, deletes in 1,000-row batches, and preserves recent rows and outcomes. |
| RM51X-015 | PASS | Charset expansion is bounded to valid UTF-8 projection prefixes and records truncation without changing raw content or attachments. |
| RM51X-016 | PASS | Notify failure is now returned through the transaction path and causes rollback; the successful ingest path is unchanged. |
| RM51X-017 | PASS | Delivery flag-parse failures log and defer with `EX_TEMPFAIL`; other subcommands retain `EX_USAGE`. |
| RM51X-018 | PASS | All five loaders reject unknown TOML keys with location detail; shipped examples/packages parse through their real load paths. |
| RM51X-019 | PASS | Every production blob-store constructor attaches the service logger; repair events are observable in integration tests. |
| RM51X-020 | PASS | Delivery inspects only root/target permissions and reuses the store for duplicate repair; a 5,000-tenant scale case passes. |
| RM51X-021 | PASS | Prometheus gauges use catalog estimates rather than base-table counts; EXPLAIN and concurrent scrape tests pass. |
| RM51X-022 | PASS | Live commands require an existing storage root and create nothing when it is absent; tenant creation remains supported. |
| RM51X-023 | PASS | Delivery dedup follows a message across folders; importer dedup remains folder-scoped and expunged mail can redeliver. |
| RM51X-024 | PASS | Non-positive SQL/connection durations and sub-second HTTP/shutdown bounds are rejected. |
| RM51X-025 | PASS | Delivery's internal invariant failures now defer; no `EX_SOFTWARE` return remains in the delivery path. |
| RM51X-026 | PASS | Hook environment is allowlisted and cancellation kills the process group within bounded wait; database secrets do not leak. |
| RM51X-027 | PASS (platform-limited) | rc.d prestart runs validation as each service account with safe argument passing; shell validation passed. FreeBSD service startup was unavailable. |
| RM51X-028 | PASS | Documentation now matches wildcard/catchall behavior and actual outcomes; parser limit and resolver tests agree. |
| RM51X-029 | PASS | The permitted documentation option clearly states literal extension handling and the transport alternative's tradeoffs. |
| RM51X-030 | PASS | Schema and operator documentation now describe the actual store-wide maintenance barrier; locking behavior is unchanged. |
| RM51X-031 | PASS | Envelope domains are IDNA-normalized to stored A-labels; localpart behavior is unchanged. |
| RM51X-032 | PASS | Maildir symlinks use target metadata and non-regular targets are skipped. |
| RM51X-033 | PASS | All loaders ignore a bogus CWD config, use absolute discovery paths, and log the selected file. |
| RM51X-034 | PASS | Store-wide jobs no longer fingerprint mailbox catalogs but still bind database identity; import checkpoints retain target binding. |
| RM51X-035 | PASS | Missing tenants produce one durable failure record and a resumable checkpoint rather than one record per blob. |
| RM51X-036 | PASS | Default maintenance manifests live under absolute hidden paths beneath storage root and work from `/`. |
| RM51X-037 | PASS | Import date precedence is last-hop Received, bounded sender Date, then mtime/now; boundary cases pass. |
| RM51X-038 | PASS | Verification continues past an oversized source, checks attachment blobs in pages, and returns `EX_DATAERR` for damage. |
| RM51X-039 | PASS | Sweep performs one maintenance query per run while retaining per-candidate mailbox resolution and the flock invariant. |
| RM51X-040 | PASS | Grace comparisons use the database clock; 23-hour/25-hour boundary coverage passes. |

### RM51X-041–080

| Finding | Result | Verification |
|---|---|---|
| RM51X-041 | PASS | Misplaced blobs and unknown subtrees are reported/journaled without deleting or moving their source files. |
| RM51X-042 | PASS | Reparse queues any changed text, HTML, attachment content, or attachment metadata; identical rewrites do not queue work. |
| RM51X-043 | PASS | Empty dry-run queues work without `job.json`; record-bearing queues give actionable identity recovery errors. |
| RM51X-044 | PASS | Transient database/maintenance failures map to `EX_TEMPFAIL`; filesystem path errors retain `EX_OSERR`. |
| RM51X-045 | PASS | Resume tolerates an independently created target with the same mailbox identity and still refuses replacement/mismatch. |
| RM51X-046 | PASS | Reference paging closes queries before hashing, commands use signal cancellation, and release verifies before its short clearing transaction. Large-mailbox and corrupt-refusal tests pass. |
| RM51X-047 | PASS | Mailbox delete retains one atomic locked cascade but disables statement timeout under signal cancellation; the 2,000-message case passes. |
| RM51X-048 | PASS (no change required) | The premise is false for the current code: every admitted login runs the same bounded catalog of stored cost classes, including above-default classes. Uniform-work and catalog-bound tests pass. |
| RM51X-049 | PASS | CLI and usage distinguish irreversible duplicate deletion from undoable moves; actual undo counts match. |
| RM51X-050 | PASS | Shared terminal escaping removes raw C0/DEL/C1 controls from all four commands while stored audit text remains verbatim. |
| RM51X-051 | PASS | Mailbox listing exposes maintenance state and offline errors name the quiesced mailbox while preserving `errors.Is`. |
| RM51X-052 | PASS | The permitted documentation option now states the exact confirmation gates and fail-fast maintenance behavior; generated usage matches. |
| RM51X-053 | PASS | Negative quota, invalid/control localparts, and empty/control token names are rejected without side effects. |
| RM51X-054 | PASS | The dead helper and stale comment are gone; build and symbol search pass. |
| RM51X-055 | PASS | Verification distinguishes missing, wrong-size, wrong-hash, stat, and read failures while keeping the report cap. |
| RM51X-056 | PASS | Token cleanup reports only newly revoked or newly rescoped live tokens. |
| RM51X-057 | PASS | `domain-set` toggles wildcard state in place and preserves aliases/allowlist identity. |
| RM51X-058 | PASS | A refused destination is logged/counts once and later batches continue; apply/merge refusal and journal behavior remain. |
| RM51X-059 | PASS | Default `purge_all_copies=false` preserves kept/fresh copies; explicit purge and storage contracts are unchanged. |
| RM51X-060 | PASS | Undo and owner move-back remain sticky across sorter passes; explicit refile still works and journal schema/reasons remain. |
| RM51X-061 | PASS | Merge records only committed moves and rechecks a surviving destination copy under lock before purging. Both removal races preserve data. |
| RM51X-062 | PASS | IMAP sessions set statement timeout, deleted rows use a partial index, and tracker work is capped without changing batch semantics. |
| RM51X-063 | PASS | EXPUNGE applies role behavior to Trash/Drafts/Junk subtrees, including UID subsets; retention scope remains documented. |
| RM51X-064 | PASS | Apply rechecks Deleted under folder/message locks; skipped-only work rolls back with no journal row. |
| RM51X-065 | PASS | Category ancestors and locked destinations are checked for special-use conflicts in import, planning, and live sorting. |
| RM51X-066 | PASS | A 422 invalidates category vocabulary immediately, retries at most once, and prevents repeated stale-model calls. |
| RM51X-067 | PASS | Journal rows carry durable folder IDs with legacy-name fallback; rename/undo/reclassify cases pass. |
| RM51X-068 | PASS | All specified RM51X-058–064 gaps and real sorter/IMAP races are covered. The extra tracker-vs-STORE outcome stays unspecified because `DeletedTracker` permits the observed ordering; no contrary policy was invented. |
| RM51X-069 | PASS | Temporary accept errors retry with bounded interruptible backoff; a real low-`RLIMIT_NOFILE` subprocess recovers after EMFILE. |
| RM51X-070 | PASS | Session state, Poll, and IDLE reconciliation are serialized; disconnect races and completion waits pass under `-race` without wire changes. |
| RM51X-071 | PASS | Verification fix keeps the listener alive during a connection-class database outage, reports not-ready/UNAVAILABLE, and recovers without restart. Maintenance and real catalog failures remain fatal. |
| RM51X-072 | PASS | RENAME preserves UIDVALIDITY, so tagged OK, NOOP, and UID FETCH succeed on the same selected connection. |
| RM51X-073 | PASS | RENAME re-resolves/locks its source and descendants inside the transaction; competing rename returns NONEXISTENT without splitting the tree. |
| RM51X-074 | PASS | One hierarchy-declaration slash is stripped and all folder writes share strict validation; corrected CREATE/COPY fixtures pass. |
| RM51X-075 | PASS | Only the leading INBOX segment is canonicalized; child case sensitivity remains. |
| RM51X-076 | PASS | Dedicated IDLE pool size must be positive; 50 sessions leave the main pool usable with the dedicated pool saturated. |
| RM51X-077 | PASS | Poll fallback uses a positive configurable interval and remains bounded by listener-pool capacity; DONE/retry cases pass. |
| RM51X-078 | PASS | LIST returns child attributes and requested STATUS, and capabilities advertise only implemented extensions. LSUB semantics remain. |
| RM51X-079 | PASS | Mailbox creation atomically creates INBOX, and SELECT/STATUS/LIST/APPEND defensively repair a missing INBOX. |
| RM51X-080 | PASS | Every panicking transaction attempt rolls back before the session guard; pooled-connection accounting returns to zero. |

### RM51X-081–120

| Finding | Result | Verification |
|---|---|---|
| RM51X-081 | PASS | Drain cancels session work, serializes BYE, closes IDLE promptly, and preserves timeout/pool shutdown ordering. |
| RM51X-082 | PASS | The bounded 30-second auth-cost catalog cache removes per-login full scans while credential/disabled state stays current. |
| RM51X-083 | PASS | Malformed hashes receive uniform KDF work, fail authentication, warn once, and appear in startup audit counts without hash disclosure. |
| RM51X-084 | PASS | Throttle logs use the capped/sanitized username helper and do not expose passwords. |
| RM51X-085 | PASS | Pre-auth timeout closes, logs, and increments its metric exactly once. |
| RM51X-086 | PASS | Production validation loads and verifies the actual certificate/key pair, catching permissions, PEM errors, and mismatches. |
| RM51X-087 | PASS | The allowed documentation solution requires source-preserving redirect and explicitly states that proxy headers are unsupported. |
| RM51X-088 | PASS | LIST reference and pattern handling canonicalize the leading INBOX segment case-insensitively. |
| RM51X-089 | PASS | APPENDLIMIT is advertised from configuration and oversize literals are refused before continuation/upload. |
| RM51X-090 | PASS | Buffered blob reads compare actual bytes with `raw_size`; truncated part fetches return logged SERVERBUG. |
| RM51X-091 | PASS | Login admission checks whether the timer already won before taking a mailbox slot or logging/counting success. |
| RM51X-092 | PASS | A global pre-TLS connection cap and bounded handshake cap release slots correctly and coexist with per-IP limits. Multi-source socket coverage was host-limited; logic/unit coverage passed. |
| RM51X-093 | PASS | Coverage now exercises LISTEN-before-notify, selected RENAME, accept recovery, real EMFILE, and the RM51X-071 unreachable-DSN daemon path. |
| RM51X-094 | PASS | Fallible blob/section preparation precedes `CreateMessage`, response writers always close, and mid-literal failure closes the connection. Missing/corrupt cases permit a following NOOP. Absent MIME parts retain the established successful empty-section wire format. |
| RM51X-095 | PASS | Hashing occurs before mailbox locks, re-stat occurs under lock, advisory/candidate operations are set-wise, and timeouts scale by set. Large COPY contention, missing/corrupt source, candidate resurrection, COPYUID, and atomic MOVE pass. |
| RM51X-096 | PASS | Shared decoding now covers ISO-2022-JP, GBK, and GB2312 names while preserving mailbox/host and group/comma handling. |
| RM51X-097 | PASS | APPEND/STORE reject invalid system flags, long keywords, and the 101st distinct keyword before mutation; existing rows and PERMANENTFLAGS remain. |
| RM51X-098 | PASS | Reconcile uses modseq-watermarked deltas and falls back to full snapshots for removals/order ambiguity; 100,000-row scale and retry semantics pass. |
| RM51X-099 | PASS | Two-pointer compaction removes large UID sets in linear time and emits correct renumbered EXPUNGE sequences; unused window work is gone. |
| RM51X-100 | PASS | Zero-child multipart nodes render as single-part text/plain and part 1 resolves to opaque body bytes without changing persisted JSON. |
| RM51X-101 | PASS | BODY/TEXT/HEADER use unlowered patterns with the explicit ICU collation; non-ASCII cases pass even with database `C` collation. |
| RM51X-102 | PASS | ENVELOPE read/header failures log the required identifiers and return SERVERBUG while preserving protocol errors. |
| RM51X-103 | PASS | UID EXPUNGE resolves `*` against the session view, excluding an unseen higher database UID. |
| RM51X-104 | PASS | Retry is limited to serialization/deadlock codes; 40003 executes the body once and returns an aborted/retryable client response. |
| RM51X-105 | PASS | Implicit Seen and STORE share sorted/distinct normalization and retain modseq behavior. |
| RM51X-106 | PASS | IMAP exposes all ingest limits, uses their defaults, and maps limit refusal to `NO [LIMIT]`; raised-limit success passes. |
| RM51X-107 | PASS | Archive flag changes notify peers even when nothing is deleted; peer IDLE receives the update. |
| RM51X-108 | PASS | The review's allowed timeout alternative returns `NO [LIMIT]` with narrowing advice; non-timeout errors stay generic. The unchosen trigram EXPLAIN check is inapplicable. |
| RM51X-109 | PASS (optional change declined) | The requested rewrite is explicitly optional and would change same-folder MOVE, which currently allocates a fresh UID and has regression coverage. Current MOVE remains internally consistent. |
| RM51X-110 | PASS | Specified dead branches/symbols and stale tests are removed/updated; search suites and `go vet` pass. Four unrelated pre-existing staticcheck symbols were not folded into this finding. |
| RM51X-111 | PASS | The Argon2 semaphore covers only verification; database work and failure delay run outside it. Capacity-one timing and throttle tests pass. |
| RM51X-112 | PASS | Both single-message body routes authorize a locator before fetching content; trace proves no out-of-scope body query. Response contracts remain. |
| RM51X-113 | PASS | Scoped tokens receive the same 403 for absent and out-of-scope mailbox names across all routes; all-mailbox tokens retain 404. |
| RM51X-114 | PASS | Correlated `EXISTS` avoids materializing the queue; 200,008-row EXPLAIN/export scale tests remain stable across pages. |
| RM51X-115 | PASS | Readiness logs backend detail but returns static 503 bodies with no database/storage disclosure; liveness remains 200. |
| RM51X-116 | PASS | Central validation rejects NUL in paths, queries, inspection fields, and prune JSON as 422 without database errors. |
| RM51X-117 | PASS | Startup probes all handler-required grants and turns each tested missing privilege into a warning/503 rather than a later 500. |
| RM51X-118 | PASS | Token cache precedes failure throttling; forwarded IP is accepted only from configured one-hop proxies. Defaults trust none. |
| RM51X-119 | PASS | GET and export order dotted part numbers numerically (`1.2`, `1.10`, `2`, `10`). |
| RM51X-120 | PASS | One page budget covers base rows, attachments, and annotations; annotation fetching is set-wise and ranking/cursor completeness pass. |

### RM51X-121–157

| Finding | Result | Verification |
|---|---|---|
| RM51X-121 | PASS | Date/ID/UID cursor components are range-checked; extreme invalid values return 422 and valid endpoints work. |
| RM51X-122 | PASS | Dirty paths enter authenticated fallback without stdlib redirects; unauthenticated requests get 401 and no Location header. |
| RM51X-123 | PASS | API validates timeouts/pool sizing and accepts case-insensitive Bearer schemes with correct splitting. |
| RM51X-124 | PASS | SQL selects a bounded UTF-8 byte window and Go trims to complete runes; emoji allocation stays bounded. |
| RM51X-125 | PASS | Recorder starts unwritten and export metrics distinguish completed, aborted, and error outcomes, including stalled clients. |
| RM51X-126 | PASS | Folder/category queries bind scope in SQL as well as Go; traces cover all-scope and constrained tokens. |
| RM51X-127 | PASS | API docs now match durable-ID scope, statement timeout, thread status, local socket use, and throttle behavior. |
| RM51X-128 | PASS | Added coverage exercises auth-slot timing, scope-before-body, NUL input, grant gaps, large pass queues, and model priority. |
| RM51X-129 | PASS | API emits a dedicated unavailable-pass-queue problem type and the worker falls back only for that type. |
| RM51X-130 | PASS | The review explicitly allowed documentation: multi-model deployments now assign complete rounds to one worker and set the others to interval zero. Schedule tests pin it; the alternative two-fast-worker test is inapplicable. |
| RM51X-131 | PASS | Every MCP HTTP bind requires a token, including loopback; stdio remains token-free and subprocess refusal/auth tests pass. |
| RM51X-132 | PASS | EOF completion is independent of per-message error; classification continues and the next scheduled round is fast below the abort threshold. Run-once failure reporting remains. |
| RM51X-133 | PASS | Worker config rejects folder-without-mailbox and malformed since/before filters while accepting both documented date layouts. |
| RM51X-134 | PASS | Tokens are trimmed after overrides and control characters are rejected without logging the secret. |
| RM51X-135 | PASS | Mail API and model URLs reject userinfo and direct operators to token fields. |
| RM51X-136 | PASS | Prompt subject/from/to/cc/mailbox/folder metadata is control-flattened; subject is rune-capped without changing line order. |
| RM51X-137 | PASS | Annotation and classification outcomes count independently, so classification failure does not erase annotation progress or defer the whole message. |
| RM51X-138 | PASS | Attempted IDs survive stream reconnects within a round, prevent duplicate model calls, and reset on the next complete round. |
| RM51X-139 | PASS | Export client clones the default transport, preserving proxy, idle, and HTTP/2 settings while adding only response-header timeout. |
| RM51X-140 | PASS | MCP surfaces a bounded redirect-refusal cause while keeping other transport errors generic and never forwarding token/body. |
| RM51X-141 | PASS | Omitted-limit calls retry the exact older-API 422 once with the numeric default and stop after a second refusal. |
| RM51X-142 | PASS | Tool description begins with the action and ends with continuation guidance; tools/list smoke inspection passes. |
| RM51X-143 | PASS | Archive-category mailbox names go into URL path once; percent/space cases arrive decoded exactly once. |
| RM51X-144 | PASS | MCP and worker config tests clear all relevant environment overrides; complete suites pass under poisoned variables. |
| RM51X-145 | PASS | Fedora instructions use the protected TOML secret location and no longer recommend a service environment file. |
| RM51X-146 | PASS (platform-limited) | Postinstall conditionally transfers only a still-root-owned default spool, retaining group/mode/path/service/FreeBSD layout; the Linux transport and cleared-group delivery assertions are present. Signed packages build, but the container test could not run here. |
| RM51X-147 | PASS (platform-limited) | MCP uses sysexits 78/71 and service artifacts suppress permanent restart loops; subprocess exit matrix and plist/shell checks pass. No live service manager test ran. |
| RM51X-148 | PASS (platform-limited) | FreeBSD install permissions and service-user readability checks are consistent and shell-valid. A prepared FreeBSD host was unavailable. |
| RM51X-149 | PASS | All modules select Go 1.27.2 while keeping the 1.26 language floor; mismatch CI check passes. A full signed snapshot produced all expected binaries, archives, and Linux packages with verified checksums/signatures/notices. |
| RM51X-150 | PASS (platform-limited) | rc.d artifacts use supervisor pidfiles and validated shutdown waits, and docs target the supervisor for rotation. Shell/static manual checks pass; live FreeBSD restart was unavailable. |
| RM51X-151 | PASS (platform-limited) | Fedora instructions point to the actual build artifact; clean linux/amd64 cross-build/install and embedded Go/version metadata pass. Fedora execution was unavailable. |
| RM51X-152 | PASS | Changelog dependency version matches all module pins exactly once. |
| RM51X-153 | PASS | Deployment guidance now matches supported transport attributes, ports, delays, token rules, and config ownership. |
| RM51X-154 | PASS | Both example and package TOMLs contain all supported keys with correct defaults; automated tag coverage passes and defaults are unchanged. |
| RM51X-155 | PASS (platform-limited) | launchd uses a dedicated account, restrictive umask, readable protected config, and owned log; plist lint/static account checks pass. Service was not installed locally. |
| RM51X-156 | PASS (platform-limited) | FreeBSD rc.d creates and repairs logfile owner/mode before startup; shell checks pass. FreeBSD 13 runtime was unavailable. |
| RM51X-157 | PASS | CI ShellCheck includes all four rc.d scripts with deliberate exclusions; the recorded official ShellCheck and `sh -n` runs pass. |

## Skipped-item disposition

| Finding | Disposition |
|---|---|
| RM51X-005 | Original skip rejected; fixed and fully tested during verification. |
| RM51X-048 | Stays unchanged. The finding assumes a default-only timing floor, but the implementation equalizes the bounded catalog of every supported stored cost class. Applying the proposed warning/dummy policy would weaken the current defense. |
| RM51X-068 | The specified coverage is present. The extra tracker-vs-STORE race has no required outcome in the fix specification, and the tracker contract explicitly permits archive after `stillDeleted`; no policy change is justified. |
| RM51X-071 | Original skip rejected; fixed during verification. The malformed-hash sentence conflicts with RM51X-083, whose required behavior is warning plus ordinary auth failure. RM51X-083 is retained, while real catalog size/budget results stay startup-fatal. |
| RM51X-093 | Unreachable-DSN daemon coverage is no longer skipped; it was added with RM51X-071. |
| RM51X-094 | Only the absent-part subcase stays as established behavior: an absent MIME part is a successful empty section. Treating it as tagged NO would violate the same finding's successful-wire constraint. Real read/stat/parse failures are preflighted and fail cleanly. |
| RM51X-096 | GB2312 skip rejected; fixed through the shared standard charset registry. |
| RM51X-108 | Trigram EXPLAIN is not run because the review offered `NO [LIMIT]` as an alternative and that alternative was implemented and tested. |
| RM51X-109 | Optional rewrite stays declined because it lacks a same-folder policy and would change a covered fresh-UID behavior. The current implementation is correct under its documented semantics. |
| RM51X-130 | Two-worker marker-retention test is not applicable to the review's selected documentation-only option. The documented one-complete-worker schedule is tested. |
