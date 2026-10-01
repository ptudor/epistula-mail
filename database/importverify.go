package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/maildir"
	"github.com/ptudor/epistula-mail/database/maintenance"
	"github.com/ptudor/epistula-mail/database/storage"
)

// runImportVerify confirms that everything in a source Maildir corresponds to
// a messages row in the target folder, AND that every row in the target
// folder has its raw blob on disk with a matching sha256. Zero tolerance for
// drift: any miss is a non-zero exit and a logged offender list.
func runImportVerify(args []string) int {
	fs := flag.NewFlagSet("import-verify", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	maildirPath := fs.String("maildir", "", "Source Maildir directory (required)")
	mailboxName := fs.String("mailbox", "", "Target mailbox name (required)")
	folderName := fs.String("folder", "INBOX", "Target IMAP folder name")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	if *maildirPath == "" || *mailboxName == "" {
		fmt.Fprintln(os.Stderr, "import-verify: -maildir and -mailbox are required")
		return EX_USAGE
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
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres: %v\n", err)
		return EX_TEMPFAIL
	}
	defer db.Close()

	mailboxID, err := db.LookupMailboxByName(ctx, *mailboxName)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "mailbox %q not found\n", *mailboxName)
			return EX_USAGE
		}
		return EX_TEMPFAIL
	}
	// The exact-match lookup succeeded, so *mailboxName is the canonical
	// mailboxes.name — the on-disk blob tenant for every row in this folder.
	tenant, err := blob.ParseTenant(*mailboxName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import-verify: mailbox name %q is not a valid blob tenant: %v\n", *mailboxName, err)
		return EX_USAGE
	}
	// Look up the folder WITHOUT creating it: import-verify is read-only, and
	// auto-creating a missing folder would "verify" it empty and report every
	// source file missing (R-023). An absent folder is a usage error.
	folderID, err := db.LookupFolder(ctx, mailboxID, *folderName)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "import-verify: folder %q does not exist in mailbox %q\n", *folderName, *mailboxName)
			return EX_USAGE
		}
		fmt.Fprintf(os.Stderr, "folder lookup: %v\n", err)
		return EX_TEMPFAIL
	}

	// import-verify only reads; it must never create or chmod the root
	// (RA6X-053).
	store, code := openBlobStoreReadOnly(cfg)
	if code != EX_OK {
		return code
	}
	lease, lerr := maintenance.Acquire(ctx, cfg.Storage.Root, db.Pool(), false, true)
	if lerr != nil {
		fmt.Fprintf(os.Stderr, "offline maintenance barrier: %v\n", lerr)
		return EX_TEMPFAIL
	}
	defer lease.Close()

	start := time.Now()
	missing, walked := 0, 0
	err = maildir.Walk(*maildirPath, func(e maildir.Entry) error {
		walked++
		// Bounded by the configured maximum (RA6X-034). Verification reads
		// the whole archive, so an oversized or special file here is exactly
		// the case that used to exhaust memory mid-pass.
		data, err := readBounded(e.Path, cfg.Limits.MaxMessageBytes)
		if err != nil {
			return fmt.Errorf("read %s: %w", e.Path, err)
		}
		sum := sha256.Sum256(data)
		shaHex := hex.EncodeToString(sum[:])
		exists, err := db.MessageExists(ctx, folderID, shaHex)
		if err != nil {
			return fmt.Errorf("dedup check %s: %w", e.Path, err)
		}
		if !exists {
			missing++
			slog.Error("source file missing from PG", "path", e.Path, "sha256", shaHex[:16])
		}
		return nil
	})
	if err != nil {
		slog.Error("walk", "err", err)
		return EX_OSERR
	}

	// Reverse direction: every PG row for this folder must have its raw blob
	// on disk and the bytes must hash to the stored sha256.
	rows, err := db.Pool().Query(ctx,
		`SELECT id, encode(raw_sha256, 'hex'), raw_size, raw_blob_date
		   FROM messages WHERE folder_id = $1`,
		folderID,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "select messages: %v\n", err)
		return EX_TEMPFAIL
	}
	defer rows.Close()

	blobMissing, blobMismatch, rowsCount := 0, 0, 0
	for rows.Next() {
		var (
			id       int64
			shaHex   string
			rawSize  int64
			blobDate time.Time
		)
		if err := rows.Scan(&id, &shaHex, &rawSize, &blobDate); err != nil {
			fmt.Fprintf(os.Stderr, "scan: %v\n", err)
			return EX_TEMPFAIL
		}
		rowsCount++
		bucket := blob.BucketFromTime(blobDate)
		r, err := store.Open(blob.KindRaw, tenant, bucket, shaHex)
		if err != nil {
			blobMissing++
			slog.Error("blob missing", "message_id", id, "sha256_short", shaHex[:16])
			continue
		}
		h := sha256.New()
		n, err := io.Copy(h, r)
		_ = r.Close()
		if err != nil {
			blobMismatch++
			slog.Error("blob read failed", "message_id", id, "err", err)
			continue
		}
		if n != rawSize {
			blobMismatch++
			slog.Error("blob size mismatch", "message_id", id, "blob_size", n, "row_size", rawSize)
			continue
		}
		got := hex.EncodeToString(h.Sum(nil))
		if got != shaHex {
			blobMismatch++
			slog.Error("blob sha256 mismatch", "message_id", id, "stored", shaHex[:16], "computed", got[:16])
		}
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "rows: %v\n", err)
		return EX_TEMPFAIL
	}

	slog.Info("import-verify complete",
		"maildir_walked", walked,
		"maildir_missing_in_pg", missing,
		"pg_rows", rowsCount,
		"pg_blob_missing", blobMissing,
		"pg_blob_mismatch", blobMismatch,
		"elapsed", time.Since(start).Truncate(time.Second),
	)
	if missing > 0 || blobMissing > 0 || blobMismatch > 0 {
		return EX_DATAERR
	}
	return EX_OK
}
