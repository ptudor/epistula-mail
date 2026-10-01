package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/ingest"
	"github.com/ptudor/epistula-mail/database/maintenance"
	"github.com/ptudor/epistula-mail/database/storage"
)

// runReparseBodystructure re-derives messages.bodystructure from each row's
// raw blob and rewrites it.
//
// This is the RO5X-003 back-fill. bodystructure is pure derived data: it is
// computed at ingest and never read back by the writer, so re-deriving it
// from the immutable raw blob is always safe and always idempotent. Rows
// written before the size-units fix carry the DECODED part length where the
// RFC requires the encoded one, which makes every MUA under-report base64
// attachment sizes; rows written before the message/rfc822 fix are unaffected
// on disk (that half is a read-side conversion) but re-parsing them is
// harmless.
//
// Deliberately a separate subcommand rather than a migration: it reads every
// raw blob off disk, so it is I/O-bound over the whole archive and must not
// run inside `migrate up`'s budget. It is resumable, safe to run hot, and
// takes no locks beyond the single-row UPDATE.
func runReparseBodystructure(args []string) int {
	fs := flag.NewFlagSet("reparse-bodystructure", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	mailboxName := fs.String("mailbox", "", "Restrict to one mailbox (optional; default: every mailbox)")
	batchSize := fs.Int("batch", 500, "Rows to scan per query")
	dryRun := fs.Bool("dry-run", false, "Report what would change without writing")
	manifestPath := fs.String("manifest", "reparse-bodystructure", "Path prefix for durable failed-item queue/report")
	retryFailures := fs.Bool("retry-failures", false, "Retry only recorded failed message IDs")
	limit := fs.Int("limit", 0, "Stop after scanning this many rows (0 = no limit)")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	if *batchSize < 1 {
		fmt.Fprintln(os.Stderr, "reparse-bodystructure: -batch must be >= 1")
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

	if *mailboxName != "" {
		if _, err := db.LookupMailboxByName(ctx, *mailboxName); err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				fmt.Fprintf(os.Stderr, "reparse-bodystructure: mailbox %q does not exist\n", *mailboxName)
				return EX_USAGE
			}
			fmt.Fprintf(os.Stderr, "lookup mailbox: %v\n", err)
			return EX_TEMPFAIL
		}
	}

	// Salvaging (OPS-003): every row here is a message the store already
	// holds, so the pass must re-derive what it can from each one rather
	// than skip the ones a limit would refuse at the door.
	parser := ingest.NewSalvaging(ingest.Limits{
		MaxMessageBytes:       cfg.Limits.MaxMessageBytes,
		MaxMimeDepth:          cfg.Limits.MaxMimeDepth,
		MaxMimeParts:          cfg.Limits.MaxMimeParts,
		MaxHeaderBytes:        cfg.Limits.MaxHeaderBytes,
		MaxHeaderSectionBytes: cfg.Limits.MaxHeaderSectionBytes,
		MaxTransferExpansion:  cfg.Limits.MaxTransferExpansion,
	})

	if *limit < 0 {
		fmt.Fprintf(os.Stderr, "reparse-bodystructure: -limit must not be negative (%d); 0 means no limit\n", *limit)
		return EX_USAGE
	}
	if *batchSize <= 0 {
		fmt.Fprintf(os.Stderr, "reparse-bodystructure: -batch must be positive (%d)\n", *batchSize)
		return EX_USAGE
	}

	job := jobIdentity{Kind: "reparse-bodystructure", Source: canonicalSource(cfg.Storage.Root), Destination: redactedDSN(cfg.Postgres.DSN), Mailbox: *mailboxName}
	if err := bindJob(ctx, db, &job); err != nil {
		slog.Error("bind reparse destination", "err", err)
		return EX_TEMPFAIL
	}
	journal, err := openRecovery(*manifestPath, job, *dryRun, *retryFailures)
	if err != nil {
		slog.Error("open reparse failure queue", "err", err)
		return EX_IOERR
	}
	defer journal.Close()
	st, err := reparseAll(ctx, db, store, parser, reparseParams{
		Mailbox:         *mailboxName,
		BatchSize:       *batchSize,
		DryRun:          *dryRun,
		Limit:           int64(*limit),
		MaxMessageBytes: cfg.Limits.MaxMessageBytes,
		Journal:         journal,
	})
	if err != nil {
		slog.Error("reparse-bodystructure failed", "err", err)
		return EX_TEMPFAIL
	}

	slog.Info("reparse-bodystructure complete",
		"scanned", st.Scanned,
		"rewritten", st.Rewritten,
		"unchanged", st.Unchanged,
		"blob_missing", st.BlobMissing,
		"parse_failed", st.ParseFailed,
		"dry_run", *dryRun,
	)
	// A retry key whose row was expunged has no remaining work. A surviving
	// row with an unreadable blob remains a failure, including if a limit kept
	// this pass from reaching it. A dry run reconciles too: it only reads, and
	// without it the dry run reported expunged rows the real run would clear
	// (OPS-005).
	if err := journal.each(func(item failedItem) error {
		id, err := strconv.ParseInt(item.Key, 10, 64)
		if err != nil {
			return err
		}
		var exists bool
		if err := db.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE id=$1)`, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return journal.resolved(item.Key)
		}
		return nil
	}); err != nil {
		slog.Error("reconcile failed IDs", "err", err)
		return EX_IOERR
	}
	return recoveryOutcome(journal, true)
}

type reparseParams struct {
	Journal   *recoveryJournal
	Mailbox   string
	BatchSize int
	DryRun    bool
	Limit     int64
	// MaxMessageBytes bounds the raw-blob read (RA6X-034).
	MaxMessageBytes int64
}

// reparseStats is the run summary.
type reparseStats struct {
	Scanned     int
	Rewritten   int
	Unchanged   int
	BlobMissing int
	ParseFailed int
}

// reparseAll walks messages in id order, re-deriving bodystructure from each
// row's raw blob. Keyset pagination on id keeps it O(page) and resumable: a
// re-run picks up any row still carrying a stale value.
func reparseAll(ctx context.Context, db *storage.DB, store *blob.Store, parser *ingest.Parser, p reparseParams) (reparseStats, error) {
	var st reparseStats
	var afterID int64

	for {
		// The query is capped by the REMAINING allowance, not by the batch
		// size (RA6X-033). p.Limit used to be checked only after the inner
		// loop had processed a whole batch, so `reparse-bs -limit 1` scanned —
		// and, live, REWROTE — up to the default 500 rows. An operator's small
		// repair therefore modified hundreds of records beyond the boundary
		// the help text promises, in dry-run as well as live.
		batchSize := p.BatchSize
		if p.Limit > 0 {
			remaining := p.Limit - int64(st.Scanned)
			if remaining <= 0 {
				return st, nil
			}
			if remaining < int64(batchSize) {
				batchSize = int(remaining)
			}
		}
		rows, err := db.Pool().Query(ctx, `
			SELECT m.id, mb.name, encode(m.raw_sha256, 'hex'), m.raw_blob_date, m.bodystructure,
			       COALESCE(m.text_body, ''), COALESCE(m.html_body, ''),
			       m.sent_date_local,
			       ARRAY(SELECT a.part_number FROM attachments a WHERE a.message_id = m.id)
			  FROM messages m
			  JOIN folders f ON f.id = m.folder_id
			  JOIN mailboxes mb ON mb.id = f.mailbox_id
			 WHERE m.id > $1
			   AND ($2 = '' OR mb.name = $2)
			 ORDER BY m.id
			 LIMIT $3`,
			afterID, p.Mailbox, batchSize,
		)
		if err != nil {
			return st, fmt.Errorf("scan messages: %w", err)
		}

		type row struct {
			id       int64
			mailbox  string
			shaHex   string
			blobDate time.Time
			current  []byte
			// The derived data this pass also repairs (RA6X-005/010/049 change
			// what a re-parse produces for these; RA6X-048 adds sentLocal;
			// OPS-004 renumbers attachment parts).
			textBody  string
			htmlBody  string
			sentLocal *time.Time
			attParts  []string
		}
		var batch []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.mailbox, &r.shaHex, &r.blobDate, &r.current,
				&r.textBody, &r.htmlBody, &r.sentLocal, &r.attParts); err != nil {
				rows.Close()
				return st, fmt.Errorf("scan row: %w", err)
			}
			batch = append(batch, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return st, fmt.Errorf("scan messages: %w", err)
		}
		if len(batch) == 0 {
			return st, nil
		}

		for _, r := range batch {
			// Checked BEFORE the row is scanned, not after the batch, so the
			// allowance means what the help text says: rows scanned.
			if p.Limit > 0 && int64(st.Scanned) >= p.Limit {
				return st, nil
			}
			afterID = r.id
			st.Scanned++
			key := strconv.FormatInt(r.id, 10)
			if p.Journal != nil {
				selected, err := p.Journal.selected(key)
				if err != nil {
					return st, err
				}
				if !selected {
					continue
				}
			}
			fail := func(reason string, cause error) error {
				var exists bool
				if err := db.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE id=$1)`, r.id).Scan(&exists); err != nil {
					return err
				}
				if !exists {
					if p.Journal != nil {
						return p.Journal.resolved(key)
					}
					return nil
				}
				if reason == "parse_failed" {
					st.ParseFailed++
				} else {
					st.BlobMissing++
				}
				if p.Journal != nil {
					return p.Journal.fail(key, reason, cause)
				}
				return nil
			}
			resolved := func() error {
				if p.Journal != nil {
					return p.Journal.resolved(key)
				}
				return nil
			}

			tenant, terr := blob.ParseTenant(r.mailbox)
			if terr != nil {
				// The mailboxes.name CHECK makes this unreachable in a
				// healthy store; treat it as a skip, not a failure.
				slog.Warn("reparse: mailbox name is not a valid tenant",
					"message_id", r.id, "mailbox", r.mailbox, "err", terr)
				if err := fail("invalid_tenant", terr); err != nil {
					return st, err
				}
				continue
			}
			if _, derr := hex.DecodeString(r.shaHex); derr != nil {
				slog.Warn("reparse: bad sha", "message_id", r.id, "err", derr)
				if err := fail("invalid_digest", derr); err != nil {
					return st, err
				}
				continue
			}

			path, perr := store.PathFor(blob.KindRaw, tenant, blob.BucketFromTime(r.blobDate), r.shaHex)
			if perr != nil {
				slog.Warn("reparse: path", "message_id", r.id, "err", perr)
				if err := fail("invalid_path", perr); err != nil {
					return st, err
				}
				continue
			}
			// Bounded by the configured maximum (RA6X-034).
			raw, rerr := readBounded(path, p.MaxMessageBytes)
			if rerr != nil {
				// A missing blob is an operator-visible store problem, but it
				// must not abort a whole back-fill pass over a large archive.
				slog.Warn("reparse: raw blob unreadable",
					"message_id", r.id, "sha16", shortSHA(r.shaHex), "err", rerr)
				if err := fail("raw_unreadable", rerr); err != nil {
					return st, err
				}
				continue
			}

			// Verify the raw bytes really are the message this row names,
			// BEFORE deriving anything from them (RA6X-035). The digest string
			// was checked for syntax and the file was read, but nothing
			// confirmed the two matched — so syntactically valid corruption
			// would be parsed and its structure written over the message's
			// real metadata, spreading a storage fault into the database where
			// no later check would catch it.
			sum := sha256.Sum256(raw)
			if hex.EncodeToString(sum[:]) != r.shaHex {
				slog.Error("reparse: raw blob does not match its content address; refusing to derive metadata from it",
					"message_id", r.id, "sha16", shortSHA(r.shaHex), "path", path,
					"on_disk_bytes", len(raw))
				if err := fail("raw_hash_mismatch", fmt.Errorf("raw content differs from %s", r.shaHex)); err != nil {
					return st, err
				}
				continue
			}

			msg, perr2 := parser.Parse(raw)
			if perr2 != nil {
				slog.Warn("reparse: parse failed",
					"message_id", r.id, "sha16", shortSHA(r.shaHex), "err", perr2)
				if err := fail("parse_failed", perr2); err != nil {
					return st, err
				}
				continue
			}

			next, jerr := json.Marshal(msg.BodyStructure)
			if jerr != nil {
				return st, fmt.Errorf("marshal bodystructure for message %d: %w", r.id, jerr)
			}

			// The pass repairs DERIVED DATA, not just the structure.
			//
			// The MIME fixes in RA6X-005/010/049 change what a re-parse
			// produces for mail already stored: quoted-printable part sizes and
			// encodings, an untyped part that used to be classified as a binary
			// attachment, and a text part explicitly dispositioned as one. The
			// structure alone was rewritten before, so those rows kept a
			// projection and an attachment set that disagreed with it. RA6X-048
			// adds the sender's calendar date, which only a Go RFC 5322 parse
			// can recover. OPS-004 renumbers the attachments of a message whose
			// own Content-Type is message/rfc822, and of an enclosed message
			// typed as a bare "multipart". The structure and the number of
			// attachments stay the same, so the numbers themselves are compared.
			//
			// Raw blobs, message ids, UIDs, dates and flags are NOT touched:
			// this rewrites what was computed from the message, never the
			// message.
			var nextSentLocal any
			if msg.SentDateLocal != "" {
				nextSentLocal = msg.SentDateLocal
			}
			changed := !jsonEquivalent(r.current, next) ||
				msg.TextBody != r.textBody ||
				msg.HTMLBody != r.htmlBody ||
				!sameAttachmentParts(r.attParts, msg.Attachments) ||
				sentLocalDiffers(r.sentLocal, msg.SentDateLocal)
			if !changed {
				st.Unchanged++
				if err := resolved(); err != nil {
					return st, err
				}
				continue
			}
			if p.DryRun {
				// Counted as the real run's successful repair would be, so a
				// recorded failure this pass would now repair is not reported
				// as still unresolved (OPS-005).
				st.Rewritten++
				slog.Debug("reparse: would rewrite", "message_id", r.id)
				if err := resolved(); err != nil {
					return st, err
				}
				continue
			}
			if uerr := repairDerived(ctx, db, store, tenant, r.blobDate, r.id,
				next, msg, nextSentLocal); uerr != nil {
				if errors.Is(uerr, pgx.ErrNoRows) {
					// Expunged between the scan and the update; nothing to do.
					if err := resolved(); err != nil {
						return st, err
					}
					continue
				}
				if err := fail("repair_failed", uerr); err != nil {
					return st, err
				}
				continue
			}
			st.Rewritten++
			if err := resolved(); err != nil {
				return st, err
			}
		}

		if p.Limit > 0 && int64(st.Scanned) >= p.Limit {
			return st, nil
		}
	}
}

// jsonEquivalent reports whether two JSON documents carry the same value,
// ignoring key order and formatting — so a row already holding the correct
// structure is not rewritten just because pgx handed it back with different
// whitespace.
func jsonEquivalent(a, b []byte) bool {
	if len(a) == 0 {
		return false
	}
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return jsonDeepEqual(av, bv)
}

func jsonDeepEqual(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			ov, ok := bv[k]
			if !ok || !jsonDeepEqual(v, ov) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !jsonDeepEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}

// shortSHA truncates a hex digest for logging without assuming its length.
func shortSHA(s string) string {
	if len(s) >= 16 {
		return s[:16]
	}
	return s
}

// sameAttachmentParts reports whether a message's stored attachment rows carry
// exactly the part numbers a fresh parse derives, compared as multisets. That
// includes the number of rows, which is what RA6X-049 changed, and a part
// number repeated by OPS-006's duplicate rows.
func sameAttachmentParts(stored []string, parsed []ingest.Attachment) bool {
	if len(stored) != len(parsed) {
		return false
	}
	derived := make([]string, len(parsed))
	for i, a := range parsed {
		derived[i] = a.PartNumber
	}
	have := slices.Clone(stored)
	slices.Sort(have)
	slices.Sort(derived)
	return slices.Equal(have, derived)
}

// sentLocalDiffers reports whether a stored sent_date_local disagrees with what
// a fresh parse produces (RA6X-048). An unparseable or absent Date header
// yields "", which must match a stored NULL.
func sentLocalDiffers(stored *time.Time, parsed string) bool {
	if stored == nil {
		return parsed != ""
	}
	return stored.Format("2006-01-02") != parsed
}

// repairDerived rewrites everything a re-parse produces for one message, in one
// transaction (RA6X-005/010/048/049).
//
// The pass used to rewrite `bodystructure` alone, which was enough while a
// re-parse could only change the structure. It no longer is: the MIME fixes
// change the text projection (an untyped part is text now, not an opaque
// attachment), the attachment SET (a text part explicitly dispositioned as an
// attachment is one now), and the sender's calendar date. A row repaired only
// in its structure would carry a projection and an attachment list that
// disagree with it, which is a worse state than the one being repaired.
//
// Raw blobs, message ids, UIDs, internal dates and flags are never touched:
// this rewrites what was COMPUTED from the message, never the message.
func repairDerived(
	ctx context.Context,
	db *storage.DB,
	store *blob.Store,
	tenant blob.Tenant,
	blobDate time.Time,
	messageID int64,
	nextBS []byte,
	msg *ingest.Message,
	sentLocal any,
) error {
	bucket := blob.BucketFromTime(blobDate.UTC())

	// Attachment blobs the new parse produces. Their content addresses are
	// computed from the decoded bytes, exactly as ingest computes them.
	type att struct {
		params storage.AttachmentParams
		data   []byte
	}
	atts := make([]att, 0, len(msg.Attachments))
	lockKeys := make([]int64, 0, len(msg.Attachments))
	for _, a := range msg.Attachments {
		sum := sha256.Sum256(a.Data)
		shaHex := hex.EncodeToString(sum[:])
		atts = append(atts, att{
			params: storage.AttachmentParams{
				PartNumber:  a.PartNumber,
				Filename:    a.Filename,
				ContentType: a.ContentType,
				ContentID:   a.ContentID,
				Disposition: a.Disposition,
				Size:        a.Size,
				SHA256Hex:   shaHex,
				BlobDate:    blobDate,
			},
			data: a.Data,
		})
		lockKeys = append(lockKeys, storage.BlobAdvisoryLockKey(
			string(tenant), string(blob.KindAttachment), string(bucket), shaHex))
	}
	sort.Slice(lockKeys, func(i, j int) bool { return lockKeys[i] < lockKeys[j] })

	return db.RunTx(ctx, func(tx pgx.Tx) error {
		// A maintenance repair is a writer too. Pin durable mailbox identity
		// before the blob locks, so a rename cannot move the tenant tree while
		// this pass creates newly derived attachment references.
		var name string
		var maintenance *time.Time
		if err := tx.QueryRow(ctx, `SELECT mb.name, mb.maintenance_at
		    FROM mailboxes mb JOIN folders f ON f.mailbox_id = mb.id
		    JOIN messages m ON m.folder_id = f.id WHERE m.id = $1
		    FOR UPDATE OF mb`, messageID).Scan(&name, &maintenance); err != nil {
			return err
		}
		if maintenance != nil || name != string(tenant) {
			return fmt.Errorf("mailbox identity changed or is in maintenance; rerun repair after maintenance")
		}

		// Join the GC protocol before creating references to attachment blobs,
		// in the same sorted order ingest uses, so a concurrent sweep
		// serializes against this transaction rather than unlinking a blob
		// this pass is about to point a row at.
		var prev int64
		for i, k := range lockKeys {
			if i > 0 && k == prev {
				continue
			}
			prev = k
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, k); err != nil {
				return fmt.Errorf("acquire blob lock: %w", err)
			}
		}
		for _, a := range atts {
			shaBytes, err := hex.DecodeString(a.params.SHA256Hex)
			if err != nil {
				return fmt.Errorf("attachment sha: %w", err)
			}
			if _, err := tx.Exec(ctx,
				`DELETE FROM gc_candidates
				  WHERE tenant = $1 AND sha256 = $2 AND kind = $3 AND bucket = $4`,
				string(tenant), shaBytes, string(blob.KindAttachment), string(bucket),
			); err != nil {
				return fmt.Errorf("clear gc candidate: %w", err)
			}
			// EnsureContent verifies the existing blob against its content
			// address and rewrites it from these bytes if it does not match
			// (RA6X-035); a genuinely new attachment is simply written.
			if _, err := store.EnsureContent(blob.KindAttachment, tenant, bucket,
				a.params.SHA256Hex, a.data); err != nil {
				return fmt.Errorf("write attachment blob: %w", err)
			}
		}

		tag, err := tx.Exec(ctx,
			`UPDATE messages
			    SET bodystructure = $1,
			        text_body = NULLIF($3, ''),
			        html_body = NULLIF($4, ''),
			        sent_date_local = $5::date
			  WHERE id = $2`,
			nextBS, messageID, msg.TextBody, msg.HTMLBody, sentLocal)
		if err != nil {
			return fmt.Errorf("update message: %w", err)
		}
		if tag.RowsAffected() == 0 {
			// Expunged between the scan and here.
			return pgx.ErrNoRows
		}

		// Replace the attachment rows wholesale. Reconciling them one by one
		// would need a stable key, and part numbers are exactly what the MIME
		// fixes change — so the old rows are the thing being corrected, not a
		// key to match against.
		if _, err := tx.Exec(ctx, `DELETE FROM attachments WHERE message_id = $1`, messageID); err != nil {
			return fmt.Errorf("clear attachments: %w", err)
		}
		for _, a := range atts {
			shaBytes, err := hex.DecodeString(a.params.SHA256Hex)
			if err != nil {
				return fmt.Errorf("attachment sha: %w", err)
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO attachments (
					message_id, part_number, filename, content_type,
					content_id, disposition, size_bytes, sha256, blob_date
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
				messageID, a.params.PartNumber,
				nullIfEmpty(a.params.Filename), a.params.ContentType,
				nullIfEmpty(a.params.ContentID), nullIfEmpty(a.params.Disposition),
				a.params.Size, shaBytes, a.params.BlobDate,
			); err != nil {
				return fmt.Errorf("insert attachment %s: %w", a.params.PartNumber, err)
			}
		}
		return nil
	})
}

// nullIfEmpty renders an empty string as SQL NULL, matching how ingest writes
// these optional attachment fields.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
