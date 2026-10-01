# Epistula — Deployment artifacts for epistula-database

Two long-running surfaces and one synchronous transport:

| Mode          | Lifecycle        | Surface                                |
|---------------|------------------|----------------------------------------|
| `deliver`     | per-message      | Postfix `pipe(8)` invokes it on stdin  |
| `serve`       | long-running     | `127.0.0.1:8782` Prometheus + health   |
| `admin ...`   | one-shot         | Operator CLI (`mailbox-*`, `acl-*`, …) |

## Files

- `freebsd/epistula_database` — rc.d script for the `serve` daemon. Copy to `/usr/local/etc/rc.d/epistula_database`, `chmod 555`, then `sysrc epistula_database_enable=YES` and `service epistula_database start`.
- `postfix/master.cf` — pipe(8) transport definition. Append to `/usr/local/etc/postfix/master.cf`, `postfix reload`.
- `postfix/transport.example` — `transport_maps` examples. Install as `hash:` map, `postmap`, point `transport_maps` at it in `main.cf`.
- `postfix/INSTALL.md` — configure a new Postfix pipe transport and verify delivery.
- `apache/epistula-database-include.conf` — optional reverse-proxy Include for `/metrics` + `/health` on a public-facing vhost.

## What's NOT in `deploy/`

- Linux systemd units and package scripts live in the repository's `packaging/`
  directory; see [the Linux package guide](../../docs/linux-packages.md).
- The TOML config. See `../epistula-database.toml.example` at the project root.
- The schema. See `../schema.sql` for bootstrap; `epistula-database migrate up` for incremental.

## Schema versioning at deploy time

For a fresh deployment:

```sh
epistula-database migrate up -config /etc/epistula/epistula-database.toml
```

For an existing deployment:

```sh
epistula-database migrate status    # see what's pending
epistula-database migrate up        # apply
```

On FreeBSD, use `/usr/local/etc/epistula/epistula-database.toml`. The
[FreeBSD quick start](../QUICKSTART-FREEBSD.md) covers creating the database and
the `epistula_database` PostgreSQL role. `schema.sql` is kept in sync with the
migrations and is an alternative bootstrap for operators using the source tree.

### Maintenance failure queues

Import checkpoints use version 3 durable destination identity. Resuming from an older marker requires the explicit `-accept-legacy-checkpoint` override (on `import` and `import-blobs`) or a complete deduplicating scan. A changed database, recreated target, or changed filters requires a new checkpoint path.

Imports retain all failed keys in `<checkpoint>.failures.d/`, with job identity in `job.json` and one JSON record per failed key. This directory is authoritative after a crash; `.failures.json` is a streamed summary. Keep the queue with its checkpoint. Resume is partial (EX_TEMPFAIL) while failures remain. Repair sources and run the same job with `-retry-failures`; missing source items remain failures, and successful retries clear their records without duplicating successful mail. Legacy failure reports should be retained while a full deduplicating scan uses a new checkpoint path. Do not delete originals.

Import, `import-blobs` and `reparse-bodystructure` salvage a malformed or over-limit message instead of failing it (OPS-003). It is imported with outcome `imported:degraded` and its defects in `error_detail`: `admin log-tail -outcome imported:degraded` lists them, and `SELECT id FROM messages WHERE bodystructure @? '$.**.defects'` finds every degraded message, whichever path stored it. `-retry-failures -dry-run` previews a retry without writing anything; its summary counts the items that would import as `dry_run_ok`, of which `degraded` would be salvaged. It reports what the retry would leave unresolved, the items that still fail plus the queue records the pass does not reach, as `dry run: N item(s) would remain unresolved`, and exits as the retry would (OPS-005).

`reparse-bodystructure -manifest /path/reparse` records failed message IDs and supports `-retry-failures`. It returns EX_TEMPFAIL for unreadable, corrupt, unparseable or unrepairable surviving messages; expunged rows have no remaining repair work. `gc -phase mark -manifest /path/gc` records staging cleanup failures; fix their cause and rerun mark with the same path. Bookkeeping persistence failures return nonzero. Dry runs never create or update these files, and report what the same real run would leave unresolved; `gc -phase mark` has no dry run.

### Offline mailbox rename

Deploy the updated writer, IMAP and API binaries before using mailbox maintenance. All participants use the same mounted storage root, which stays in place throughout the move. Stop IMAP/API and Postfix, and wait for all LDA, import, verification, reparse and GC processes to exit. Their shared root-directory leases prevent maintenance entry until readers, cached authenticated sessions and writers have ended. Maintenance affects the whole blob store; it consumes no reserved PostgreSQL connection.

Run `epistula-database admin mailbox-maintenance -name old -on`, move only the tenant subtree (for example with `zfs rename`), then `admin mailbox-rename -from old -to new -yes`. New blob processes refuse startup while any mailbox is in maintenance. Rename verifies every referenced destination size/hash and preserves mailbox/message/folder IDs, UIDs, dates, flags and API token scopes. Maintenance releases with the database commit; restart services afterward.

If the filesystem move succeeds but rename fails, keep services stopped: repair the destination and retry rename, or restore the complete old tree and run maintenance `-off`. That release also refuses missing/corrupt references. A missing/unmounted root cannot pass the exclusive barrier. Do not bypass a busy barrier by clearing the database flag manually.
