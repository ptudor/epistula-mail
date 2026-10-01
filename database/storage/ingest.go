package storage

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/ingest"
)

// IngestParams carries everything needed to persist one delivered message.
// Blob bytes must already be written to disk before calling Ingest; this
// transaction only records pointers.
type IngestParams struct {
	MailboxID int64
	// MailboxName is the canonical mailboxes.name — the tenant the blobs are
	// stored under on disk. It must match exactly what the blob Writer used
	// and what the GC walker reads back from the directory name, so the
	// per-blob advisory lock key and the gc_candidates rows agree across
	// ingest and sweep. Required (blobs are physically partitioned by it).
	MailboxName string
	AliasID     *int64
	FolderName  string
	// MustExistFolderID, when non-zero, names the exact folder this ingest
	// must land in, and forbids creating one (RA6X-047).
	//
	// The default (zero) keeps the auto-create behaviour the LDA and the
	// importers need: the first delivery to a fresh mailbox has to create
	// INBOX. IMAP APPEND has the opposite policy — RFC 9051 §6.3.12 wants
	// NO [TRYCREATE] for a folder that does not exist — and used to enforce it
	// with a lookup before the transaction, discarding the id. Between that
	// lookup and the upsert, a folder deleted or renamed by another session
	// was silently recreated under its old name and the message landed in it.
	//
	// Carrying the ID rather than re-resolving the name is what makes it
	// atomic: the folder is locked by identity, so a concurrent rename moves
	// the message with the folder (which is what the user asked for) and a
	// concurrent delete fails the APPEND rather than resurrecting the name.
	MustExistFolderID int64
	EnvelopeFrom      string
	EnvelopeTo        string

	RawSHA256Hex string
	RawSize      int64
	// RawBlobDate is the on-disk bucket the raw blob was written under.
	// If zero, it defaults to time.Now().UTC() (live delivery).
	RawBlobDate time.Time

	Message     *ingest.Message
	Attachments []AttachmentParams

	// Optional overrides used by the import path. Live delivery leaves them
	// at their zero values.
	InternalDate *time.Time // nil = time.Now().UTC()
	Flags        []string   // nil = empty IMAP flag set
	// Outcome is the delivery_log verb; "" = "delivered". A message the
	// parser had to degrade (Message.Defects non-empty) is logged as
	// "<outcome>:degraded" with the defects in error_detail, so an operator
	// can tell a salvaged parse from a clean one (OPS-003).
	Outcome string

	// IgnoreQuota bypasses over-quota enforcement. Set by the migration/import
	// paths, which restore existing mail and must not bounce it. Live delivery
	// and IMAP APPEND leave it false so quotas are enforced (R-029).
	IgnoreQuota bool

	// DedupOnRawSHA makes Ingest re-check, inside the transaction and under the
	// per-blob advisory lock, whether a message with this raw_sha256 already
	// exists in the target folder; if so it returns ErrDuplicate and rolls back.
	// The import paths set it to close the check-then-act race between two
	// concurrent runs of the same tree (the pre-tx MessageExists alone lets both
	// insert). Deliver and IMAP APPEND leave it false — identical bytes twice
	// are legitimately two messages (R-044).
	DedupOnRawSHA bool

	// EnsureBlobs re-verifies that every blob this message references is
	// still present on disk, rewriting any that are missing. Ingest calls
	// it inside the transaction, after taking the per-blob advisory locks,
	// so a concurrent `gc sweep` cannot unlink a blob between the check and
	// the commit (see BlobAdvisoryLockKey). Callers that hold the raw
	// message and attachment bytes (deliver, import, IMAP APPEND) should
	// always set this; nil skips the verification.
	EnsureBlobs func(ctx context.Context) error

	// OnCommitted records acceptance immediately after COMMIT succeeds.
	OnCommitted func()
}

// AttachmentParams is one attachment row to insert. The blob with this
// sha256 MUST already exist on disk under BlobDate's bucket.
type AttachmentParams struct {
	PartNumber  string
	Filename    string
	ContentType string
	ContentID   string
	Disposition string
	Size        int64
	SHA256Hex   string
	// BlobDate is the on-disk bucket the attachment blob was written
	// under. If zero, defaults to the message's RawBlobDate so the
	// attachment is co-located with its parent.
	BlobDate time.Time
}

// IngestResult is what Ingest returns on success.
type IngestResult struct {
	MessageID int64
	FolderID  int64
	UID       int64
	ModSeq    int64 // the highest_modseq value stamped on this row (RFC 7162)
	// UIDValidity is the destination folder's uidvalidity, read inside the
	// same transaction that allocated UID (RA6X-047). IMAP APPEND needs the
	// pair to be consistent for its UIDPLUS response, and a second query after
	// commit could report a validity from a later rename — or zero, if the
	// folder had been deleted by then.
	UIDValidity int64
}

// Ingest atomically persists one delivered message: ensures the target folder
// exists, allocates the next IMAP UID, inserts the messages row + attachments
// rows, bumps the mailbox used_bytes, writes a delivery_log row, and sends a
// pg_notify on mail_arrived. The whole thing is one transaction.
func (db *DB) Ingest(ctx context.Context, p IngestParams) (IngestResult, error) {
	if p.MailboxID == 0 {
		return IngestResult{}, errors.New("storage.Ingest: MailboxID is required")
	}
	tenant, err := blob.ParseTenant(p.MailboxName)
	if err != nil {
		return IngestResult{}, fmt.Errorf("storage.Ingest: invalid MailboxName %q: %w", p.MailboxName, err)
	}
	if p.FolderName == "" {
		p.FolderName = "INBOX"
	}
	if p.Message == nil {
		return IngestResult{}, errors.New("storage.Ingest: Message is required")
	}
	if p.RawSHA256Hex == "" {
		return IngestResult{}, errors.New("storage.Ingest: RawSHA256Hex is required")
	}
	rawSHA, err := hex.DecodeString(p.RawSHA256Hex)
	if err != nil {
		return IngestResult{}, fmt.Errorf("invalid RawSHA256Hex: %w", err)
	}

	headersJSON, err := json.Marshal(p.Message.Headers)
	if err != nil {
		return IngestResult{}, fmt.Errorf("marshal headers: %w", err)
	}
	bsJSON, err := json.Marshal(p.Message.BodyStructure)
	if err != nil {
		return IngestResult{}, fmt.Errorf("marshal bodystructure: %w", err)
	}

	// Effective blob dates are needed before the transaction starts so the
	// advisory-lock keys and gc_candidates rows can be derived from the
	// exact (kind, bucket, sha) coordinates the blobs live under.
	// Normalize to UTC up front so the advisory-lock key, the gc_candidates
	// bucket, and the raw_blob_date DATE column all derive from the SAME
	// calendar date as the on-disk bucket (blob.BucketFromTime is UTC). A
	// future caller passing a zoned time near midnight would otherwise write
	// the blob under the UTC bucket but store the local date — readers would
	// 404 the blob and gc could reap a referenced one (R-064).
	rawBlobDate := p.RawBlobDate
	if rawBlobDate.IsZero() {
		rawBlobDate = time.Now()
	}
	rawBlobDate = rawBlobDate.UTC()
	type blobRef struct {
		kind   string
		bucket string
		sha    []byte
	}
	refs := []blobRef{{
		kind:   string(blob.KindRaw),
		bucket: string(blob.BucketFromTime(rawBlobDate)),
		sha:    rawSHA,
	}}
	for _, att := range p.Attachments {
		attSHA, err := hex.DecodeString(att.SHA256Hex)
		if err != nil {
			return IngestResult{}, fmt.Errorf("attachment %s: invalid sha256: %w", att.PartNumber, err)
		}
		attDate := att.BlobDate
		if attDate.IsZero() {
			attDate = rawBlobDate
		}
		attDate = attDate.UTC()
		refs = append(refs, blobRef{
			kind:   string(blob.KindAttachment),
			bucket: string(blob.BucketFromTime(attDate)),
			sha:    attSHA,
		})
	}
	lockKeys := make([]int64, len(refs))
	for i, r := range refs {
		lockKeys[i] = BlobAdvisoryLockKey(string(tenant), r.kind, r.bucket, hex.EncodeToString(r.sha))
	}

	var result IngestResult
	err = db.RunTxCommitted(ctx, func(tx pgx.Tx) error {
		// Pin the mailbox's durable identity for the whole transaction before
		// anything is written (RA6X-013).
		//
		// This proves that `tenant` — the on-disk tree the blobs were already
		// written under, derived from a name the caller may have resolved long
		// ago (an IMAP session holds one from LOGIN until disconnect) — is
		// still where this mailbox lives. Without it, an APPEND in flight
		// across `admin mailbox-rename` writes its blob under the old tenant
		// and its row into the renamed mailbox: acknowledged, permanently
		// unreadable mail.
		//
		// FOR UPDATE rather than FOR SHARE, and taken FIRST. The transaction
		// already ends up holding this row exclusively — `UPDATE mailboxes SET
		// used_bytes` below — so a shared lock here would be a lock UPGRADE,
		// and two concurrent deliveries to one mailbox would each hold the
		// share and wait forever for the other to release it. (The integration
		// suite catches this immediately: TestIngestConcurrentUIDAllocation
		// deadlocked with SQLSTATE 40P01.) Acquiring the exclusive lock up
		// front costs nothing extra in contention terms, because deliveries to
		// one mailbox already serialize on that UPDATE, and it establishes the
		// mailbox → folder → messages order every other writer follows.
		//
		// It is also what makes the barrier a barrier: `admin mailbox-rename`
		// takes the same row FOR UPDATE, so it cannot commit while an ingest is
		// in flight, and no ingest can begin after it without reading the new
		// name here.
		var currentName string
		var maintenanceAt *time.Time
		if err := tx.QueryRow(ctx,
			`SELECT name, maintenance_at FROM mailboxes WHERE id = $1 FOR UPDATE`,
			p.MailboxID,
		).Scan(&currentName, &maintenanceAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: mailbox %d no longer exists", ErrMailboxIdentityChanged, p.MailboxID)
			}
			return fmt.Errorf("pin mailbox identity: %w", err)
		}
		if maintenanceAt != nil {
			return fmt.Errorf("%w: mailbox %q since %s",
				ErrMailboxInMaintenance, currentName, maintenanceAt.UTC().Format(time.RFC3339))
		}
		if currentName != p.MailboxName {
			return fmt.Errorf("%w: resolved %q, now %q", ErrMailboxIdentityChanged, p.MailboxName, currentName)
		}

		// GC coordination, in strict order: (1) take the per-blob advisory
		// locks so a concurrent sweep serializes against this transaction,
		// (2) clear any gc_candidates rows — these blobs are referenced
		// again, (3) re-verify the files survived any sweep that ran before
		// we held the locks, rewriting from memory if not.
		if err := acquireBlobLocks(ctx, tx, lockKeys); err != nil {
			return fmt.Errorf("acquire blob locks: %w", err)
		}
		for _, r := range refs {
			if _, err := tx.Exec(ctx,
				`DELETE FROM gc_candidates
				  WHERE tenant = $1 AND sha256 = $2 AND kind = $3 AND bucket = $4`,
				string(tenant), r.sha, r.kind, r.bucket,
			); err != nil {
				return fmt.Errorf("clear gc candidate: %w", err)
			}
		}
		if p.EnsureBlobs != nil {
			if err := p.EnsureBlobs(ctx); err != nil {
				return fmt.Errorf("ensure blobs on disk: %w", err)
			}
		}

		var folderID, uid, modSeq, uidValidity int64
		if p.MustExistFolderID != 0 {
			// Must-exist policy (IMAP APPEND): take the folder by IDENTITY.
			// Bumping uidnext and highest_modseq in the same statement leaves
			// the row locked for the transaction, so a concurrent DELETE of
			// this folder serializes behind the commit rather than racing it,
			// and the uidvalidity returned belongs to the same committed
			// generation as the UID (RA6X-047).
			if err := tx.QueryRow(ctx,
				`UPDATE folders
				   SET uidnext        = uidnext + 1,
				       highest_modseq = highest_modseq + 1
				 WHERE id = $1 AND mailbox_id = $2
				 RETURNING id, uidnext - 1, highest_modseq, uidvalidity`,
				p.MustExistFolderID, p.MailboxID,
			).Scan(&folderID, &uid, &modSeq, &uidValidity); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("%w: folder %d", ErrFolderGone, p.MustExistFolderID)
				}
				return fmt.Errorf("allocate UID: %w", err)
			}
		} else {
			// Auto-create policy (LDA, importers). EnsureFolder is the one
			// folder-creation path every writer shares: it creates missing
			// hierarchical ancestors first, so importing into
			// `Sent/2004/11-Nov` also creates `Sent` and `Sent/2004` instead of
			// leaving a child LIST shows without its parents (OPS-001), and it
			// consumes a UIDVALIDITY only when a row is genuinely created
			// (RA6X-052). Its lock precondition holds: the mailbox row has been
			// FOR UPDATE since the top of this transaction.
			folder, _, err := EnsureFolder(ctx, tx, p.MailboxID, p.FolderName, nil)
			if err != nil {
				return fmt.Errorf("ensure folder: %w", err)
			}

			// Bump uidnext AND highest_modseq atomically so the new
			// message stamps a fresh modseq value the IMAP CONDSTORE
			// surface can advertise.
			if err := tx.QueryRow(ctx,
				`UPDATE folders
				   SET uidnext        = uidnext + 1,
				       highest_modseq = highest_modseq + 1
				 WHERE id = $1 AND mailbox_id = $2
				 RETURNING id, uidnext - 1, highest_modseq, uidvalidity`,
				folder.ID, p.MailboxID,
			).Scan(&folderID, &uid, &modSeq, &uidValidity); err != nil {
				return fmt.Errorf("allocate UID: %w", err)
			}
		}
		// IMAP carries UID and UIDVALIDITY in 32-bit fields, while the columns
		// are BIGINT and the sequence is unrestricted (RA6X-052). A wrapped
		// conversion would emit zero — which the protocol forbids — or reuse
		// an identifier a client has cached, so the allocation is refused
		// before anything is committed rather than after it is on the wire.
		if err := CheckProtocolID("uid", uid); err != nil {
			return err
		}
		// Reserve a representable UIDNEXT; the final 32-bit value is the
		// exhaustion sentinel, not another assignable UID.
		if err := CheckProtocolID("uidnext", uid+1); err != nil {
			return err
		}
		if err := CheckProtocolID("uidvalidity", uidValidity); err != nil {
			return err
		}

		result.FolderID = folderID
		result.UID = uid
		result.ModSeq = modSeq
		result.UIDValidity = uidValidity

		// Import dedup backstop (R-044): re-check for an existing row with this
		// raw_sha256 in the folder, now that we hold the per-blob advisory lock
		// (acquired above) and know the folder id. A concurrent import of the
		// same message is serialized on that lock, so the loser sees the winner's
		// committed row here and rolls back instead of inserting a duplicate. The
		// wasted uidnext bump above is undone by the rollback. Only imports set
		// this; deliver/APPEND allow identical bytes as distinct messages.
		if p.DedupOnRawSHA {
			var exists bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM messages WHERE folder_id = $1 AND raw_sha256 = $2)`,
				folderID, rawSHA,
			).Scan(&exists); err != nil {
				return fmt.Errorf("dedup recheck: %w", err)
			}
			if exists {
				return ErrDuplicate
			}
		}

		internalDate := time.Now().UTC()
		if p.InternalDate != nil {
			internalDate = p.InternalDate.UTC()
		}
		var sentDate any
		if !p.Message.SentDate.IsZero() {
			sentDate = p.Message.SentDate
		}
		// The sender's own calendar date, stored alongside the instant so
		// SENTSINCE/SENTBEFORE can answer what RFC 9051 §6.4.4 actually asks
		// (RA6X-048). NULL when the message carried no usable Date header.
		var sentDateLocal any
		if p.Message.SentDateLocal != "" {
			sentDateLocal = p.Message.SentDateLocal
		}
		flags := p.Flags
		if flags == nil {
			flags = []string{}
		}
		// to_addrs / cc_addrs are NOT NULL; pgx writes a nil []string as
		// SQL NULL (which DEFAULT can't rescue mid-INSERT), so coerce
		// here. Most messages without explicit To/Cc still have at least
		// the envelope recipient threaded through deliver, but APPEND can
		// legitimately produce nil slices.
		toAddrs := p.Message.To
		if toAddrs == nil {
			toAddrs = []string{}
		}
		ccAddrs := p.Message.Cc
		if ccAddrs == nil {
			ccAddrs = []string{}
		}

		var messageID int64
		if err := tx.QueryRow(ctx,
			`INSERT INTO messages (
				folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date,
				message_id, in_reply_to, subject, from_addr,
				to_addrs, cc_addrs, sent_date, sent_date_local, headers,
				text_body, html_body, bodystructure, flags, mod_seq
			) VALUES (
				$1, $2, $3, $4, $5, $6,
				$7, $8, $9, $10,
				$11, $12, $13, $14::date, $15,
				$16, $17, $18, $19, $20
			)
			RETURNING id`,
			folderID, uid, rawSHA, rawBlobDate, p.RawSize, internalDate,
			nullStr(p.Message.MessageID),
			nullStr(p.Message.InReplyTo),
			nullStr(p.Message.Subject),
			nullStr(p.Message.From),
			toAddrs, ccAddrs, sentDate, sentDateLocal, headersJSON,
			nullStr(p.Message.TextBody), nullStr(p.Message.HTMLBody), bsJSON, flags, modSeq,
		).Scan(&messageID); err != nil {
			return fmt.Errorf("insert message: %w", err)
		}
		result.MessageID = messageID

		for _, att := range p.Attachments {
			attSHA, err := hex.DecodeString(att.SHA256Hex)
			if err != nil {
				return fmt.Errorf("attachment %s: invalid sha256: %w", att.PartNumber, err)
			}
			attDate := att.BlobDate
			if attDate.IsZero() {
				attDate = rawBlobDate
			}
			attDate = attDate.UTC()
			if _, err := tx.Exec(ctx,
				`INSERT INTO attachments (
					message_id, part_number, filename, content_type,
					content_id, disposition, size_bytes, sha256, blob_date
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
				messageID, att.PartNumber,
				nullStr(att.Filename), att.ContentType,
				nullStr(att.ContentID), nullStr(att.Disposition),
				att.Size, attSHA, attDate,
			); err != nil {
				return fmt.Errorf("insert attachment %s: %w", att.PartNumber, err)
			}
		}

		// Queue the message for the annotation pipeline (migration 022). In
		// this transaction, so a stored message is never unmarked and a
		// rolled-back one leaves no marker behind.
		if _, err := tx.Exec(ctx,
			`INSERT INTO annotation_pass_required (message_id) VALUES ($1)`, messageID,
		); err != nil {
			return fmt.Errorf("mark annotation pass required: %w", err)
		}

		var usedBytes int64
		var quotaBytes *int64
		if err := tx.QueryRow(ctx,
			`UPDATE mailboxes
			   SET used_bytes = used_bytes + $1, updated_at = now()
			 WHERE id = $2
			 RETURNING used_bytes, quota_bytes`,
			p.RawSize, p.MailboxID,
		).Scan(&usedBytes, &quotaBytes); err != nil {
			return fmt.Errorf("update used_bytes: %w", err)
		}
		// Enforce the quota: reject only the message that pushes the mailbox
		// over. NULL quota = unlimited. Returning here rolls the tx back, so
		// used_bytes is not counted for the rejected message (R-029). Import
		// paths set IgnoreQuota to restore existing mail without bouncing it.
		if !p.IgnoreQuota && quotaBytes != nil && usedBytes > *quotaBytes {
			return ErrOverQuota
		}

		sha16 := ""
		if len(p.RawSHA256Hex) >= 16 {
			sha16 = p.RawSHA256Hex[:16]
		}
		outcome := p.Outcome
		if outcome == "" {
			outcome = "delivered"
		}
		// Every writer (deliver, both importers, IMAP APPEND) records a
		// salvaged parse the same way, because it is decided here from the
		// parse itself rather than by each caller (OPS-003). The same defects
		// are persisted on the bodystructure nodes, which outlive this row.
		detail := p.Message.DefectSummary()
		if detail != "" {
			outcome += ":degraded"
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO delivery_log (
				envelope_from, envelope_to, matched_alias_id, matched_mailbox_id,
				message_id, bytes, raw_sha256_short, outcome, error_detail
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			nullStr(p.EnvelopeFrom), p.EnvelopeTo, p.AliasID, p.MailboxID,
			messageID, p.RawSize, nullStr(sha16), outcome, nullStr(detail),
		); err != nil {
			return fmt.Errorf("insert delivery_log: %w", err)
		}

		if _, err := tx.Exec(ctx,
			`SELECT pg_notify('mail_arrived', $1)`,
			strconv.FormatInt(folderID, 10),
		); err != nil {
			// Best-effort. IMAP clients will rediscover on their next poll.
			slog.Warn("pg_notify failed", "err", err, "folder_id", folderID)
		}
		return nil
	}, p.OnCommitted)
	return result, err
}

// LogRejection records a rejected delivery to delivery_log for audit. Used
// when ingest parsing fails or the recipient is unknown; the message body is
// not persisted and no blob is written. Best-effort: errors are returned but
// callers should not block delivery success/failure on them. The insert is
// bounded by the configured statement timeout so a wedged Postgres can't
// hang the LDA at the rejection-logging moment.
func (db *DB) LogRejection(ctx context.Context, envFrom, envTo, outcome, detail string, bytes int64) error {
	if db.cfg.StatementTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, db.cfg.StatementTimeout)
		defer cancel()
	}
	_, err := db.pool.Exec(ctx,
		`INSERT INTO delivery_log (
			envelope_from, envelope_to, bytes, outcome, error_detail
		) VALUES ($1, $2, $3, $4, $5)`,
		nullStr(envFrom), envTo, bytes, outcome, nullStr(detail),
	)
	return err
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
