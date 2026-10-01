package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/maintenance"
	"github.com/ptudor/epistula-mail/database/storage"
)

// rowQuerier is the subset of the pgx pool / transaction API the GC reference
// checks need. Both *pgxpool.Pool (mark, on the bare pool) and pgx.Tx (sweep,
// inside the per-candidate transaction) satisfy it, so mark and sweep run the
// IDENTICAL resolution + reference logic — a hard requirement: if the two
// disagreed about whether a tenant resolves or a blob is referenced, a blob
// could be marked but never swept, or swept while still live.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// mailboxRef is a memoized tenant-name → mailbox-id resolution. ok=false means
// the tenant directory has no backing mailbox row (a deleted mailbox, or a
// stray subtree): its blobs are definitionally unreferenced and reapable.
//
// quiesced means the mailbox exists but is in maintenance — an operator is
// moving its blob tenant tree. GC must not touch it (RA6X-013).
type mailboxRef struct {
	id       int64
	ok       bool
	quiesced bool
}

// resolveMailboxID maps a canonical tenant (mailboxes.name) to its id and
// maintenance state. A missing row is not an error — it returns ok=false
// (reapable, subject to the unresolved-tenant policy below).
func resolveMailboxID(ctx context.Context, q rowQuerier, tenant string) (mailboxRef, error) {
	var id int64
	var maintenanceAt *time.Time
	err := q.QueryRow(ctx,
		`SELECT id, maintenance_at FROM mailboxes WHERE name = $1`, tenant,
	).Scan(&id, &maintenanceAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return mailboxRef{}, nil
	}
	if err != nil {
		return mailboxRef{}, err
	}
	return mailboxRef{id: id, ok: true, quiesced: maintenanceAt != nil}, nil
}

// anyMailboxInMaintenance reports whether ANY mailbox is currently quiesced.
//
// It gates the "an unresolvable tenant directory is reapable" rule, which is
// otherwise correct — a directory with no mailbox row really is a deleted
// account's leftovers — but is catastrophic during a rename (RA6X-013). A
// rename is two-phase: the operator moves <storage_root>/<old> to
// <storage_root>/<new>, then updates mailboxes.name. In between, the tree is
// sitting under a name that resolves to nothing, and it holds the account's
// entire mail. A GC pass in that window would delete all of it.
//
// GC cannot tell which unresolvable directory is the in-flight rename — the
// maintenance flag lives on the mailbox row, which still carries the OLD name.
// So while any mailbox is quiesced, no unresolvable tenant is reaped at all.
// Orphans inside resolvable tenants are still collected normally, and the
// deferred work is picked up by the next pass once maintenance is released.
func anyMailboxInMaintenance(ctx context.Context, q rowQuerier) (bool, error) {
	var any bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM mailboxes WHERE maintenance_at IS NOT NULL)`,
	).Scan(&any); err != nil {
		return false, err
	}
	return any, nil
}

// blobReferenced reports whether a blob is still referenced by a message (raw)
// or attachment (att) row OWNED BY the given mailbox. Scoping to the mailbox is
// essential under per-tenant isolation: alice's on-disk file must be judged
// only against alice's rows, not bob's identical-content copy, or alice's
// orphan would never be reaped.
// referencePredicate returns the "is this blob referenced?" SQL for one kind,
// as a bare EXISTS-able subquery whose sha, bucket-date, and mailbox-id
// operands are supplied by the caller.
//
// This is the SINGLE source of truth for the predicate, deliberately. The hard
// GC invariant (see the file header) is that mark and sweep run identical
// resolution + reference logic: sweep re-checks one blob at a time under the
// blob's advisory lock and must keep doing so, while mark batches for speed.
// Two hand-written copies of this SQL is exactly the "mark and sweep disagree"
// failure that invariant exists to prevent, so both forms are built from this
// one fragment (RO5X-018).
//
// The %s substitutions are placeholder names and column references chosen by
// the caller — never user data. Nothing interpolated here comes from a
// message, a filename, or an operator argument.
func referencePredicate(kind blob.Kind, shaExpr, dateExpr, mailboxExpr string) (string, error) {
	switch kind {
	case blob.KindRaw:
		return fmt.Sprintf(`
			SELECT 1 FROM messages m
			  JOIN folders f ON f.id = m.folder_id
			 WHERE m.raw_sha256 = %s AND m.raw_blob_date = %s AND f.mailbox_id = %s`,
			shaExpr, dateExpr, mailboxExpr), nil
	case blob.KindAttachment:
		return fmt.Sprintf(`
			SELECT 1 FROM attachments a
			  JOIN messages m ON m.id = a.message_id
			  JOIN folders f ON f.id = m.folder_id
			 WHERE a.sha256 = %s AND a.blob_date = %s AND f.mailbox_id = %s`,
			shaExpr, dateExpr, mailboxExpr), nil
	default:
		return "", fmt.Errorf("referencePredicate: unknown kind %q", kind)
	}
}

// blobReferenced answers the predicate for one blob. Used by the sweep, which
// must stay one-blob-at-a-time under the advisory lock.
func blobReferenced(ctx context.Context, q rowQuerier, kind blob.Kind, mailboxID int64, sha []byte, bucketDate time.Time) (bool, error) {
	pred, err := referencePredicate(kind, "$1", "$2", "$3")
	if err != nil {
		return false, err
	}
	var referenced bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(`+pred+`)`, sha, bucketDate, mailboxID).
		Scan(&referenced); err != nil {
		return false, err
	}
	return referenced, nil
}

// blobProbe is one (sha, bucket-date) pair to test for references.
type blobProbe struct {
	sha        []byte
	bucketDate time.Time
	// shaHex and bucket carry the walker's original strings so the caller can
	// build the candidate row without re-encoding.
	shaHex string
	bucket blob.Bucket
}

// blobsReferenced answers the predicate for a whole chunk in ONE round trip,
// returning the subset that IS referenced, keyed by (hex sha, bucket).
//
// gc mark ran one SELECT EXISTS per blob file plus one upsert per orphan. Over
// a large archive that is millions of sequential round trips — at a
// conservative 0.2 ms each, 5M blobs is ~17 minutes of pure latency locally
// and hours over a network DSN. The pass holds no locks, so it was safe, just
// slow enough that an operator would run it less often, which directly
// lengthens the orphan-retention window the two-phase design depends on
// (RO5X-018).
func blobsReferenced(ctx context.Context, q rowQuerier, kind blob.Kind, mailboxID int64, probes []blobProbe) (map[string]bool, error) {
	out := make(map[string]bool, len(probes))
	if len(probes) == 0 {
		return out, nil
	}
	pred, err := referencePredicate(kind, "probe.sha", "probe.d", "$3")
	if err != nil {
		return nil, err
	}

	shas := make([][]byte, len(probes))
	dates := make([]time.Time, len(probes))
	for i, p := range probes {
		shas[i] = p.sha
		dates[i] = p.bucketDate
	}

	rows, err := q.Query(ctx, `
		SELECT DISTINCT encode(probe.sha, 'hex'), probe.d
		  FROM unnest($1::bytea[], $2::date[]) AS probe(sha, d)
		 WHERE EXISTS (`+pred+`)`,
		shas, dates, mailboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var shaHex string
		var d time.Time
		if err := rows.Scan(&shaHex, &d); err != nil {
			return nil, err
		}
		out[shaHex+"|"+d.Format("2006/01/02")] = true
	}
	return out, rows.Err()
}

// pruneEmptyDirs best-effort removes now-empty directories from dir upward,
// stopping at (and never removing) boundary (the `${root}/<tenant>/<kind>`
// subtree root). os.Remove on a directory only succeeds when it is empty, so
// any concurrent first-delivery MkdirAll/Link leaves the dir non-empty and the
// walk stops harmlessly (ENOTEMPTY); a racing prune yields ENOENT, also
// harmless. This keeps the per-tenant tree tidy for `rsync` without ever
// racing a writer into a corrupt state.
func pruneEmptyDirs(dir, boundary string) {
	for dir != boundary && strings.HasPrefix(dir, boundary+string(os.PathSeparator)) {
		if err := os.Remove(dir); err != nil {
			// A date bucket whose blobs are all gone still holds its
			// durability marker (RA6X-036), so the rmdir fails ENOTEMPTY on a
			// directory that is logically empty. Drop the marker and retry
			// once: the next writer into this bucket recreates it, syncing the
			// chain again, which is exactly the protocol.
			//
			// Only when the marker is the ONLY thing left — os.Remove of the
			// directory succeeding after is the proof, and if it does not, the
			// marker is restored on the next write anyway.
			if !blob.RemoveDurabilityMarkerIfOnlyEntry(dir) {
				return
			}
			if err := os.Remove(dir); err != nil {
				return
			}
		}
		dir = filepath.Dir(dir)
	}
}

func runGC(args []string) int {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	manifestPath := fs.String("manifest", "gc-mark", "Path prefix for durable staging-cleanup failure queue/report")
	phase := fs.String("phase", "", "Phase: mark | sweep | reconcile-quotas")
	graceSecs := fs.Int("grace-seconds", 86400, "Grace period: mark skips blobs younger than this; sweep defers candidates younger than this")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	if *phase == "" {
		fmt.Fprintln(os.Stderr, "gc: -phase is required (mark | sweep | reconcile-quotas)")
		return EX_USAGE
	}
	// A negative grace disables both the young-file skip and the sweep age
	// cutoff at once — dangerous silent behavior. Reject it; warn on 0 (R-030).
	if *graceSecs < 0 {
		fmt.Fprintln(os.Stderr, "gc: -grace-seconds must not be negative")
		return EX_USAGE
	}
	if *graceSecs == 0 {
		fmt.Fprintln(os.Stderr, "gc: WARNING -grace-seconds=0 disables the young-blob grace; in-flight deliveries risk being reaped")
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return EX_CONFIG
	}
	setupLogging(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := storage.Open(ctx, storage.Config{
		DSN:              cfg.Postgres.DSN,
		StatementTimeout: cfg.StatementTimeoutDuration(),
		// GC runs many of its queries directly on the pool (the mark
		// walker's per-blob checks); bound them all at the session level
		// so a wedged shared Postgres can't hang a maintenance run.
		SessionStatementTimeout: true,
		// Honour the documented pool knobs (RO5X-021).
		MaxConns:        int32(cfg.Postgres.MaxOpenConns),
		MinConns:        int32(cfg.Postgres.MaxIdleConns),
		ConnMaxLifetime: cfg.ConnMaxLifetimeDuration(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres: %v\n", err)
		return EX_TEMPFAIL
	}
	defer db.Close()

	store, code := openBlobStore(cfg, false)
	if code != EX_OK {
		return code
	}
	lease, lerr := maintenance.Acquire(ctx, cfg.Storage.Root, db.Pool(), false, false)
	if lerr != nil {
		fmt.Fprintf(os.Stderr, "offline maintenance barrier: %v\n", lerr)
		return EX_TEMPFAIL
	}
	defer lease.Close()

	switch *phase {
	case "mark":
		job := jobIdentity{Kind: "gc-staging", Source: canonicalSource(cfg.Storage.Root), Destination: redactedDSN(cfg.Postgres.DSN)}
		if err := bindJob(ctx, db, &job); err != nil {
			return EX_TEMPFAIL
		}
		journal, err := openRecovery(*manifestPath, job, false, false)
		if err != nil {
			slog.Error("open GC failure queue", "err", err)
			return EX_IOERR
		}
		defer journal.Close()
		return gcMark(ctx, db, store, time.Duration(*graceSecs)*time.Second, journal)
	case "sweep":
		return gcSweep(ctx, db, store, time.Duration(*graceSecs)*time.Second)
	case "reconcile-quotas":
		return gcReconcileQuotas(ctx, db)
	default:
		fmt.Fprintf(os.Stderr, "gc: unknown phase %q (use mark | sweep | reconcile-quotas)\n", *phase)
		return EX_USAGE
	}
}

// gcMark walks every blob on disk (across all tenants) and records the
// unreferenced ones as candidates in gc_candidates. Files younger than the
// grace period are skipped entirely — a freshly written blob may belong to a
// delivery whose transaction has not committed yet. Re-marking an existing
// candidate updates generation_marked (first_seen_at is preserved), which is
// the re-confirmation the sweep's generation gate requires: a candidate is
// only sweepable once a mark pass *later* than its first sighting has found it
// still unreferenced.
//
// References are checked by (tenant, sha256, date), scoped to the owning
// mailbox. Same content on a different day lives in a different bucket; the
// same content in a different mailbox lives in a different tenant subtree —
// both are separate blobs, and a row elsewhere does not protect this file. A
// tenant directory whose name no longer resolves to a mailbox (deleted box, or
// a stray subtree) has all its blobs treated as unreferenced and reapable.
func gcMark(ctx context.Context, db *storage.DB, store *blob.Store, grace time.Duration, journals ...*recoveryJournal) int {
	start := time.Now()
	// Take the mark-run generation from the DB clock, not the Go clock, so it
	// shares one timeline with first_seen_at. The sweep's generation gate
	// (generation_marked > epoch(first_seen_at)) is only sound when both come
	// from the same clock: with the Go clock even a few seconds ahead of PG, a
	// candidate could satisfy the gate on its very first sighting and collapse
	// the required two-mark-pass confirmation into one (R-045). gen is read at
	// mark start, so it is always <= the DB now() that stamps first_seen_at on
	// any candidate inserted during this run.
	var gen int64
	if err := db.Pool().QueryRow(ctx, `SELECT floor(extract(epoch FROM now()))::bigint`).Scan(&gen); err != nil {
		slog.Error("gc mark clock", "err", err)
		return EX_TEMPFAIL
	}

	// Surface storage_root children that are not valid tenants. Walk skips
	// them — right for stray files, but a subtree left by a mailbox whose name
	// was legal under an older rule, created by hand, or restored with the
	// wrong case is otherwise invisible to GC forever and the store silently
	// grows. Report, never auto-delete: it could be an operator's staging
	// area (RO5X-034).
	unknownSubtrees := map[string]struct{}{}
	store.SetOnUnknownSubtree(func(name string) { unknownSubtrees[name] = struct{}{} })
	defer store.SetOnUnknownSubtree(nil)

	idCache := map[string]mailboxRef{}
	var checked, marked, skippedYoung, orphanTenantBlobs, skippedMaintenance int64

	// Decided once per pass: an unresolvable tenant directory is only reapable
	// when no mailbox anywhere is mid-rename (RA6X-013).
	inMaintenance, err := anyMailboxInMaintenance(ctx, db.Pool())
	if err != nil {
		slog.Error("gc mark: maintenance check", "err", err)
		return EX_TEMPFAIL
	}
	reapUnresolved := !inMaintenance
	if inMaintenance {
		slog.Warn("gc mark: a mailbox is in maintenance; tenant directories with no mailbox row " +
			"will be skipped this pass rather than reaped")
	}

	// Batch the reference checks. Blobs accumulate per (tenant, kind); a chunk
	// is flushed when it fills, at every tenant/kind boundary, and at walk end,
	// so the check always runs against the right mailbox scope.
	const markChunkSize = 1000
	var (
		pending     []blobProbe
		pendingKind blob.Kind
		pendingTen  blob.Tenant
		pendingRef  mailboxRef
	)

	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		defer func() { pending = pending[:0] }()

		// An unresolvable tenant means every blob under it is reapable; no
		// reference check applies (this shortcut stays per-tenant and needs no
		// batching).
		referenced := map[string]bool{}
		if pendingRef.ok {
			var err error
			referenced, err = blobsReferenced(ctx, db.Pool(), pendingKind, pendingRef.id, pending)
			if err != nil {
				return fmt.Errorf("ref check chunk (tenant %q, kind %s, %d blobs): %w",
					pendingTen, pendingKind, len(pending), err)
			}
		} else {
			orphanTenantBlobs += int64(len(pending))
		}

		// Upsert every candidate in the chunk in one statement, mirroring the
		// batched read.
		var (
			cShas    [][]byte
			cBuckets []string
		)
		// Deduplicate by candidate key before the upsert (RA6X-062).
		//
		// The primary key is (tenant, sha256, kind, bucket), and an
		// INSERT ... SELECT FROM unnest() with the same key twice fails the
		// whole statement — "ON CONFLICT DO UPDATE command cannot affect row a
		// second time" — aborting a mark pass over the entire store. The shard
		// validation in blob.Walk should now prevent two files ever presenting
		// the same key, so this is defence in depth: a batch-wide abort is far
		// too large a consequence for one misplaced file.
		seenKey := make(map[string]struct{}, len(pending))
		for _, p := range pending {
			if referenced[p.shaHex+"|"+string(p.bucket)] {
				continue
			}
			key := p.shaHex + "|" + string(p.bucket)
			if _, dup := seenKey[key]; dup {
				slog.Warn("gc mark: duplicate candidate key in one batch; ignoring the repeat",
					"tenant", pendingTen, "kind", pendingKind, "sha16", p.shaHex[:16], "bucket", p.bucket)
				continue
			}
			seenKey[key] = struct{}{}
			cShas = append(cShas, p.sha)
			cBuckets = append(cBuckets, string(p.bucket))
		}
		if len(cShas) == 0 {
			return nil
		}
		if _, err := db.Pool().Exec(ctx, `
			INSERT INTO gc_candidates (tenant, sha256, kind, bucket, generation_marked)
			SELECT $1, c.sha, $2, c.bucket, $3
			  FROM unnest($4::bytea[], $5::text[]) AS c(sha, bucket)
			ON CONFLICT (tenant, sha256, kind, bucket)
			DO UPDATE SET generation_marked = EXCLUDED.generation_marked`,
			string(pendingTen), string(pendingKind), gen, cShas, cBuckets,
		); err != nil {
			return fmt.Errorf("insert candidates (tenant %q, %d blobs): %w",
				pendingTen, len(cShas), err)
		}
		marked += int64(len(cShas))
		return nil
	}

	visit := func(kind blob.Kind, tenant blob.Tenant, bucket blob.Bucket, shaHex, path string, info os.FileInfo) error {
		checked++
		if time.Since(info.ModTime()) < grace {
			skippedYoung++
			return nil
		}
		sha, err := hex.DecodeString(shaHex)
		if err != nil {
			return nil
		}
		bucketDate, err := time.Parse("2006/01/02", string(bucket))
		if err != nil {
			return nil
		}

		ref, cached := idCache[string(tenant)]
		if !cached {
			r, rerr := resolveMailboxID(ctx, db.Pool(), string(tenant))
			if rerr != nil {
				return fmt.Errorf("resolve tenant %q: %w", tenant, rerr)
			}
			ref = r
			idCache[string(tenant)] = ref
		}
		// Leave quiesced mailboxes and — while any rename is in flight —
		// unresolvable tenants completely alone (RA6X-013).
		if ref.quiesced || (!ref.ok && !reapUnresolved) {
			skippedMaintenance++
			return nil
		}

		// Boundary: a chunk only ever covers one (tenant, kind).
		if len(pending) > 0 && (tenant != pendingTen || kind != pendingKind) {
			if err := flush(); err != nil {
				return err
			}
		}
		pendingTen, pendingKind, pendingRef = tenant, kind, ref
		pending = append(pending, blobProbe{
			sha: sha, bucketDate: bucketDate, shaHex: shaHex, bucket: bucket,
		})
		if len(pending) >= markChunkSize {
			return flush()
		}
		return nil
	}

	// Surface files that look like blobs but sit under the wrong shard
	// (RA6X-062). They are not canonical, so mark must not treat them as
	// references or as candidates — but leaving them unreported means the
	// misplaced copy survives every pass while mark claims to have handled
	// that digest. Reported, never deleted: an operator resolves them.
	var misplaced []string
	store.SetOnMisplacedBlob(func(kind blob.Kind, tenant blob.Tenant, shaHex, path string) {
		if len(misplaced) < 100 {
			misplaced = append(misplaced, path)
		}
		slog.Error("gc mark: blob is stored under shard directories that do not match its digest; "+
			"it is not a canonical reference and will not be reaped — investigate and relocate or remove it by hand",
			"tenant", tenant, "kind", kind, "sha16", shaHex[:16], "path", path)
	})
	defer store.SetOnMisplacedBlob(nil)

	for _, k := range []blob.Kind{blob.KindRaw, blob.KindAttachment} {
		if err := store.Walk(k, visit); err != nil {
			slog.Error("gc mark walk", "kind", k, "err", err)
			return EX_OSERR
		}
		// Flush the tail of this kind before moving to the next.
		if err := flush(); err != nil {
			slog.Error("gc mark flush", "kind", k, "err", err)
			return EX_OSERR
		}
	}

	// Reap orphaned staging files (tmp/blob-*) left by a signal-killed or
	// watchdog-exited LDA — nothing in the raw/att walk visits tmp/ (R-031).
	// Same grace as blob marking so an in-flight write is never touched.
	staleTmp, err := store.SweepStaleTmp(grace)
	if err != nil {
		slog.Warn("gc mark tmp sweep", "err", err)
		if len(journals) > 0 {
			key := store.Root()
			var pe *os.PathError
			if errors.As(err, &pe) {
				key = pe.Path
			}
			if qerr := journals[0].fail(key, "staging_cleanup_failed", err); qerr != nil {
				slog.Error("GC failure record", "err", qerr)
				return EX_IOERR
			}
			return recoveryOutcome(journals[0], true)
		}
		return EX_TEMPFAIL
	}
	if len(journals) > 0 {
		j := journals[0]
		if err := j.each(func(item failedItem) error { return j.resolved(item.Key) }); err != nil {
			return EX_IOERR
		}
		if code := recoveryOutcome(j, true); code != EX_OK {
			return code
		}
	}

	if len(unknownSubtrees) > 0 {
		names := make([]string, 0, len(unknownSubtrees))
		for n := range unknownSubtrees {
			names = append(names, n)
		}
		sort.Strings(names)
		slog.Warn("gc mark: unrecognized subtree under storage_root; not scanned",
			"names", names, "count", len(names),
			"hint", "these directories are never marked, swept, or permission-checked; "+
				"rename to a valid mailbox name or move them out of storage_root")
	}

	slog.Info("gc mark complete",
		"blobs_checked", checked,
		"unknown_subtrees", len(unknownSubtrees),
		"marked", marked,
		"skipped_young", skippedYoung,
		"skipped_maintenance", skippedMaintenance,
		"misplaced_blobs", len(misplaced),
		"orphan_tenant_blobs", orphanTenantBlobs,
		"stale_tmp_removed", staleTmp,
		"elapsed", time.Since(start).Truncate(time.Second),
	)
	return EX_OK
}

// gcSweep deletes candidates that are (a) older than the grace period,
// (b) re-confirmed unreferenced by a mark pass strictly later than their
// first sighting (the generation gate — proving the blob stayed orphaned
// across at least one full mark interval), and (c) still unreferenced at
// delete time. The re-check and the unlink run in one transaction holding
// the blob's advisory lock, so a concurrent ingest of the same blob either
// commits first (we see the row and resurrect) or blocks until our unlink
// commits (its EnsureBlobs rewrite then restores the file). See
// storage.BlobAdvisoryLockKey for the full protocol.
func gcSweep(ctx context.Context, db *storage.DB, store *blob.Store, grace time.Duration) int {
	start := time.Now()
	cutoff := time.Now().Add(-grace)

	// generation_marked is the DB-clock epoch of the *last* mark run that found
	// the blob unreferenced (gcMark reads it via SELECT now(), R-045); it and
	// first_seen_at (also the DB clock at first insert) therefore share one
	// timeline, so generation_marked > epoch(first_seen_at) reliably means "a
	// mark pass strictly later than the first sighting re-confirmed it" — no
	// cross-clock skew assumption is involved.
	type cand struct {
		tenant string
		hex    string
		sha    []byte
		kind   string
		bucket string
		seenAt time.Time
	}

	// Process candidates in bounded keyset batches instead of materializing the
	// whole eligible set — deleting a large mailbox can turn every blob into a
	// candidate (hundreds of MB–GB). The cursor is (first_seen_at, tenant,
	// sha256, kind, bucket), binding the sha256 bytea itself so its memcmp
	// ordering matches idx_gc_candidates_sweep exactly. Advancing the
	// cursor past every processed row — including deferred ones whose row
	// survives — keeps the loop terminating; deleted/resurrected rows are gone
	// anyway. The per-candidate lock tx, unlink-before-commit, generation gate,
	// and outcome handling below are unchanged (R-025).
	const sweepBatchSize = 1000
	var deleted, resurrected, deferred, errCount, total, skippedMaintenance int64
	var (
		curSeen    time.Time
		curTenant  string
		curSHA     []byte
		curKind    string
		curBucket  string
		haveCursor bool
	)
	for {
		var (
			rows pgx.Rows
			err  error
		)
		// Order and page on the sha256 bytea column (not encode(...,'hex')) so
		// idx_gc_candidates_sweep (first_seen_at, tenant, sha256, kind, bucket)
		// backs both the range and the keyset — without it, each batch would
		// re-sort the whole table (R-025).
		if haveCursor {
			rows, err = db.Pool().Query(ctx,
				`SELECT tenant, encode(sha256, 'hex'), sha256, kind, bucket, first_seen_at
				   FROM gc_candidates
				  WHERE first_seen_at < $1
				    AND generation_marked > floor(extract(epoch FROM first_seen_at))::bigint
				    AND (first_seen_at, tenant, sha256, kind, bucket) > ($3, $4, $5, $6, $7)
				  ORDER BY first_seen_at, tenant, sha256, kind, bucket
				  LIMIT $2`,
				cutoff, sweepBatchSize, curSeen, curTenant, curSHA, curKind, curBucket)
		} else {
			rows, err = db.Pool().Query(ctx,
				`SELECT tenant, encode(sha256, 'hex'), sha256, kind, bucket, first_seen_at
				   FROM gc_candidates
				  WHERE first_seen_at < $1
				    AND generation_marked > floor(extract(epoch FROM first_seen_at))::bigint
				  ORDER BY first_seen_at, tenant, sha256, kind, bucket
				  LIMIT $2`,
				cutoff, sweepBatchSize)
		}
		if err != nil {
			slog.Error("gc sweep query", "err", err)
			return EX_TEMPFAIL
		}
		var batch []cand
		for rows.Next() {
			var c cand
			if err := rows.Scan(&c.tenant, &c.hex, &c.sha, &c.kind, &c.bucket, &c.seenAt); err != nil {
				rows.Close()
				slog.Error("gc sweep scan", "err", err)
				return EX_TEMPFAIL
			}
			batch = append(batch, c)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			slog.Error("gc sweep rows", "err", err)
			return EX_TEMPFAIL
		}
		rows.Close()
		if len(batch) == 0 {
			break
		}
		total += int64(len(batch))

		for _, c := range batch {
			// Advance the cursor first, before any `continue`, so a
			// persistently-erroring or deferred candidate cannot be re-selected
			// into the next batch and loop forever.
			curSeen, curTenant, curSHA, curKind, curBucket = c.seenAt, c.tenant, c.sha, c.kind, c.bucket
			haveCursor = true
			shaBytes, err := hex.DecodeString(c.hex)
			if err != nil {
				errCount++
				continue
			}
			bucketDate, err := time.Parse("2006/01/02", c.bucket)
			if err != nil {
				errCount++
				slog.Warn("gc sweep bad bucket", "sha", c.hex[:16], "bucket", c.bucket)
				continue
			}
			tenant, terr := blob.ParseTenant(c.tenant)
			if terr != nil {
				errCount++
				slog.Warn("gc sweep bad tenant", "sha", c.hex[:16], "tenant", c.tenant, "err", terr)
				continue
			}
			path, err := store.PathFor(blob.Kind(c.kind), tenant, blob.Bucket(c.bucket), c.hex)
			if err != nil {
				errCount++
				continue
			}

			var outcome string
			err = db.RunTx(ctx, func(tx pgx.Tx) error {
				key := storage.BlobAdvisoryLockKey(c.tenant, c.kind, c.bucket, c.hex)
				if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
					return fmt.Errorf("advisory lock: %w", err)
				}

				// Re-resolve + re-check under the lock, with the SAME logic as
				// mark: an unresolvable tenant (deleted mailbox) means
				// unreferenced, hence deletable — EXCEPT while a rename is in
				// flight, when an unresolvable directory may be a whole
				// account's mail sitting under its new name before the row
				// catches up (RA6X-013).
				//
				// Re-decided here under the blob lock rather than reused from
				// the caller, because mark and sweep must run identical
				// resolution logic and maintenance can start between passes.
				ref, rerr := resolveMailboxID(ctx, tx, c.tenant)
				if rerr != nil {
					return fmt.Errorf("resolve tenant: %w", rerr)
				}
				sweepInMaintenance, rerr := anyMailboxInMaintenance(ctx, tx)
				if rerr != nil {
					return fmt.Errorf("maintenance check: %w", rerr)
				}
				if ref.quiesced || (!ref.ok && sweepInMaintenance) {
					outcome = "skipped_maintenance"
					return nil
				}
				stillReferenced := false
				if ref.ok {
					stillReferenced, rerr = blobReferenced(ctx, tx, blob.Kind(c.kind), ref.id, shaBytes, bucketDate)
					if rerr != nil {
						return fmt.Errorf("recheck: %w", rerr)
					}
				}

				if stillReferenced {
					if _, err := tx.Exec(ctx,
						`DELETE FROM gc_candidates
					  WHERE tenant = $1 AND sha256 = $2 AND kind = $3 AND bucket = $4`,
						c.tenant, shaBytes, c.kind, c.bucket,
					); err != nil {
						return fmt.Errorf("remove resurrected candidate: %w", err)
					}
					outcome = "resurrected"
					return nil
				}

				// Belt-and-suspenders for paths outside the lock protocol
				// (operator copying files into the tree by hand): a recently
				// touched file is deferred to a future sweep.
				if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) < grace {
					outcome = "deferred"
					return nil
				}

				// Unlink BEFORE commit, while the lock is held: an ingest
				// blocked on this lock must observe the file as already gone
				// so its EnsureBlobs rewrite fires. If the commit below fails
				// after the unlink, the surviving candidate row is harmless —
				// the next sweep re-checks, tolerates the missing file, and
				// clears it.
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("unlink: %w", err)
				}
				// Make the unlink durable before the commit asserts "file gone";
				// mirrors blob.Writer's fsync-parent-after-link ordering. Tolerate
				// a parent that a concurrent prune already removed.
				if err := fsyncDir(filepath.Dir(path)); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("fsync parent after unlink: %w", err)
				}
				if _, err := tx.Exec(ctx,
					`DELETE FROM gc_candidates
				  WHERE tenant = $1 AND sha256 = $2 AND kind = $3 AND bucket = $4`,
					c.tenant, shaBytes, c.kind, c.bucket,
				); err != nil {
					return fmt.Errorf("delete candidate: %w", err)
				}
				outcome = "deleted"
				return nil
			})
			if err != nil {
				errCount++
				slog.Warn("gc sweep candidate", "sha", c.hex[:16], "tenant", c.tenant, "bucket", c.bucket, "err", err)
				continue
			}
			switch outcome {
			case "deleted":
				deleted++
				// Best-effort tidy: remove now-empty shard/date dirs up to (not
				// including) the tenant's per-kind subtree root. Runs after the
				// transaction committed and the lock released, so it cannot wedge
				// a delivery; a racing writer just leaves the dir non-empty.
				sub := "raw"
				if blob.Kind(c.kind) == blob.KindAttachment {
					sub = "att"
				}
				pruneEmptyDirs(filepath.Dir(path), filepath.Join(store.Root(), c.tenant, sub))
			case "resurrected":
				resurrected++
			case "deferred":
				deferred++
			case "skipped_maintenance":
				skippedMaintenance++
			}
		}

		if len(batch) < sweepBatchSize {
			break
		}
	}

	slog.Info("gc sweep complete",
		"pending", total,
		"deleted", deleted,
		"resurrected", resurrected,
		"deferred", deferred,
		"skipped_maintenance", skippedMaintenance,
		"errors", errCount,
		"grace", grace,
		"elapsed", time.Since(start).Truncate(time.Second),
	)
	if errCount > 0 {
		// Per-candidate failures were logged above; surface them in the
		// exit code (mirrors gcReconcileQuotas) so cron/periodic wrappers
		// notice instead of reading "success".
		return EX_TEMPFAIL
	}
	return EX_OK
}

// fsyncDir flushes directory metadata so a preceding unlink survives a
// power loss. Same pattern as the blob package's post-link fsync.
func fsyncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// gcReconcileQuotas recomputes mailboxes.used_bytes from SUM(messages.raw_size),
// one transaction per mailbox holding the mailbox row lock (FOR UPDATE). The
// lock is what makes this safe to run hot: a concurrent deliver's in-tx
// `used_bytes = used_bytes + $1` blocks on the row until the reconcile
// commits, then applies its increment on top of the reconciled value — the
// stale-sum lost-update of a single bulk UPDATE cannot happen. Run after
// manual data manipulation or to fix drift.
func gcReconcileQuotas(ctx context.Context, db *storage.DB) int {
	start := time.Now()

	rows, err := db.Pool().Query(ctx, `SELECT id FROM mailboxes ORDER BY id`)
	if err != nil {
		slog.Error("gc reconcile-quotas list mailboxes", "err", err)
		return EX_TEMPFAIL
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			slog.Error("gc reconcile-quotas scan", "err", err)
			return EX_TEMPFAIL
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		slog.Error("gc reconcile-quotas rows", "err", err)
		return EX_TEMPFAIL
	}
	rows.Close()

	var reconciled, drifted, errCount int64
	for _, id := range ids {
		var before, after int64
		err := db.RunTx(ctx, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx,
				`SELECT used_bytes FROM mailboxes WHERE id = $1 FOR UPDATE`,
				id,
			).Scan(&before); err != nil {
				return err
			}
			return tx.QueryRow(ctx,
				`UPDATE mailboxes m
				    SET used_bytes = COALESCE((
				        SELECT SUM(msg.raw_size)
				          FROM messages msg
				          JOIN folders f ON f.id = msg.folder_id
				         WHERE f.mailbox_id = m.id
				    ), 0),
				        updated_at = now()
				  WHERE m.id = $1
				  RETURNING m.used_bytes`,
				id,
			).Scan(&after)
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Mailbox deleted between the list and the lock; fine.
				continue
			}
			errCount++
			slog.Warn("gc reconcile-quotas mailbox", "mailbox_id", id, "err", err)
			continue
		}
		reconciled++
		if before != after {
			drifted++
			slog.Info("quota drift corrected",
				"mailbox_id", id, "before", before, "after", after)
		}
	}

	slog.Info("gc reconcile-quotas complete",
		"mailboxes", reconciled,
		"drift_corrected", drifted,
		"errors", errCount,
		"elapsed", time.Since(start).Truncate(time.Second),
	)
	if errCount > 0 {
		return EX_TEMPFAIL
	}
	return EX_OK
}
