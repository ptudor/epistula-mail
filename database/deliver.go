package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/ingest"
	"github.com/ptudor/epistula-mail/database/maintenance"
	"github.com/ptudor/epistula-mail/database/recipients"
	"github.com/ptudor/epistula-mail/database/storage"
)

// runDeliver is the LDA hot path: invoked by Postfix's pipe transport,
// reads a single RFC 5322 message from stdin, parses it, writes the raw
// blob (and any attachment blobs), then atomically inserts the messages /
// attachments / delivery_log rows in one transaction. Exits with a
// sysexits.h code Postfix understands.
func runDeliver(args []string) int {
	fs := flag.NewFlagSet("deliver", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	recipient := fs.String("recipient", "", "Envelope recipient address (required)")
	sender := fs.String("sender", "", "Envelope sender address")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	if *recipient == "" {
		// DEFER, don't bounce. EX_USAGE is a permanent failure to Postfix
		// (5.x.x), so an empty ${recipient} — an empty envelope recipient, or
		// a mangled argv= after a master.cf edit — would DESTROY mail rather
		// than requeue it. That is exactly what the neighbouring trailing-args
		// guard below deliberately avoids for the same class of
		// misconfiguration (RO5X-035).
		slog.Error("deliver: -recipient is empty; refusing to lose the message "+
			"(check the argv= line in master.cf: ${recipient} expanded to nothing)",
			"args", os.Args[1:])
		fmt.Fprintln(os.Stderr, "deliver: -recipient is required")
		return EX_TEMPFAIL
	}
	// flag.Parse stops at the first non-flag word, so if Postfix handed us
	// more argv than our flags consumed it is almost certainly a missing
	// `epistula-database_destination_recipient_limit = 1` letting ${recipient}
	// expand to several addresses (`-recipient=a@x b@y`). The extra
	// recipients would otherwise be silently dropped. Defer (EX_TEMPFAIL) so
	// no recipient's copy is lost — a misconfig must requeue, never bounce.
	if fs.NArg() > 0 {
		slog.Error("deliver: unexpected trailing arguments; refusing to drop recipients "+
			"(set epistula-database_destination_recipient_limit = 1 in main.cf)",
			"leftover_args", fs.Args(), "recipient", *recipient)
		return EX_TEMPFAIL
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		// DEFER, don't bounce: a transiently unreadable or mid-edit config
		// must not cost inbound mail. To Postfix, EX_CONFIG (78) is a 5.x.x
		// bounce; EX_TEMPFAIL requeues so the message survives until the
		// operator fixes the config. (serve/admin/check-config keep EX_CONFIG
		// — their exit codes never reach Postfix.)
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return EX_TEMPFAIL
	}
	setupLogging(cfg)

	// Hard wall-clock for the entire delivery. The context bounds the DB
	// phase; the watchdog bounds EVERYTHING — parse, charset decode, blob
	// fsync on a stalled disk — by killing the process per the CLAUDE.md
	// contract ("the LDA process kills itself if exceeded"). EX_TEMPFAIL
	// makes Postfix requeue, so a pathological message or wedged disk
	// never wedges a pipe worker.
	timeout := cfg.DeliveryTimeoutDuration()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// The watchdog and the delivery share one durable-acceptance boundary
	// (RA6X-059). Without it the timer stays armed across the post-delivery
	// hook, so a transaction that commits at second 59 of a 60-second budget
	// followed by a two-second hook exits EX_TEMPFAIL — Postfix requeues mail
	// that is already durably stored, and the folder-scoped dedup that would
	// normally absorb the retry does not protect a message the user has since
	// moved or expunged.
	accepted := &deliveryAcceptance{}
	stopWatchdogs := armDeliveryWatchdogs(accepted, timeout, cfg.PostHookTimeoutDuration()+time.Second, *recipient)
	defer stopWatchdogs()

	return recoverDeliver(*recipient, accepted, func() int {
		return deliver(ctx, cfg, accepted, *recipient, *sender)
	})
}

// deliveryAcceptance is the boundary between "this message might still be lost"
// and "this message is ours to keep" (RA6X-059).
//
// It is recorded once at the successful transaction commit boundary. The mutex
// serializes that record with watchdog exit; atomic reads also serve recovery.
// Everything downstream of the commit (the post-delivery hook, the terminal log
// line) is bounded work whose failure must not cost a redelivery, because the
// message is already durable and the retry would either duplicate it or, if the
// user has moved or expunged it in the meantime, slip past the folder-scoped
// dedup and duplicate it anyway.
//
// The ordering is deliberately one-way: a commit that has not yet been recorded
// reads as not-accepted, so a timeout racing the commit still requeues. That is
// the ambiguous case, and requeueing is the safe answer to it.
type deliveryAcceptance struct {
	mu          sync.Mutex // serializes recording acceptance with the actual exit
	committed   atomic.Bool
	commitOnce  sync.Once
	afterCommit func()
}

// deliveryDeadlineExceeded is the watchdog's whole body, named so the exit-code
// decision is testable rather than buried in a closure.
//
// The process dies either way — a wedged hook or a stalled disk must never hold
// a Postfix pipe worker open — so the wall clock still bounds the delivery
// exactly as before. Only the exit code depends on whether the message has
// already become ours to keep.
func deliveryDeadlineExceeded(accepted *deliveryAcceptance, timeout time.Duration, recipient string) {
	// Logging must neither freeze the hard watchdog nor choose an exit code
	// before a blocked log write. Give the diagnostic a bounded opportunity,
	// then decide against the current acceptance state.
	logged := make(chan struct{})
	go func() {
		slog.Error("delivery wall clock exceeded; terminating", "timeout", timeout, "envelope_to", recipient)
		close(logged)
	}()
	select {
	case <-logged:
	case <-time.After(10 * time.Millisecond):
	}
	if accepted == nil {
		exitProcess(EX_TEMPFAIL)
		return
	}
	accepted.mu.Lock()
	defer accepted.mu.Unlock() // reached only by the test exit substitute
	code := EX_TEMPFAIL
	if accepted.isCommitted() {
		code = EX_OK
	}
	// No commit can be recorded between this decision and process exit.
	// A commit racing ahead of this lock wins; an unacknowledged/ambiguous
	// commit at the deadline still retries. No I/O runs under the lock.
	exitProcess(code)
}

// markCommitted records that the ingest transaction committed.
func (a *deliveryAcceptance) markCommitted() {
	if a != nil {
		a.commitOnce.Do(func() {
			a.mu.Lock()
			a.committed.Store(true)
			a.mu.Unlock()
			if a.afterCommit != nil {
				a.afterCommit()
			}
		})
	}
}

// isCommitted reports whether the message is durably stored.
func (a *deliveryAcceptance) isCommitted() bool {
	return a != nil && a.committed.Load()
}

// recoverDeliver runs the delivery body under the panic-recovery contract and
// returns the resulting sysexits code. The result is a *named* return so a
// recovered panic (or an early default) can never surface as the zero value
// EX_OK — the exact failure mode that would make Postfix delete undelivered
// mail. exitCode defaults to EX_TEMPFAIL so any panic window (including one
// before body() runs) requeues rather than losing the message, and the
// terminal "deliver exit" log line always reports the code actually returned.
// Extracted from runDeliver so the panic path is unit-testable.
func recoverDeliver(recipient string, accepted *deliveryAcceptance, body func() int) (exitCode int) {
	exitCode = EX_TEMPFAIL
	defer func() {
		if p := recover(); p != nil {
			slog.Error("deliver panic", "panic", p)
			// A panic BEFORE the commit is the case this default exists for:
			// requeue rather than lose the message. A panic after it is a bug
			// in the hook or the terminal logging, and the message is already
			// durably stored — requeueing would ask Postfix to deliver it a
			// second time (RA6X-059).
			if accepted.isCommitted() {
				slog.Error("panic occurred AFTER a successful commit; exiting EX_OK " +
					"because the message is durably stored and must not be redelivered")
				exitCode = EX_OK
			} else {
				exitCode = EX_TEMPFAIL
			}
		}
		// Annotate the terminal log line with the symbolic name + a
		// classification so operators reading mail.log can tell at a
		// glance whether Postfix will retry or bounce.
		slog.Info("deliver exit",
			"code", exitCode,
			"name", ExitCodeName(exitCode),
			"disposition", exitDisposition(exitCode),
			"envelope_to", recipient,
		)
	}()

	exitCode = body()
	return exitCode
}

// exitProcess is an indirection over os.Exit so the watchdog path is
// testable; production behavior is a straight process exit.
var exitProcess = os.Exit

// exitDisposition translates a sysexit into the word Postfix uses for it.
func exitDisposition(code int) string {
	switch {
	case code == EX_OK:
		return "delivered"
	case IsPermanentFailure(code):
		return "bounce"
	case IsTemporaryFailure(code):
		return "requeue"
	default:
		return "unclassified"
	}
}

func deliver(ctx context.Context, cfg *Config, accepted *deliveryAcceptance, envTo, envFrom string) int {
	// Envelope values land in delivery_log TEXT columns. Postfix normally
	// hands us clean addresses, but the sender controls these bytes upstream
	// and an invalid-UTF-8 value would fail the INSERT mid-delivery.
	envTo = ingest.SanitizeUTF8(envTo)
	envFrom = ingest.SanitizeUTF8(envFrom)

	// Read up to MaxMessageBytes+1 so we can distinguish "exactly at limit"
	// from "exceeds limit".
	maxBytes := cfg.Limits.MaxMessageBytes
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, maxBytes+1))
	if err != nil {
		slog.Error("deliver read stdin", "err", err)
		return EX_TEMPFAIL
	}
	if int64(len(raw)) > maxBytes {
		slog.Warn("deliver message exceeds limit", "max_bytes", maxBytes, "envelope_to", envTo)
		return EX_DATAERR
	}

	// Parse.
	parser := ingest.New(ingest.Limits{
		MaxMessageBytes:       cfg.Limits.MaxMessageBytes,
		MaxMimeDepth:          cfg.Limits.MaxMimeDepth,
		MaxMimeParts:          cfg.Limits.MaxMimeParts,
		MaxHeaderBytes:        cfg.Limits.MaxHeaderBytes,
		MaxHeaderSectionBytes: cfg.Limits.MaxHeaderSectionBytes,
		MaxTransferExpansion:  cfg.Limits.MaxTransferExpansion,
	})
	msg, err := parser.Parse(raw)
	if err != nil {
		slog.Warn("deliver parse rejected", "err", err, "envelope_to", envTo, "bytes", len(raw))
		return parseErrorToExit(err)
	}

	// Open DB (lazy until needed). Connection failure here is EX_TEMPFAIL.
	//
	// The LDA is a one-shot, single-message process: it does one recipient
	// resolve and one ingest tx, never concurrently. A warm idle pool is pure
	// waste here — pgxpool eagerly opens MinConns, so a burst of N Postfix
	// deliveries each honoring MaxIdleConns would spike ~N×MaxIdleConns
	// connections. Pin a tiny pool for deliver (MinConns 0, MaxConns 2) so a
	// burst opens ~2N at worst; MaxOpenConns/MaxIdleConns tune the long-lived
	// serve/gc pools, not the hot delivery path (R-054).
	//
	// This deliberately IGNORES postgres.max_open_conns / max_idle_conns —
	// do not "fix" it to read them. Those knobs are honoured by serve and gc,
	// which are the long-lived pools they exist for (RO5X-021).
	db, err := storage.Open(ctx, storage.Config{
		DSN:              cfg.Postgres.DSN,
		StatementTimeout: cfg.StatementTimeoutDuration(),
		MaxConns:         2,
		MinConns:         0,
		ConnMaxLifetime:  cfg.ConnMaxLifetimeDuration(),
	})
	if err != nil {
		slog.Error("deliver postgres open", "err", err)
		return EX_TEMPFAIL
	}
	defer db.Close()

	// Resolve recipient.
	resolver := recipients.New(db.Pool())
	match, err := resolver.Resolve(ctx, envTo)
	if err != nil {
		switch {
		case errors.Is(err, recipients.ErrUnknownDomain):
			slog.Warn("deliver unknown domain", "envelope_to", envTo)
			_ = db.LogRejection(ctx, envFrom, envTo, "rejected:unknown_domain", "", int64(len(raw)))
			return EX_NOHOST
		case errors.Is(err, recipients.ErrDenied):
			slog.Warn("deliver denied by domain ACL", "envelope_to", envTo)
			_ = db.LogRejection(ctx, envFrom, envTo, "rejected:denylist", "", int64(len(raw)))
			return EX_NOUSER
		case errors.Is(err, recipients.ErrNotAllowlisted):
			slog.Warn("deliver not on allowlist", "envelope_to", envTo)
			_ = db.LogRejection(ctx, envFrom, envTo, "rejected:not_allowlisted", "", int64(len(raw)))
			return EX_NOUSER
		case errors.Is(err, recipients.ErrNoMatch):
			slog.Warn("deliver no recipient match", "envelope_to", envTo)
			_ = db.LogRejection(ctx, envFrom, envTo, "rejected:no_recipient", "", int64(len(raw)))
			return EX_NOUSER
		case errors.Is(err, recipients.ErrInvalidAddress):
			slog.Warn("deliver invalid address", "envelope_to", envTo)
			return EX_DATAERR
		default:
			slog.Error("deliver recipient lookup", "err", err)
			return backendErrorToExit(err)
		}
	}

	// Idempotency: Postfix retries a delivery whenever the LDA exits
	// non-zero — including the window where the transaction committed but
	// the process died (or the watchdog fired) before EX_OK was returned.
	// The redelivered bytes are identical (same queue file), so
	// (folder, raw_sha256) is the natural dedup key, mirroring the import
	// path. Distinct SMTP submissions of the same content differ in their
	// Received headers and are never deduped.
	//
	// This pre-tx check is a fast idempotency skip, not a concurrency
	// backstop. Two identical deliveries stay distinct rows only because
	// master.cf runs the LDA with flags=D — which prepends a per-recipient
	// Delivered-To: header so each recipient's raw bytes (hence raw_sha256)
	// differ — and because R-003's recipient limit keeps one queue file from
	// handing two aliases of the same mailbox to a single invocation. Drop
	// flags=D or that limit and two aliases of one mailbox in one queue file
	// could both pass this check and insert two rows (there is no
	// UNIQUE (folder_id, raw_sha256) index by design — IMAP APPEND of
	// identical bytes is legitimate). See master.cf and R-044 (the import
	// path closes its own race in-transaction).
	store, code := openBlobStore(cfg, true)
	if code != EX_OK {
		return code
	}
	lease, lerr := maintenance.Acquire(ctx, cfg.Storage.Root, db.Pool(), false, false)
	if lerr != nil {
		slog.Error("delivery offline maintenance barrier", "err", lerr)
		return EX_TEMPFAIL
	}
	defer lease.Close()
	dup, _, err := deliveryIsDuplicate(ctx, db, match.MailboxID, cfg.Delivery.DefaultFolder, msg.SHA256Hex)
	if err != nil {
		slog.Error("deliver dedup check", "err", err)
		return EX_TEMPFAIL
	}
	if dup {
		dup, err = repairDuplicateDelivery(ctx, db, cfg, match.MailboxID, match.MailboxName, msg, raw, accepted)
		if err != nil {
			slog.Error("deliver duplicate verification failed", "err", err, "envelope_to", envTo)
			return EX_TEMPFAIL
		}
	}
	if dup {
		accepted.markCommitted()
		slog.Info("deliver duplicate; already committed",
			"envelope_to", envTo, "raw_sha256_short", msg.SHA256Hex[:16], "bytes", len(raw))
		_ = db.LogRejection(ctx, envFrom, envTo, "duplicate", "", int64(len(raw)))
		return EX_OK
	}

	// The blob tenant is the resolved mailbox's canonical name. It comes from
	// the DB (mailboxes.name, constrained to the path-safe charset), so a
	// parse failure here is an internal/config invariant violation, not a
	// per-message data error.
	tenant, err := blob.ParseTenant(match.MailboxName)
	if err != nil {
		slog.Error("deliver invalid mailbox name for blob tenant — internal bug",
			"mailbox", match.MailboxName, "mailbox_id", match.MailboxID, "err", err)
		return EX_SOFTWARE
	}

	// Advisory over-quota pre-check, before any blob reaches disk.
	//
	// The authoritative check lives inside the ingest tx (R-029) and stays
	// exactly where it is — it is the only race-free one. But the tx runs
	// AFTER the raw blob (and every attachment blob) is already committed to
	// disk, and a rollback does not unlink them. Nothing does: they are
	// reclaimed only by `gc mark` (which skips files younger than the 24 h
	// grace) followed by a later `gc sweep` that satisfies the generation
	// gate. So a sender who knows a mailbox is over quota could park
	// 50 MiB per message on the spool for over a day, at the cost of one
	// bounced SMTP transaction each — the exact opposite of what a quota is
	// for (RO5X-009).
	//
	// This pre-check is advisory only: a query error must NOT reject the
	// delivery, and a NULL quota means unlimited. The residual race (quota
	// crossed between here and the tx) still leaks one blob, which is what
	// GC is for.
	if overQuotaPreCheck(ctx, db, match.MailboxID, int64(len(raw))) {
		slog.Warn("deliver over quota (pre-check)",
			"envelope_to", envTo, "mailbox_id", match.MailboxID, "bytes", len(raw))
		_ = db.LogRejection(ctx, envFrom, envTo, "rejected:over_quota", "", int64(len(raw)))
		return EX_CANTCREAT
	}

	// Write raw blob under the tenant's subtree. Use wall-clock UTC arrival
	// time as the bucket so the blob path matches the stored raw_blob_date and
	// so daily backups can rsync only the current day's directory.
	blobDate := time.Now().UTC()
	bucket := blob.BucketFromTime(blobDate)
	rawWriter, err := store.NewWriter(blob.KindRaw, tenant, bucket)
	if err != nil {
		slog.Error("deliver blob writer", "err", err)
		return EX_TEMPFAIL
	}
	if _, err := rawWriter.Write(raw); err != nil {
		_ = rawWriter.Abort()
		slog.Error("deliver blob write", "err", err)
		return EX_TEMPFAIL
	}
	rawSHA, _, _, err := rawWriter.Close()
	if err != nil {
		slog.Error("deliver blob close", "err", err)
		return EX_TEMPFAIL
	}

	// Sanity: the parser computed the same sha256 we just wrote.
	if rawSHA != msg.SHA256Hex {
		slog.Error("deliver sha256 mismatch — internal bug", "blob", rawSHA, "parsed", msg.SHA256Hex)
		return EX_SOFTWARE
	}

	// Write attachments under the same bucket so a message and its parts
	// stay co-located on disk.
	attParams := make([]storage.AttachmentParams, 0, len(msg.Attachments))
	for _, att := range msg.Attachments {
		w, err := store.NewWriter(blob.KindAttachment, tenant, bucket)
		if err != nil {
			slog.Error("deliver att writer", "err", err)
			return EX_TEMPFAIL
		}
		if _, err := w.Write(att.Data); err != nil {
			_ = w.Abort()
			slog.Error("deliver att write", "err", err)
			return EX_TEMPFAIL
		}
		sha, _, _, err := w.Close()
		if err != nil {
			slog.Error("deliver att close", "err", err)
			return EX_TEMPFAIL
		}
		attParams = append(attParams, storage.AttachmentParams{
			PartNumber:  att.PartNumber,
			Filename:    att.Filename,
			ContentType: att.ContentType,
			ContentID:   att.ContentID,
			Disposition: att.Disposition,
			Size:        att.Size,
			SHA256Hex:   sha,
			BlobDate:    blobDate,
		})
	}

	// Persist.
	aliasIDPtr := &match.AliasID
	if match.AliasID == 0 {
		aliasIDPtr = nil
	}
	start := time.Now()
	res, err := db.Ingest(ctx, storage.IngestParams{
		MailboxID:    match.MailboxID,
		MailboxName:  match.MailboxName,
		AliasID:      aliasIDPtr,
		FolderName:   cfg.Delivery.DefaultFolder,
		EnvelopeFrom: envFrom,
		EnvelopeTo:   envTo,
		RawSHA256Hex: rawSHA,
		RawSize:      int64(len(raw)),
		RawBlobDate:  blobDate,
		Message:      msg,
		Attachments:  attParams,
		EnsureBlobs:  ensureBlobsFunc(store, tenant, bucket, rawSHA, raw, msg.Attachments, attParams),
		OnCommitted:  accepted.markCommitted,
	})
	if err != nil {
		if errors.Is(err, storage.ErrOverQuota) {
			slog.Warn("deliver over quota", "envelope_to", envTo, "mailbox_id", match.MailboxID)
			_ = db.LogRejection(ctx, envFrom, envTo, "rejected:over_quota", "", int64(len(raw)))
			return EX_CANTCREAT
		}
		if errors.Is(err, storage.ErrMailboxInMaintenance) || errors.Is(err, storage.ErrMailboxIdentityChanged) {
			// An operator is moving this mailbox's blob tenant tree
			// (RA6X-013). Both conditions are transient by construction and
			// neither is the sender's fault, so requeue: the message is
			// delivered on the next attempt, once the barrier lifts.
			slog.Warn("deliver deferred: mailbox is being moved",
				"err", err, "envelope_to", envTo, "mailbox_id", match.MailboxID)
			return EX_TEMPFAIL
		}
		slog.Error("deliver ingest", "err", err, "envelope_to", envTo)
		return backendErrorToExit(err)
	}

	// The transaction has committed. From here on the message is ours to keep,
	// and nothing — a watchdog timeout, a hook failure, a panic in the logging
	// below — may ask Postfix to deliver it again (RA6X-059). Recorded before
	// the log line, so even the log line is inside the boundary.
	accepted.markCommitted()

	slog.Info("delivered",
		"envelope_to", envTo,
		"mailbox_id", match.MailboxID,
		"folder_id", res.FolderID,
		"uid", res.UID,
		"message_id", res.MessageID,
		"raw_sha256_short", rawSHA[:16],
		"bytes", len(raw),
		"attachments", len(attParams),
		"is_catchall", match.IsCatchall,
		"elapsed_ms", time.Since(start).Milliseconds(),
	)
	if len(msg.Defects) > 0 {
		// Delivered rather than bounced (OPS-003); delivery_log has the same
		// defects under a ":degraded" outcome.
		slog.Warn("delivered with a degraded parse",
			"envelope_to", envTo, "message_id", res.MessageID, "defects", msg.DefectSummary())
	}

	// Hook fires only on a fully-committed delivery. Errors are logged but
	// not propagated — the message is already durable, and re-running the
	// LDA would duplicate it (the dedup key includes envelope recipient,
	// not the hook's success).
	runPostDeliveryHook(cfg, PostDeliveryHookContext{
		EnvelopeFrom: envFrom,
		EnvelopeTo:   envTo,
		MailboxID:    match.MailboxID,
		FolderID:     res.FolderID,
		UID:          res.UID,
		MessageID:    res.MessageID,
		ModSeq:       res.ModSeq,
		RawSHA256Hex: rawSHA,
		RawSize:      int64(len(raw)),
		Attachments:  len(attParams),
		IsCatchall:   match.IsCatchall,
	})
	return EX_OK
}

// deliveryIsDuplicate reports whether the message with the given content
// hash already exists in the recipient's target folder. A folder that does
// not exist yet trivially has no duplicate.
func deliveryIsDuplicate(ctx context.Context, db *storage.DB, mailboxID int64, folderName, sha256Hex string) (bool, time.Time, error) {
	var folderID int64
	err := db.Pool().QueryRow(ctx,
		`SELECT id FROM folders WHERE mailbox_id = $1 AND name = $2`,
		mailboxID, folderName,
	).Scan(&folderID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, time.Time{}, nil
		}
		return false, time.Time{}, fmt.Errorf("folder lookup: %w", err)
	}
	// The blob date comes back with the answer so a caller can verify the
	// stored file at the coordinates the row actually names (RA6X-035),
	// instead of guessing which day it was written on.
	shaBytes, derr := hex.DecodeString(sha256Hex)
	if derr != nil {
		return false, time.Time{}, fmt.Errorf("decode sha: %w", derr)
	}
	var blobDate time.Time
	err = db.Pool().QueryRow(ctx,
		`SELECT raw_blob_date FROM messages
		  WHERE folder_id = $1 AND raw_sha256 = $2
		  ORDER BY id LIMIT 1`,
		folderID, shaBytes,
	).Scan(&blobDate)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, time.Time{}, nil
		}
		return false, time.Time{}, fmt.Errorf("duplicate lookup: %w", err)
	}
	return true, blobDate, nil
}

// parseErrorToExit classifies a parse failure into a sysexits code.
//
// Both arms used to return EX_DATAERR, so the default bounced errors that are
// NOT data errors as well. Today ingest.Parse is pure in-memory so nothing
// else can arrive, but if it ever returns a wrapped I/O or context error the
// message would have been BOUNCED instead of deferred.
//
// The default is now EX_TEMPFAIL, per the "when in doubt at a routine deliver
// error site, return EX_TEMPFAIL" rule in exitcodes.go. That makes the switch
// real classification and puts the safe direction — requeue, never lose mail —
// in the fallback (RO5X-025).
//
// The seven named sentinels stay EX_DATAERR: Postfix must bounce a genuinely
// malformed or oversize message rather than retry the same bytes forever.
func parseErrorToExit(err error) int {
	switch {
	case errors.Is(err, ingest.ErrTooLarge),
		errors.Is(err, ingest.ErrTooDeep),
		errors.Is(err, ingest.ErrTooManyParts),
		errors.Is(err, ingest.ErrHeaderTooLarge),
		errors.Is(err, ingest.ErrHeadersTooBig),
		errors.Is(err, ingest.ErrZipBomb),
		errors.Is(err, ingest.ErrMalformed):
		return EX_DATAERR
	default:
		return EX_TEMPFAIL
	}
}

// backendErrorToExit classifies an unexpected resolver or storage failure on
// the Postfix-facing delivery path (RA6X-008).
//
// It is deliberately NOT storage.IsRetryable. That helper answers "would a
// retry of this SQL plausibly succeed?", and correctly answers "no" for
// SQLSTATE classes 22/23/42 — a data, constraint, syntax or permission error
// replayed against an unchanged database fails identically. But the LDA is not
// asking that question. It is asking "whose fault is this, the sender's or
// ours?", and for an unapplied migration, a revoked GRANT, schema drift or an
// application SQL bug the answer is "ours". Bouncing accepted mail because an
// operator has not finished a deployment destroys the queue; deferring it
// costs a retry interval and keeps the message until the backend is repaired.
//
// So every backend failure that reaches here defers. Permanent (bounce-class)
// codes on this path are reserved for explicit, validated policy outcomes that
// are decided before this function is ever called: the recipients package's
// unknown-domain/denylist/allowlist/no-match/invalid-address sentinels, the
// parser's malformed/oversize sentinels (parseErrorToExit) and
// storage.ErrOverQuota.
//
// storage.IsRetryable keeps its meaning and its callers; it simply no longer
// decides the bounce/defer boundary for delivery. Its answer is still worth
// recording, because "not retryable" marks the cases an operator must fix
// before the queue drains rather than ones that clear on their own.
func backendErrorToExit(err error) int {
	if !storage.IsRetryable(err) {
		slog.Error("deliver deferring a non-transient backend error — "+
			"queued mail is preserved but delivery will keep failing until "+
			"the backend is repaired (check migrations, grants and schema)",
			"err", err)
	}
	return EX_TEMPFAIL
}

// overQuotaPreCheck reports whether adding rawSize bytes would push the
// mailbox past its quota, as a cheap advisory gate before any blob is
// written (RO5X-009).
//
// It is deliberately fail-open: on any query error, or when quota_bytes is
// NULL (unlimited), it returns false and lets the authoritative in-tx check
// in storage.Ingest decide. It must never be the reason a delivery is
// rejected on its own — only a confirmation of what the tx would do anyway.
//
// Import paths (IgnoreQuota) must not call this.
func overQuotaPreCheck(ctx context.Context, db *storage.DB, mailboxID, rawSize int64) bool {
	var used int64
	var quota *int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT used_bytes, quota_bytes FROM mailboxes WHERE id = $1`, mailboxID,
	).Scan(&used, &quota); err != nil {
		// Advisory only — let the in-tx check be the authority.
		slog.Debug("over-quota pre-check query failed; deferring to the ingest tx",
			"mailbox_id", mailboxID, "err", err)
		return false
	}
	if quota == nil {
		return false // unlimited
	}
	// Match the in-tx comparison exactly: Ingest adds RawSize to used_bytes
	// and rejects when the result is strictly greater than the quota.
	return used+rawSize > *quota
}
