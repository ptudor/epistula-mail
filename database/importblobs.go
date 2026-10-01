package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/ingest"
	"github.com/ptudor/epistula-mail/database/maildir"
	"github.com/ptudor/epistula-mail/database/maintenance"
	"github.com/ptudor/epistula-mail/database/storage"
)

// importBlobsTenantSep separates the tenant from the in-tenant portion of a
// resume checkpoint key. It is deliberately a control byte lower than every
// character a tenant or bucket can contain, so a single lexical comparison of
// "<tenant>\x1f<bucket>/<sha>" keys matches the on-disk walk order — tenants in
// os.ReadDir (filename) order, then buckets/shards in filepath.Walk lexical
// order. A higher separator (e.g. '/') would mis-order tenants whose names are
// prefixes of one another (e.g. "a" vs "a-b").
const importBlobsTenantSep = "\x1f"

// runImportBlobs is the disk-only disaster-recovery path: it walks the
// per-tenant, date-partitioned raw blob tree
// (<mailbox>/raw/yyyy/mm/dd/aa/bb/<sha>.eml) and re-ingests every message
// through the standard parser back into its OWN mailbox — the owning mailbox
// is recovered from the path's tenant component, so a single run rebuilds every
// mailbox. This is what makes the on-disk tree a self-sufficient recovery
// source when Postgres is lost: the blobs carry the bytes, the tenant carries
// the owner, the bucket path carries the date, and re-import rebuilds the rows.
//
// What it still cannot recover (PG-only state): original folder placement and
// IMAP flags. Everything lands unflagged in the target folder; restoring a
// pg_dump remains the first-choice recovery when one exists. A tenant subtree
// with no backing mailbox row is skipped with a warning (create the mailbox,
// then re-run). -mailbox restricts recovery to a single mailbox subtree.
func runImportBlobs(args []string) int {
	fs := flag.NewFlagSet("import-blobs", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	mailboxName := fs.String("mailbox", "", "Restrict recovery to this one mailbox subtree (optional; default: recover every mailbox found on disk)")
	folderName := fs.String("folder", "INBOX", "Target IMAP folder name")
	checkpointPath := fs.String("checkpoint-file", "", "Resume checkpoint file (default: <storage_root>/.epistula-database-import-blobs.ckpt)")
	resume := fs.Bool("resume", false, "Resume from checkpoint file")
	dryRun := fs.Bool("dry-run", false, "Parse and dedup-check but do not write anything")
	acceptLegacy := fs.Bool("accept-legacy-checkpoint", false,
		"Resume from a checkpoint written before job identity existed (RA6X-031). "+
			"Only safe if you are certain it belongs to this job.")
	retryFailures := fs.Bool("retry-failures", false,
		"Process ONLY the items the previous pass recorded as failed, from the failure manifest")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return EX_CONFIG
	}
	setupLogging(cfg)

	// A dry-run advertises that it writes nothing (RA6X-053).
	store, code := openBlobStoreFor(cfg, *dryRun)
	if code != EX_OK {
		return code
	}

	if *checkpointPath == "" {
		*checkpointPath = store.Root() + "/.epistula-database-import-blobs.ckpt"
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := storage.Open(ctx, storage.Config{
		DSN:              cfg.Postgres.DSN,
		StatementTimeout: cfg.StatementTimeoutDuration(),
		MaxConns:         int32(cfg.Postgres.MaxOpenConns),
		MinConns:         int32(cfg.Postgres.MaxIdleConns),
		ConnMaxLifetime:  cfg.ConnMaxLifetimeDuration(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres: %v\n", err)
		return EX_TEMPFAIL
	}
	defer db.Close()
	lease, lerr := maintenance.Acquire(ctx, cfg.Storage.Root, db.Pool(), false, *dryRun)
	if lerr != nil {
		fmt.Fprintf(os.Stderr, "offline maintenance barrier: %v\n", lerr)
		return EX_TEMPFAIL
	}
	defer lease.Close()

	// Optional single-mailbox filter. When set, it must already exist.
	var filterTenant blob.Tenant
	if *mailboxName != "" {
		t, perr := blob.ParseTenant(*mailboxName)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "import-blobs: mailbox name %q is not a valid blob tenant: %v\n", *mailboxName, perr)
			return EX_USAGE
		}
		if _, err := db.LookupMailboxByName(ctx, *mailboxName); err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				fmt.Fprintf(os.Stderr, "import-blobs: mailbox %q does not exist; create it with `admin mailbox-add` first\n", *mailboxName)
				return EX_USAGE
			}
			fmt.Fprintf(os.Stderr, "lookup mailbox: %v\n", err)
			return EX_TEMPFAIL
		}
		filterTenant = t
	}

	// Salvaging, not refusing (OPS-003): every blob here is mail that was
	// already accepted once, and recovery must not leave any of it out.
	parser := ingest.NewSalvaging(ingest.Limits{
		MaxMessageBytes:       cfg.Limits.MaxMessageBytes,
		MaxMimeDepth:          cfg.Limits.MaxMimeDepth,
		MaxMimeParts:          cfg.Limits.MaxMimeParts,
		MaxHeaderBytes:        cfg.Limits.MaxHeaderBytes,
		MaxHeaderSectionBytes: cfg.Limits.MaxHeaderSectionBytes,
		MaxTransferExpansion:  cfg.Limits.MaxTransferExpansion,
	})

	// Identity includes the -mailbox filter, because a run restricted to one
	// tenant covers a different item set than a full recovery: sharing a
	// checkpoint between them silently skipped everything the narrower run had
	// walked past (RA6X-031).
	job := jobIdentity{
		Kind:        "blob-import",
		Source:      canonicalSource(cfg.Storage.Root),
		Destination: redactedDSN(cfg.Postgres.DSN),
		Mailbox:     *mailboxName,
		Folder:      *folderName,
	}
	if *mailboxName != "" {
		job.Filters = append(job.Filters, "mailbox="+*mailboxName)
	}
	if err := bindJob(ctx, db, &job); err != nil {
		fmt.Fprintf(os.Stderr, "bind import destination: %v\n", err)
		return EX_TEMPFAIL
	}
	state, cerr := loadCheckpoint(*checkpointPath, *resume, job, *acceptLegacy)
	if cerr != nil {
		fmt.Fprintf(os.Stderr, "import-blobs: %v\n", cerr)
		return EX_USAGE
	}

	journal, err := openRecovery(*checkpointPath, job, *dryRun, *retryFailures)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open recovery queue: %v\n", err)
		return EX_IOERR
	}
	defer journal.Close()

	resumeKey := state.resumeKey()
	stats := importStats{startedAt: time.Now()}
	defer func() { stats.report() }()

	// Per-tenant resolution cache: tenant name → its mailbox + target folder.
	// ok=false records a tenant subtree with no backing mailbox row, warned
	// once and skipped thereafter.
	type tenantTarget struct {
		mailboxID int64
		folderID  int64
		ok        bool
	}
	resolved := map[blob.Tenant]tenantTarget{}
	resolveTenant := func(tenant blob.Tenant) (tenantTarget, error) {
		if t, seen := resolved[tenant]; seen {
			return t, nil
		}
		mailboxID, lerr := db.LookupMailboxByName(ctx, string(tenant))
		if lerr != nil {
			if errors.Is(lerr, storage.ErrNotFound) {
				slog.Warn("import-blobs skipping tenant subtree with no mailbox row",
					"tenant", tenant, "hint", "create it with `admin mailbox-add`, then re-run")
				t := tenantTarget{ok: false}
				resolved[tenant] = t
				return t, nil
			}
			return tenantTarget{}, fmt.Errorf("lookup mailbox %q: %w", tenant, lerr)
		}
		// Look up the folder WITHOUT creating it: a pure dry-run, or a real run
		// before its first ingest into this tenant, must not add a folder row
		// (one per tenant would change IMAP LIST for real users — R-023).
		// folderID stays 0 until the first real ingest creates and returns it.
		folderID := int64(0)
		if fid, ferr := db.LookupFolder(ctx, mailboxID, *folderName); ferr == nil {
			folderID = fid
		} else if !errors.Is(ferr, storage.ErrNotFound) {
			return tenantTarget{}, fmt.Errorf("folder for %q: %w", tenant, ferr)
		}
		expected, known := job.Targets[string(tenant)]
		if !known || expected.MailboxID != mailboxID || expected.FolderID != folderID {
			return tenantTarget{}, fmt.Errorf("%w: destination changed for %s", errCheckpointMismatch, tenant)
		}
		t := tenantTarget{mailboxID: mailboxID, folderID: folderID, ok: true}
		resolved[tenant] = t
		return t, nil
	}

	visit := func(kind blob.Kind, tenant blob.Tenant, bucket blob.Bucket, shaHex, path string, info os.FileInfo) error {
		if filterTenant != "" && tenant != filterTenant {
			stats.skippedForeignTenant++
			return nil
		}
		key := string(tenant) + importBlobsTenantSep + string(bucket) + "/" + shaHex
		selected, qerr := journal.selected(key)
		if qerr != nil {
			return qerr
		}
		if !selected {
			return nil
		}
		if !*retryFailures && resumeKey != "" && key <= resumeKey {
			stats.skippedCheckpoint++
			return nil
		}

		tgt, err := resolveTenant(tenant)
		if err != nil {
			return err
		}
		if !tgt.ok {
			stats.skippedNoMailbox++
			return journal.fail(key, "missing_mailbox", storage.ErrNotFound)
		}

		rc, newFolderID, err := importBlobOne(ctx, db, store, parser, importBlobParams{
			MaxMessageBytes: cfg.Limits.MaxMessageBytes,
			Tenant:          tenant,
			Bucket:          bucket,
			SHAHex:          shaHex,
			Path:            path,
			MailboxID:       tgt.mailboxID,
			FolderID:        tgt.folderID,
			FolderName:      *folderName,
			DryRun:          *dryRun,
		})
		switch rc {
		case importOK:
			stats.imported++
		case importDegraded:
			stats.imported++
			stats.degraded++
		case importDuplicate, importDryRunDuplicate:
			stats.duplicates++
		case importDryRunOK:
			stats.dryRunOK++
		case importDryRunDegraded:
			stats.dryRunOK++
			stats.degraded++
		case importParseFailed:
			stats.parseFailures++
			// Recorded durably (RA6X-032). importBlobOne classifies a HASH
			// MISMATCH as a parse failure too, so this manifest is also the
			// record of which blobs do not match their content address.
			if qerr := journal.fail(key, "parse_failed", err); qerr != nil {
				return qerr
			}
			slog.Warn("import-blobs parse failed", "path", path, "err", err)
		default:
			return fmt.Errorf("import-blobs %s: %w", path, err)
		}
		// The first real ingest into this tenant created its folder; cache the
		// id so later blobs under the same tenant dedup accurately (R-023).
		if newFolderID != 0 && tgt.folderID == 0 {
			if err := job.bindCreatedFolder(string(tenant), tgt.mailboxID, newFolderID); err != nil {
				return err
			}
			tgt.folderID = newFolderID
			resolved[tenant] = tgt
			state.Job = job
			state.Fingerprint = job.Fingerprint()
			if err := journal.bind(job); err != nil {
				return err
			}
		}

		if rc != importParseFailed {
			if err := journal.resolved(key); err != nil {
				return err
			}
		}
		state.LastKey = key
		state.Count++
		if state.Count%1000 == 0 {
			if !*dryRun && !*retryFailures {
				if cerr := state.save(*checkpointPath); cerr != nil {
					return fmt.Errorf("checkpoint save: %w", cerr)
				}
			}
			slog.Info("import-blobs progress",
				"count", state.Count,
				"imported", stats.imported,
				"degraded", stats.degraded,
				"duplicates", stats.duplicates,
				"parse_failures", stats.parseFailures,
				"elapsed", time.Since(stats.startedAt).Truncate(time.Second),
			)
		}
		return nil
	}

	if err := store.Walk(blob.KindRaw, visit); err != nil {
		slog.Error("import-blobs walk", "err", err)
		if !*dryRun && !*retryFailures {
			_ = state.save(*checkpointPath)
		}
		return EX_OSERR
	}
	// A dry-run never persists a checkpoint — overwriting the real run's resume
	// marker with a full-walk LastKey would make a later -resume skip every
	// un-imported blob as done (R-023).
	checkpointOK := true
	if !*dryRun && !*retryFailures {
		if err := state.save(*checkpointPath); err != nil {
			slog.Error("final checkpoint save failed", "path", *checkpointPath, "err", err)
			checkpointOK = false
		}
	}
	return recoveryOutcome(journal, checkpointOK)
}

type importBlobParams struct {
	// MaxMessageBytes bounds the local file read, so a corrupt or growing
	// source cannot be allocated whole before the parser sees it (RA6X-034).
	MaxMessageBytes int64
	Tenant          blob.Tenant
	Bucket          blob.Bucket
	SHAHex          string
	Path            string
	MailboxID       int64
	FolderID        int64
	FolderName      string
	DryRun          bool
}

// importBlobOne re-ingests one raw blob. The blob already lives at its
// content-addressed location, so the raw write is skipped; attachment blobs
// are rewritten into the same bucket (a no-op when they already exist).
// Returns the importResult, the folder id created by a real ingest (0 if none),
// and any unrecoverable error.
func importBlobOne(ctx context.Context, db *storage.DB, store *blob.Store, parser *ingest.Parser, p importBlobParams) (importResult, int64, error) {
	// Bounded by the configured maximum (RA6X-034).
	raw, err := readBounded(p.Path, p.MaxMessageBytes)
	if err != nil {
		return importParseFailed, 0, fmt.Errorf("read: %w", err)
	}

	msg, err := parser.Parse(raw)
	if err != nil {
		return importParseFailed, 0, err
	}
	// The filename IS the content hash; a mismatch means the file was
	// corrupted on disk after it was written. Never ingest it under the
	// wrong identity.
	if msg.SHA256Hex != p.SHAHex {
		return importParseFailed, 0, fmt.Errorf("sha mismatch: file %s hashes to %s (disk corruption?)", p.SHAHex, msg.SHA256Hex)
	}

	if p.FolderID != 0 {
		if p.DryRun {
			exists, err := db.MessageExists(ctx, p.FolderID, msg.SHA256Hex)
			if err != nil {
				return importErr, 0, fmt.Errorf("dedup check: %w", err)
			}
			if exists {
				return importDryRunDuplicate, 0, nil
			}
		} else {
			id, err := db.RepairDuplicateInFolder(ctx, p.MailboxID, string(p.Tenant), p.FolderName, p.FolderID, msg.SHA256Hex, duplicateContentRepair(ctx, store, p.Tenant, msg, raw))
			if err != nil {
				return importParseFailed, 0, fmt.Errorf("duplicate integrity: %w", err)
			}
			if id != 0 {
				return importDuplicate, id, nil
			}
		}
	}
	if p.DryRun {
		return parsedResult(msg, true, p.Path), 0, nil
	}

	bucketDate, err := time.Parse("2006/01/02", string(p.Bucket))
	if err != nil {
		return importErr, 0, fmt.Errorf("bucket %q: %w", p.Bucket, err)
	}
	bucketDate = bucketDate.UTC()

	// INTERNALDATE: Date header → earliest Received → the bucket date. The
	// bucket is the best last resort — it was derived from the message's
	// date when the blob was first written.
	internalDate := chooseInternalDate(msg, maildir.Entry{ModTime: bucketDate})

	// Attachment blobs: re-extract and write into the parent's tenant+bucket;
	// the write dedups against blobs that survived alongside the raw tree.
	attParams := make([]storage.AttachmentParams, 0, len(msg.Attachments))
	for _, att := range msg.Attachments {
		w, err := store.NewWriter(blob.KindAttachment, p.Tenant, p.Bucket)
		if err != nil {
			return importErr, 0, fmt.Errorf("att writer: %w", err)
		}
		if _, err := w.Write(att.Data); err != nil {
			_ = w.Abort()
			return importErr, 0, fmt.Errorf("att write: %w", err)
		}
		sha, _, _, err := w.Close()
		if err != nil {
			return importErr, 0, fmt.Errorf("att close: %w", err)
		}
		attParams = append(attParams, storage.AttachmentParams{
			PartNumber:  att.PartNumber,
			Filename:    att.Filename,
			ContentType: att.ContentType,
			ContentID:   att.ContentID,
			Disposition: att.Disposition,
			Size:        att.Size,
			SHA256Hex:   sha,
			BlobDate:    bucketDate,
		})
	}

	res, err := db.Ingest(ctx, storage.IngestParams{
		MailboxID:         p.MailboxID,
		MailboxName:       string(p.Tenant),
		FolderName:        p.FolderName,
		MustExistFolderID: p.FolderID,
		EnvelopeTo:        ingest.SanitizeUTF8("import-blobs:" + p.SHAHex[:16]),
		RawSHA256Hex:      msg.SHA256Hex,
		RawSize:           int64(len(raw)),
		RawBlobDate:       bucketDate,
		Message:           msg,
		Attachments:       attParams,
		InternalDate:      &internalDate,
		Outcome:           "imported",
		IgnoreQuota:       true, // disaster recovery restores existing mail; never bounce on quota (R-029)
		DedupOnRawSHA:     true, // close the concurrent-import race in-tx (R-044)
		EnsureBlobs:       ensureBlobsFunc(store, p.Tenant, p.Bucket, msg.SHA256Hex, raw, msg.Attachments, attParams),
	})
	if err != nil {
		if errors.Is(err, storage.ErrDuplicate) {
			// A concurrent winner must satisfy the same integrity boundary as
			// a duplicate discovered before ingest, including historical refs.
			id, err := db.RepairDuplicateInFolder(ctx, p.MailboxID, string(p.Tenant), p.FolderName, p.FolderID, msg.SHA256Hex, duplicateContentRepair(ctx, store, p.Tenant, msg, raw))
			if err != nil {
				return importParseFailed, 0, fmt.Errorf("duplicate integrity: %w", err)
			}
			if id == 0 {
				return importParseFailed, 0, fmt.Errorf("duplicate disappeared before integrity verification; retry item")
			}
			return importDuplicate, id, nil
		}
		return importErr, 0, fmt.Errorf("ingest: %w", err)
	}
	return parsedResult(msg, false, p.Path), res.FolderID, nil
}
