package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/mail"
	"os"
	"path/filepath"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/ingest"
	"github.com/ptudor/epistula-mail/database/maildir"
	"github.com/ptudor/epistula-mail/database/maintenance"
	"github.com/ptudor/epistula-mail/database/storage"
)

// runImport walks a Maildir tree and ingests every message through the same
// parser the LDA uses. Idempotent on (folder_id, raw_sha256). Resumable via
// a JSON checkpoint file.
func runImport(args []string) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	maildirPath := fs.String("maildir", "", "Source Maildir directory (required)")
	mailboxName := fs.String("mailbox", "", "Target mailbox name (required)")
	folderName := fs.String("folder", "INBOX", "Target IMAP folder name")
	checkpointPath := fs.String("checkpoint-file", "", "Resume checkpoint file (default: <maildir>/.epistula-database-import.ckpt)")
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
	if *maildirPath == "" || *mailboxName == "" {
		fmt.Fprintln(os.Stderr, "import: -maildir and -mailbox are required")
		return EX_USAGE
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return EX_CONFIG
	}
	setupLogging(cfg)

	if *checkpointPath == "" {
		*checkpointPath = filepath.Join(*maildirPath, ".epistula-database-import.ckpt")
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

	mailboxID, err := db.LookupMailboxByName(ctx, *mailboxName)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "import: mailbox %q does not exist; create it with `admin mailbox-add` first\n", *mailboxName)
			return EX_USAGE
		}
		fmt.Fprintf(os.Stderr, "lookup mailbox: %v\n", err)
		return EX_TEMPFAIL
	}
	// The exact-match lookup above succeeded, so *mailboxName is the canonical
	// mailboxes.name — the on-disk blob tenant. Validate it as a path-safe
	// component up front (a non-conforming legacy name is a config error to fix
	// before importing, not something to discover mid-walk).
	tenant, err := blob.ParseTenant(*mailboxName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: mailbox name %q is not a valid blob tenant: %v\n", *mailboxName, err)
		return EX_USAGE
	}
	// A folder an earlier `admin folder-merge` emptied, and prune removed, is
	// imported into the merge's destination (migration 021). Re-creating it
	// instead would store every message the merge already moved a second
	// time, because dedup is per folder. A folder that exists is imported
	// into as named, so `admin folder-create` first opts out.
	target, redirected, rerr := db.ResolveImportFolder(ctx, mailboxID, *folderName)
	switch {
	case errors.Is(rerr, storage.ErrFolderRedirectCycle):
		fmt.Fprintf(os.Stderr, "import: %v; undo one of the merges or create the folder to import into it as named\n", rerr)
		return EX_DATAERR
	case rerr != nil:
		fmt.Fprintf(os.Stderr, "folder lookup: %v\n", rerr)
		return EX_TEMPFAIL
	}
	if redirected {
		slog.Info("target folder was merged away; importing into the merge destination",
			"folder", *folderName, "into", target)
		*folderName = target
	}

	// Look up the target folder WITHOUT creating it. A pure dry-run must not
	// add a folder row (R-023); a real run defers creation to the first ingest,
	// which upserts the folder in its own transaction. folderID stays 0 until
	// then, and dedup treats a not-yet-created folder as holding no messages.
	folderID := int64(0)
	if fid, ferr := db.LookupFolder(ctx, mailboxID, *folderName); ferr == nil {
		folderID = fid
	} else if !errors.Is(ferr, storage.ErrNotFound) {
		fmt.Fprintf(os.Stderr, "folder lookup: %v\n", ferr)
		return EX_TEMPFAIL
	}

	// A dry-run advertises that it writes nothing, so it must not create or
	// chmod the storage root either (RA6X-053).
	store, code := openBlobStoreFor(cfg, *dryRun)
	if code != EX_OK {
		return code
	}
	lease, lerr := maintenance.Acquire(ctx, cfg.Storage.Root, db.Pool(), false, *dryRun)
	if lerr != nil {
		fmt.Fprintf(os.Stderr, "offline maintenance barrier: %v\n", lerr)
		return EX_TEMPFAIL
	}
	defer lease.Close()

	// Salvaging, not refusing (OPS-003): this is existing mail being moved
	// into the store, and a message left out is lost once the source is
	// retired. A message that reaches a limit is kept with what was read
	// within it; none of the limits is raised.
	parser := ingest.NewSalvaging(ingest.Limits{
		MaxMessageBytes:       cfg.Limits.MaxMessageBytes,
		MaxMimeDepth:          cfg.Limits.MaxMimeDepth,
		MaxMimeParts:          cfg.Limits.MaxMimeParts,
		MaxHeaderBytes:        cfg.Limits.MaxHeaderBytes,
		MaxHeaderSectionBytes: cfg.Limits.MaxHeaderSectionBytes,
		MaxTransferExpansion:  cfg.Limits.MaxTransferExpansion,
	})

	job := jobIdentity{
		Kind:        "maildir-import",
		Source:      canonicalSource(*maildirPath),
		Destination: redactedDSN(cfg.Postgres.DSN),
		Mailbox:     *mailboxName,
		Folder:      *folderName,
	}
	if err := bindJob(ctx, db, &job); err != nil {
		fmt.Fprintf(os.Stderr, "bind import destination: %v\n", err)
		return EX_TEMPFAIL
	}
	if target, ok := job.Targets[*mailboxName]; !ok || target.MailboxID != mailboxID || target.FolderID != folderID {
		fmt.Fprintln(os.Stderr, "import: destination changed during startup; retry from the beginning")
		return EX_TEMPFAIL
	}
	state, cerr := loadCheckpoint(*checkpointPath, *resume, job, *acceptLegacy)
	if cerr != nil {
		fmt.Fprintf(os.Stderr, "import: %v\n", cerr)
		return EX_USAGE
	}

	// -retry-failures processes exactly the items the previous pass could not,
	// so a corrected parser or a restored source can be applied without
	// re-walking (and re-deduping) an entire archive (RA6X-032).
	journal, err := openRecovery(*checkpointPath, job, *dryRun, *retryFailures)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open recovery queue: %v\n", err)
		return EX_IOERR
	}
	defer journal.Close()

	stats := importStats{startedAt: time.Now()}
	defer func() { stats.report() }()

	resumeKey := state.resumeKey()
	err = maildir.Walk(*maildirPath, func(e maildir.Entry) error {
		selected, qerr := journal.selected(e.Key())
		if qerr != nil {
			return qerr
		}
		if !selected {
			return nil
		}
		if !*retryFailures && resumeKey != "" && e.Key() <= resumeKey {
			stats.skippedCheckpoint++
			return nil
		}

		rc, newFolderID, err := importOne(ctx, db, store, parser, importOneParams{
			MaxMessageBytes: cfg.Limits.MaxMessageBytes,
			Entry:           e,
			MailboxID:       mailboxID,
			Tenant:          tenant,
			FolderID:        folderID,
			FolderName:      *folderName,
			DryRun:          *dryRun,
		})
		switch rc {
		case importOK:
			stats.imported++
		case importDegraded:
			stats.imported++
			stats.degraded++
		case importDuplicate:
			stats.duplicates++
		case importDryRunOK:
			stats.dryRunOK++
		case importDryRunDegraded:
			stats.dryRunOK++
			stats.degraded++
		case importDryRunDuplicate:
			stats.duplicates++
		case importParseFailed:
			stats.parseFailures++
			// Recorded durably, not just counted (RA6X-032). The checkpoint
			// advances past this file, so without a manifest a later resume
			// would never retry it — even after the parser was fixed or the
			// source restored — and the pass would still exit EX_OK.
			if qerr := journal.fail(e.Key(), "parse_failed", err); qerr != nil {
				return qerr
			}
			slog.Warn("import parse failed", "path", e.Path, "err", err)
			// No `err = nil` here: err is never read after this switch, so the
			// assignment was dead and merely read as if it controlled flow —
			// misleading a maintainer who later added a post-switch error
			// check into thinking parse failures were already neutralised
			// (RO5X-026). This arm falls through and the walk continues.
		default:
			return fmt.Errorf("import %s: %w", e.Path, err)
		}
		// The first real ingest created the folder; cache its id so subsequent
		// dedup checks in this run are accurate (R-023 deferred creation).
		if newFolderID != 0 {
			if err := job.bindCreatedFolder(*mailboxName, mailboxID, newFolderID); err != nil {
				return err
			}
			folderID = newFolderID
			state.Job = job
			state.Fingerprint = job.Fingerprint()
			if err := journal.bind(job); err != nil {
				return err
			}
		}

		if rc != importParseFailed {
			if err := journal.resolved(e.Key()); err != nil {
				return err
			}
		}
		state.LastKey = e.Key()
		state.Count++
		if state.Count%1000 == 0 {
			if !*dryRun && !*retryFailures {
				if cerr := state.save(*checkpointPath); cerr != nil {
					return fmt.Errorf("checkpoint save: %w", cerr)
				}
			}
			slog.Info("import progress",
				"count", state.Count,
				"imported", stats.imported,
				"degraded", stats.degraded,
				"duplicates", stats.duplicates,
				"parse_failures", stats.parseFailures,
				"elapsed", time.Since(stats.startedAt).Truncate(time.Second),
			)
		}
		return nil
	})
	if err != nil {
		slog.Error("import walk", "err", err)
		if !*dryRun && !*retryFailures {
			_ = state.save(*checkpointPath)
		}
		return EX_OSERR
	}

	// Never persist a checkpoint for a dry-run: it would overwrite the real
	// import's resume marker with a full-walk LastKey, so a later -resume would
	// skip every un-imported message as done (R-023). Dry-run keeps stats in
	// memory only.
	checkpointOK := true
	if !*dryRun && !*retryFailures {
		if err := state.save(*checkpointPath); err != nil {
			// A checkpoint that could not be written is not a successful
			// recovery pass: the next resume will redo work, or worse, the
			// operator will believe a marker exists that does not (RA6X-032).
			slog.Error("final checkpoint save failed", "path", *checkpointPath, "err", err)
			checkpointOK = false
		}
	}

	return recoveryOutcome(journal, checkpointOK)
}

// Recovery bookkeeping is part of success. Outstanding failures from an
// earlier pass remain visible even when -resume skips their source keys.
//
// A dry run reports what the same real run would leave unresolved, with the
// exit code that run would have. Its queue on disk is unchanged, so the
// message says what would remain rather than what is there (OPS-005).
func recoveryOutcome(journal *recoveryJournal, checkpointOK bool) int {
	failures, err := journal.finish()
	if err != nil {
		slog.Error("failure manifest publication failed", "err", err)
		return EX_IOERR
	}
	if !checkpointOK {
		return EX_IOERR
	}
	if failures > 0 {
		retry := "repair the source and run -retry-failures with the same job and manifest/checkpoint path; missing retry sources remain unresolved"
		if journal.job.Kind == "gc-staging" {
			retry = "repair the staging permissions/filesystem and rerun gc -phase mark with the same -manifest path"
		}
		if journal.dry {
			fmt.Fprintf(os.Stderr, "dry run: %d item(s) would remain unresolved in %s; %s.\n", failures, journal.dir, retry)
		} else {
			fmt.Fprintf(os.Stderr, "%d unresolved item(s) in %s; %s.\n", failures, journal.dir, retry)
		}
		return EX_TEMPFAIL
	}
	return EX_OK
}

type importStats struct {
	startedAt time.Time
	imported  int64
	// degraded counts the imported (or, in a dry run, importable) messages
	// the parser had to salvage; they are included in imported/dryRunOK too
	// (OPS-003).
	degraded          int64
	duplicates        int64
	parseFailures     int64
	skippedCheckpoint int64
	dryRunOK          int64
	// import-blobs only: blobs under a tenant excluded by the -mailbox filter,
	// and blobs under a tenant subtree with no backing mailbox row.
	skippedForeignTenant int64
	skippedNoMailbox     int64
}

func (s importStats) report() {
	slog.Info("import complete",
		"imported", s.imported,
		"degraded", s.degraded,
		"duplicates", s.duplicates,
		"parse_failures", s.parseFailures,
		"skipped_checkpoint", s.skippedCheckpoint,
		"skipped_foreign_tenant", s.skippedForeignTenant,
		"skipped_no_mailbox", s.skippedNoMailbox,
		"dry_run_ok", s.dryRunOK,
		"elapsed", time.Since(s.startedAt).Truncate(time.Second),
	)
}

type importResult int

const (
	importErr importResult = iota
	importOK
	importDuplicate
	importDryRunOK
	importDryRunDuplicate
	importParseFailed
	// importDegraded and importDryRunDegraded are importOK and
	// importDryRunOK for a message the parser salvaged (OPS-003).
	importDegraded
	importDryRunDegraded
)

// parsedResult is the success result for a message that parsed, telling a
// salvaged parse apart from a clean one and logging what was salvaged.
func parsedResult(msg *ingest.Message, dryRun bool, path string) importResult {
	if len(msg.Defects) == 0 {
		if dryRun {
			return importDryRunOK
		}
		return importOK
	}
	slog.Info("import degraded parse", "path", path,
		"raw_sha256_short", msg.SHA256Hex[:16], "defects", msg.DefectSummary())
	if dryRun {
		return importDryRunDegraded
	}
	return importDegraded
}

type importOneParams struct {
	// MaxMessageBytes bounds the local file read, so a corrupt or growing
	// source cannot be allocated whole before the parser sees it (RA6X-034).
	MaxMessageBytes int64
	Entry           maildir.Entry
	MailboxID       int64
	Tenant          blob.Tenant // canonical mailbox name; the on-disk blob subtree
	FolderID        int64
	FolderName      string
	DryRun          bool
}

// importOne reads one Maildir file, parses it, dedups, and persists.
// Returns the importResult kind for stats accounting, the folder id created by
// a real ingest or verified duplicate (0 for dry-run or failure), and any
// unrecoverable error (which should halt the walk).
func importOne(ctx context.Context, db *storage.DB, store *blob.Store, parser *ingest.Parser, p importOneParams) (importResult, int64, error) {
	// Bounded by the configured maximum, and refusing anything that is not a
	// regular file (RA6X-034): os.ReadFile allocated the whole file before the
	// parser could enforce MaxMessageBytes, and a FIFO in the source tree
	// blocked the import indefinitely.
	raw, err := readBounded(p.Entry.Path, p.MaxMessageBytes)
	if err != nil {
		return importParseFailed, 0, fmt.Errorf("read: %w", err)
	}

	msg, err := parser.Parse(raw)
	if err != nil {
		return importParseFailed, 0, err
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
		return parsedResult(msg, true, p.Entry.Path), 0, nil
	}

	// Bucket = the same date the importer uses for INTERNALDATE: Date header
	// → Received → file mtime → now. Co-locates the message and its parts
	// under one daily dir, so an "import the 2003 archive" run lands every
	// 2003 file under raw/2003/...
	internalDate := chooseInternalDate(msg, p.Entry)
	bucket := blob.BucketFromTime(internalDate)
	rw, err := store.NewWriter(blob.KindRaw, p.Tenant, bucket)
	if err != nil {
		return importErr, 0, fmt.Errorf("blob writer: %w", err)
	}
	if _, err := rw.Write(raw); err != nil {
		_ = rw.Abort()
		return importErr, 0, fmt.Errorf("blob write: %w", err)
	}
	rawSHA, _, _, err := rw.Close()
	if err != nil {
		return importErr, 0, fmt.Errorf("blob close: %w", err)
	}

	// Attachments share the parent message's bucket.
	attParams := make([]storage.AttachmentParams, 0, len(msg.Attachments))
	for _, att := range msg.Attachments {
		w, err := store.NewWriter(blob.KindAttachment, p.Tenant, bucket)
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
			BlobDate:    internalDate,
		})
	}

	flags := p.Entry.Flags.IMAP()

	res, err := db.Ingest(ctx, storage.IngestParams{
		MailboxID:         p.MailboxID,
		MailboxName:       string(p.Tenant),
		FolderName:        p.FolderName,
		MustExistFolderID: p.FolderID,
		EnvelopeFrom:      "",
		EnvelopeTo:        ingest.SanitizeUTF8("import:" + p.Entry.BaseName),
		RawSHA256Hex:      rawSHA,
		RawSize:           int64(len(raw)),
		RawBlobDate:       internalDate,
		Message:           msg,
		Attachments:       attParams,
		InternalDate:      &internalDate,
		Flags:             flags,
		Outcome:           "imported",
		IgnoreQuota:       true, // migration restores existing mail; never bounce on quota (R-029)
		DedupOnRawSHA:     true, // close the concurrent-import race in-tx (R-044)
		EnsureBlobs:       ensureBlobsFunc(store, p.Tenant, bucket, rawSHA, raw, msg.Attachments, attParams),
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
	return parsedResult(msg, false, p.Entry.Path), res.FolderID, nil
}

// chooseInternalDate prefers Date: header, then the earliest Received: line,
// then the file mtime, then now() as the last fallback.
func chooseInternalDate(msg *ingest.Message, e maildir.Entry) time.Time {
	if !msg.SentDate.IsZero() {
		return msg.SentDate.UTC()
	}
	if vs := msg.Headers["Received"]; len(vs) > 0 {
		for _, v := range vs {
			if t, ok := parseReceivedTime(v); ok {
				return t.UTC()
			}
		}
	}
	if !e.ModTime.IsZero() {
		return e.ModTime.UTC()
	}
	return time.Now().UTC()
}

// parseReceivedTime extracts the date suffix from a Received: header line.
// Format: "by foo from bar ...; <RFC 5322 date>".
func parseReceivedTime(received string) (time.Time, bool) {
	idx := -1
	for i := len(received) - 1; i >= 0; i-- {
		if received[i] == ';' {
			idx = i
			break
		}
	}
	if idx < 0 {
		return time.Time{}, false
	}
	t, err := mail.ParseDate(received[idx+1:])
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// checkpoint is the resume marker the importer writes periodically. The key
// is maildir.Entry.Key ("<sub>/<basename>") — stable across runs no matter
// how -maildir was spelled (trailing slash, relative path, symlink).
// LastPath is read for checkpoints written before the key form existed and
// is no longer written.
type checkpoint struct {
	// Version is the shape version. Absent (0) means a file written before
	// job identity existed (RA6X-031).
	Version int `json:"version,omitempty"`
	// Job describes WHICH job produced this marker, and Fingerprint is the
	// value compared. Without them a checkpoint was an unqualified "I got this
	// far", and pointing a second import at the same path silently skipped
	// every key the first job had already passed — an incomplete import that
	// looked exactly like a successful resume.
	Job         jobIdentity `json:"job,omitempty"`
	Fingerprint string      `json:"fingerprint,omitempty"`

	LastKey   string    `json:"last_key,omitempty"`
	LastPath  string    `json:"last_path,omitempty"` // legacy; superseded by LastKey
	Count     int64     `json:"count"`
	StartedAt time.Time `json:"started_at"`
}

// resumeKey returns the entry key to resume after, deriving it from a
// legacy absolute-path checkpoint when necessary.
func (c checkpoint) resumeKey() string {
	if c.LastKey != "" {
		return c.LastKey
	}
	if c.LastPath == "" {
		return ""
	}
	dir, base := filepath.Split(filepath.Clean(c.LastPath))
	return filepath.Base(filepath.Clean(dir)) + "/" + base
}

// loadCheckpoint reads the resume marker and verifies it belongs to THIS job
// (RA6X-031).
//
// A mismatch is refused, not ignored and not silently skipped past: skipping is
// exactly what turned a reused checkpoint into an import that reported success
// having processed a fraction of its input. The error names both jobs so an
// operator can see which checkpoint they pointed at.
//
// allowLegacy governs a marker written before identity existed. Its job cannot
// be established, so resuming from it is a guess; the operator must say
// explicitly that the guess is safe.
func loadCheckpoint(path string, resume bool, job jobIdentity, allowLegacy bool) (checkpoint, error) {
	fresh := checkpoint{Version: checkpointVersion, Job: job, Fingerprint: job.Fingerprint(), StartedAt: time.Now()}
	if !resume {
		return fresh, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("checkpoint read failed", "path", path, "err", err)
		}
		return fresh, nil
	}
	var c checkpoint
	if err := json.Unmarshal(data, &c); err != nil {
		// A corrupt checkpoint is not a reason to skip anything. Start over
		// rather than resume from a marker nobody can read.
		slog.Warn("checkpoint parse failed; starting from the beginning", "path", path, "err", err)
		return fresh, nil
	}

	switch {
	case c.Version > checkpointVersion:
		return checkpoint{}, fmt.Errorf("%w: unsupported checkpoint version %d", errCheckpointMismatch, c.Version)
	case c.Version < checkpointVersion || c.Fingerprint == "":
		if !allowLegacy {
			return checkpoint{}, fmt.Errorf(
				"%w: %s predates job identity, so which job wrote it cannot be established.\n"+
					"  this job: %s\n"+
					"Re-run without -resume to start over, or pass -accept-legacy-checkpoint "+
					"if you are certain it belongs to this job",
				errCheckpointMismatch, path, job)
		}
		slog.Warn("resuming from a legacy checkpoint whose job identity cannot be verified",
			"path", path, "job", job.String())
	case c.Fingerprint != job.Fingerprint() || c.Fingerprint != c.Job.Fingerprint():
		return checkpoint{}, fmt.Errorf(
			"%w:\n  checkpoint %s was written by: %s\n  this job is:                %s\n"+
				"Resuming would skip items this job has not processed. Use a different "+
				"-checkpoint path, or re-run without -resume",
			errCheckpointMismatch, path, c.Job, job)
	}

	c.Version = checkpointVersion
	c.Job = job
	c.Fingerprint = job.Fingerprint()
	slog.Info("import resumed", "last_key", c.resumeKey(), "count", c.Count, "job", job.String())
	return c, nil
}

// save writes the checkpoint atomically. The temporary name includes the
// process id so two concurrent jobs pointed at neighbouring paths cannot
// clobber each other's staging file (RA6X-031); the rename itself is atomic.
func (c checkpoint) save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeDurableFile(path, data)
}
