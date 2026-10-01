package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/maintenance"
	"github.com/ptudor/epistula-mail/database/storage"
)

// adminMailboxMaintenance quiesces (or releases) one mailbox for an operation
// that moves its blob tenant tree — in practice, a rename (RA6X-013).
//
// The mailbox name IS the on-disk tenant path, so renaming is inherently
// two-phase: the operator moves <storage_root>/<old> to <storage_root>/<new>,
// then this tool updates mailboxes.name. In between, the database and the
// filesystem disagree about where the account's blobs live, and every other
// participant will happily act on that disagreement — an IMAP session APPENDing
// under its cached tenant, a delivery resolving the old name, `gc mark` seeing a
// tenant directory that resolves to no mailbox and treating the whole freshly
// moved tree as reapable.
//
// maintenance_at closes that window: while it is set, storage.Ingest refuses
// every write to the mailbox (delivery, IMAP APPEND and import all share that
// path) and GC skips its tenant entirely.
//
// This is NOT mailbox-disable. Disabling rejects authentication but keeps
// accepting delivery, which is exactly the traffic that must stop here.
//
// Maintenance transitions require the exclusive storage-root directory lock.
// All blob processes hold shared locks, so administrators must first stop and
// drain IMAP/API, Postfix, imports, reparse and GC. The durable flag then
// prevents new readers/writers from starting during the filesystem move.
func adminMailboxMaintenance(args []string) int {
	fs := flag.NewFlagSet("admin mailbox-maintenance", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	name := fs.String("name", "", "Mailbox name (required)")
	on := fs.Bool("on", false, "Quiesce the mailbox: refuse all writes and exclude it from GC")
	off := fs.Bool("off", false, "Release the mailbox back to normal service")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	*name = strings.ToLower(strings.TrimSpace(*name))
	if *name == "" {
		fmt.Fprintln(os.Stderr, "-name is required")
		return EX_USAGE
	}
	if *on == *off {
		fmt.Fprintln(os.Stderr, "pass exactly one of -on or -off")
		return EX_USAGE
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return EX_CONFIG
	}
	setupLogging(cfg)

	db, code := adminOpenDB(cfg)
	if code != EX_OK {
		return code
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	lease, lerr := maintenance.Acquire(ctx, cfg.Storage.Root, db.Pool(), true, false)
	if lerr != nil {
		fmt.Fprintf(os.Stderr, "offline maintenance barrier: %v\n", lerr)
		return EX_TEMPFAIL
	}
	defer lease.Close()

	var (
		mailboxID int64
		since     *time.Time
		changed   bool
	)
	err = db.RunTx(ctx, func(tx pgx.Tx) error {
		// The exclusive root lease already drained blob processes. This row
		// lock also serializes administrative changes to durable identity.
		if err := tx.QueryRow(ctx,
			`SELECT id, maintenance_at FROM mailboxes WHERE name = $1 FOR UPDATE`, *name,
		).Scan(&mailboxID, &since); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errMailboxNotFound
			}
			return fmt.Errorf("lookup: %w", err)
		}
		switch {
		case *on && since == nil:
			if _, err := tx.Exec(ctx,
				`UPDATE mailboxes SET maintenance_at = now(), updated_at = now() WHERE id = $1`,
				mailboxID,
			); err != nil {
				return fmt.Errorf("set maintenance: %w", err)
			}
			changed = true
		case *off && since != nil:
			// Releasing after an incomplete/abandoned move would restart readers
			// against the wrong tree. Verify through this transaction's connection.
			missing, _, err := verifyTenantBlobsOn(ctx, tx, mailboxID, cfg.Storage.Root, *name)
			if err != nil {
				return err
			}
			if len(missing) > 0 {
				return fmt.Errorf("cannot release maintenance: referenced blobs are missing/corrupt under %s; restore the tree or complete mailbox-rename", *name)
			}
			if _, err := tx.Exec(ctx,
				`UPDATE mailboxes SET maintenance_at = NULL, updated_at = now() WHERE id = $1`,
				mailboxID,
			); err != nil {
				return fmt.Errorf("clear maintenance: %w", err)
			}
			changed = true
		}
		return nil
	})
	switch {
	case errors.Is(err, errMailboxNotFound):
		fmt.Fprintf(os.Stderr, "mailbox %q not found\n", *name)
		return EX_USAGE
	case err != nil:
		fmt.Fprintf(os.Stderr, "maintenance: %v\n", err)
		return EX_TEMPFAIL
	}

	if !changed {
		if *on {
			fmt.Printf("mailbox %q was already in maintenance (since %s); no change\n",
				*name, since.UTC().Format(time.RFC3339))
		} else {
			fmt.Printf("mailbox %q was not in maintenance; no change\n", *name)
		}
		return EX_OK
	}

	if *on {
		fmt.Printf(`mailbox %q (id=%d) is now quiesced.

  * all blob readers/writers are offline; keep IMAP/API and Postfix stopped
  * new IMAP/API, delivery, import, reparse and GC processes refuse to start
  * existing authenticated sessions ended before the exclusive barrier opened

Release it with:  epistula-database admin mailbox-maintenance -name %s -off
`, *name, mailboxID, *name)
	} else {
		fmt.Printf("mailbox %q (id=%d) released; normal service resumes.\n", *name, mailboxID)
	}
	return EX_OK
}

// mailboxMaintenanceState reports whether the named mailbox is quiesced, for
// commands that require it (mailbox-rename) or must report it.
func mailboxMaintenanceState(ctx context.Context, tx pgx.Tx, mailboxID int64) (*time.Time, error) {
	var since *time.Time
	if err := tx.QueryRow(ctx,
		`SELECT maintenance_at FROM mailboxes WHERE id = $1`, mailboxID,
	).Scan(&since); err != nil {
		return nil, err
	}
	return since, nil
}

// verifyTenantBlobs checks that every blob the database says mailboxID owns is
// present under the tenant tree named destTenant (RA6X-013).
//
// A mailbox rename moves the blob tree by hand, and "the directory arrived" is
// not the same fact as "all of it arrived": an interrupted rsync, a dataset
// that only half replicated, or a move that raced a delivery all leave a tree
// that looks right and is missing mail. The database knows exactly which blobs
// must be there, so ask it before committing the name change, while the
// mailbox is still quiesced and the situation is still recoverable.
//
// Verify size and content hashes during the offline operation, so a directory
// containing unrelated, truncated or equal-size corrupt files is not accepted.
//
// Returns the missing entries (capped, so a wholly absent tree does not build a
// million-line slice) and the number of blobs checked.
func verifyTenantBlobs(ctx context.Context, db *storage.DB, mailboxID int64, storageRoot, destTenant string) ([]string, int64, error) {
	return verifyTenantBlobsOn(ctx, db.Pool(), mailboxID, storageRoot, destTenant)
}

type renameQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func verifyTenantBlobsOn(ctx context.Context, qdb renameQuerier, mailboxID int64, storageRoot, destTenant string) ([]string, int64, error) {
	tenant, err := blob.ParseTenant(destTenant)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid destination tenant %q: %w", destTenant, err)
	}
	store := blob.NewStore(storageRoot)

	const maxReported = 100
	var missing []string
	var checked int64

	// Raw message blobs, then attachment blobs. Both are keyed by
	// (sha256, blob_date) within the tenant.
	for _, q := range []struct {
		kind blob.Kind
		sql  string
	}{
		{blob.KindRaw, `
			SELECT DISTINCT encode(m.raw_sha256, 'hex'), m.raw_blob_date,m.raw_size
			  FROM messages m
			  JOIN folders f ON f.id = m.folder_id
			 WHERE f.mailbox_id = $1`},
		{blob.KindAttachment, `
			SELECT DISTINCT encode(a.sha256, 'hex'), a.blob_date,a.size_bytes
			  FROM attachments a
			  JOIN messages m ON m.id = a.message_id
			  JOIN folders f ON f.id = m.folder_id
			 WHERE f.mailbox_id = $1`},
	} {
		rows, err := qdb.Query(ctx, q.sql, mailboxID)
		if err != nil {
			return nil, checked, fmt.Errorf("list %s blobs: %w", q.kind, err)
		}
		for rows.Next() {
			var shaHex string
			var bucketDate time.Time
			var size int64
			if err := rows.Scan(&shaHex, &bucketDate, &size); err != nil {
				rows.Close()
				return nil, checked, fmt.Errorf("scan %s blob: %w", q.kind, err)
			}
			checked++
			path, err := store.PathFor(q.kind, tenant, blob.BucketFromTime(bucketDate.UTC()), shaHex)
			if err != nil {
				rows.Close()
				return nil, checked, fmt.Errorf("path for %s %s: %w", q.kind, shaHex, err)
			}
			if ok, err := store.ContentMatches(q.kind, tenant, blob.BucketFromTime(bucketDate.UTC()), shaHex, size); err != nil || !ok {
				if len(missing) < maxReported {
					missing = append(missing, path)
				} else if len(missing) == maxReported {
					missing = append(missing, "(further missing blobs not listed)")
				}
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, checked, fmt.Errorf("iterate %s blobs: %w", q.kind, err)
		}
		rows.Close()
	}
	return missing, checked, nil
}
