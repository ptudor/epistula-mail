package imapsess

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/ingest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// Append reads the IMAP literal, runs it through the shared ingest
// parser, writes raw + attachment blobs, and persists the message via
// storage.Ingest — the same code path Postfix-side deliver uses, so
// there's exactly one parser and one schema mutation across both write
// paths. Returns the new UID + UIDVALIDITY for the UIDPLUS APPENDUID
// response.
func (s *Session) Append(mailbox string, r imap.LiteralReader, options *imap.AppendOptions) (appendData *imap.AppendData, err error) {
	defer s.guard("APPEND", &err)
	if err := s.requireAuth(); err != nil {
		return nil, err
	}

	cap := s.be.MaxAppendBytes
	if cap <= 0 {
		cap = 52_428_800 // 50 MiB default; matches LDA cap.
	}
	if r.Size() > cap {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTooBig,
			Text: fmt.Sprintf("APPEND literal exceeds %d bytes", cap),
		}
	}

	if verr := validateFolderName(mailbox); verr != nil {
		return nil, verr
	}
	// Normalize once, here, so the existence check below and the
	// IngestParams.FolderName that reaches storage.Ingest agree. Checking
	// with the canonical name but ingesting with the raw one would let
	// `APPEND inbox` pass the check against INBOX and then have Ingest's
	// upsert create a second, differently-cased folder.
	mailbox = canonicalFolderName(mailbox)

	// RFC 3501 §6.3.11 / RFC 9051 §6.3.12: if the destination mailbox does
	// not exist, respond NO [TRYCREATE] so the client can offer to create
	// it. storage.Ingest upserts the folder unconditionally — correct for
	// the LDA (first delivery must create INBOX) and for the importers, but
	// wrong here: it turned a client bug or a fat-fingered folder name into
	// a permanent folder visible in LIST on every device, with no way to
	// tell "the client meant this" from "the client mistyped".
	//
	// The policy belongs in the caller that knows it, so Ingest is left
	// alone rather than growing a must-exist flag.
	//
	// Checking here, before the literal is read and before any blob is
	// written, also removes one of the two orphan-blob paths RO5X-009
	// addresses (RO5X-012).
	// The folder ID from this lookup is CARRIED INTO the transaction rather
	// than discarded (RA6X-047). Re-resolving the name inside Ingest let a
	// folder deleted or renamed between here and the commit be silently
	// recreated under its old name, with the message landing in the new one.
	lookupCtx, lookupCancel := s.queryCtx()
	destFolderID, lerr := s.lookupFolder(lookupCtx, mailbox)
	lookupCancel()
	if lerr != nil {
		var imapErr *imap.Error
		if errors.As(lerr, &imapErr) && imapErr.Code == imap.ResponseCodeNonExistent {
			return nil, &imap.Error{
				Type: imap.StatusResponseTypeNo,
				Code: imap.ResponseCodeTryCreate,
				Text: "no such mailbox; create it first",
			}
		}
		return nil, lerr
	}

	// Bound the read at cap+1 so we can distinguish "exactly at cap"
	// from "client lied about Size()".
	raw, err := io.ReadAll(io.LimitReader(r, cap+1))
	if err != nil {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "APPEND read: " + err.Error()}
	}
	if int64(len(raw)) > cap {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTooBig,
			Text: "APPEND literal exceeded declared size",
		}
	}

	// Parse via the same ingest used by Postfix-side delivery.
	parser := ingest.New(ingest.Limits{
		MaxMessageBytes:       cap,
		MaxMimeDepth:          10,
		MaxMimeParts:          200,
		MaxHeaderBytes:        16_384,
		MaxHeaderSectionBytes: 262_144,
		MaxTransferExpansion:  10,
	})
	msg, perr := parser.Parse(raw)
	if perr != nil {
		s.be.Logger.Warn("APPEND parse rejected",
			"mailbox", s.mailboxName, "folder", mailbox,
			"bytes", len(raw), "err", perr)
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "APPEND: malformed RFC 5322 message",
		}
	}

	ctx, cancel := s.queryCtx()
	defer cancel()

	// Pick the on-disk bucket. APPEND uses the wall-clock UTC of when
	// the IMAP server received it (matches deliver semantics: arrival
	// time = bucket date) unless the client passed an explicit date-time
	// in AppendOptions. Per RFC 3501/9051 §6.3.12 that optional date-time
	// sets the message's INTERNALDATE, and it drives the bucket too, so
	// importing historical mail via APPEND lands in the right
	// raw/yyyy/mm/dd directory AND sorts correctly in clients.
	now := time.Now().UTC()
	blobDate := now
	var internalDate *time.Time
	if options != nil && !options.Time.IsZero() {
		blobDate = clampAppendDate(options.Time.UTC(), now, s.be.Logger, s.mailboxName)
		internalDate = &blobDate
	}
	bucket := blob.BucketFromTime(blobDate)

	// Advisory over-quota pre-check, before any blob reaches disk. The
	// authoritative check stays inside the ingest tx (R-029); this only
	// avoids writing up to 50 MiB of blob that the tx is about to roll back
	// and that nothing then unlinks — GC would not reclaim it for at least
	// the 24 h grace plus a mark and a sweep, so an authenticated client
	// looping APPEND into an over-quota mailbox could park arbitrary bytes
	// on the spool for a day (RO5X-009).
	//
	// Advisory only: a query error must not reject the APPEND, and a NULL
	// quota means unlimited.
	if s.overQuotaPreCheck(ctx, int64(len(raw))) {
		s.be.Logger.Warn("APPEND over quota (pre-check)",
			"mailbox", s.mailboxName, "folder", mailbox, "bytes", len(raw))
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeOverQuota,
			Text: "mailbox over quota",
		}
	}

	rawSHA, code, err := s.writeRawBlob(raw, bucket)
	if err != nil {
		s.be.Logger.Error("APPEND raw blob write",
			"mailbox", s.mailboxName, "folder", mailbox, "err", err)
		return nil, code
	}
	if rawSHA != msg.SHA256Hex {
		// Defensive: parser and blob writer must agree on the hash.
		s.be.Logger.Error("APPEND sha mismatch — internal bug",
			"blob", rawSHA, "parsed", msg.SHA256Hex)
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "APPEND: internal hash mismatch"}
	}

	attParams, code, err := s.writeAttachmentBlobs(msg.Attachments, bucket)
	if err != nil {
		s.be.Logger.Error("APPEND attachment blob write",
			"mailbox", s.mailboxName, "folder", mailbox, "err", err)
		return nil, code
	}

	flags := flagsFromOptions(options)

	res, ierr := s.be.Storage().Ingest(ctx, storage.IngestParams{
		MailboxID:   s.mailboxID,
		MailboxName: s.mailboxName, // the blob tenant; APPEND is intra-mailbox
		FolderName:  mailbox,
		// Must-exist: RFC 9051 §6.3.12 wants NO [TRYCREATE] for a folder that
		// is not there, and an APPEND must never resurrect one another session
		// has just deleted.
		MustExistFolderID: destFolderID,
		EnvelopeFrom:      "", // APPEND has no envelope
		EnvelopeTo:        "imap-append:" + s.mailboxName,
		RawSHA256Hex:      rawSHA,
		RawSize:           int64(len(raw)),
		RawBlobDate:       blobDate,
		Message:           msg,
		Attachments:       attParams,
		InternalDate:      internalDate, // nil = server time, per RFC
		Flags:             flags,
		Outcome:           "appended",
		// GC race protocol: re-verify the blobs inside the transaction
		// (post advisory-lock) and rewrite any a concurrent sweep removed.
		EnsureBlobs: func(ctx context.Context) error {
			if _, err := s.be.BlobStore.EnsureContent(blob.KindRaw, s.tenant, bucket, rawSHA, raw); err != nil {
				return fmt.Errorf("raw blob: %w", err)
			}
			for i := range msg.Attachments {
				if _, err := s.be.BlobStore.EnsureContent(blob.KindAttachment, s.tenant, bucket, attParams[i].SHA256Hex, msg.Attachments[i].Data); err != nil {
					return fmt.Errorf("attachment blob %s: %w", attParams[i].PartNumber, err)
				}
			}
			return nil
		},
	})
	if ierr != nil {
		if errors.Is(ierr, storage.ErrOverQuota) {
			// The mailbox is over quota; reject this APPEND with NO [OVERQUOTA]
			// per RFC 9208. Not logged at ERROR — it is an expected condition.
			s.be.Logger.Warn("APPEND over quota", "mailbox", s.mailboxName, "folder", mailbox)
			return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeOverQuota, Text: "mailbox over quota"}
		}
		if errors.Is(ierr, storage.ErrMailboxInMaintenance) || errors.Is(ierr, storage.ErrMailboxIdentityChanged) {
			// This session cached the mailbox name — and with it the on-disk
			// blob tenant — at LOGIN, and an operator is moving that tree
			// (RA6X-013). The blob for this APPEND has already been written
			// under the cached tenant; committing the row would acknowledge a
			// message that no reader could ever find. The transaction rolled
			// back, so nothing was acknowledged, and the orphaned blob is
			// GC's to reclaim.
			//
			// The session's view is now stale for every purpose, not just this
			// command, so drop the connection rather than leave the client
			// issuing commands against a mailbox that has moved. It reconnects
			// and resolves the current name.
			s.be.Logger.Warn("APPEND refused: mailbox is being moved; closing the session",
				"mailbox", s.mailboxName, "folder", mailbox, "err", ierr)
			s.closeStaleSession()
			return nil, &imap.Error{
				Type: imap.StatusResponseTypeNo,
				Code: imap.ResponseCodeUnavailable,
				Text: "mailbox is temporarily unavailable; reconnect",
			}
		}
		if errors.Is(ierr, storage.ErrFolderGone) {
			// The destination was deleted between the lookup and the commit.
			// TRYCREATE is the honest answer: the folder really is not there,
			// and the alternative — recreating it — would undo another
			// session's DELETE without anyone asking (RA6X-047).
			s.be.Logger.Warn("APPEND destination folder disappeared mid-command",
				"mailbox", s.mailboxName, "folder", mailbox)
			return nil, &imap.Error{
				Type: imap.StatusResponseTypeNo,
				Code: imap.ResponseCodeTryCreate,
				Text: "no such mailbox; create it first",
			}
		}
		s.be.Logger.Error("APPEND ingest",
			"mailbox", s.mailboxName, "folder", mailbox, "err", ierr)
		if storage.IsRetryable(ierr) {
			return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeUnavailable, Text: "backend temporarily unavailable"}
		}
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "APPEND failed"}
	}

	metricAppendsTotal.Inc()
	metricAppendBytes.Add(float64(len(raw)))

	// UIDVALIDITY comes from the SAME committed transaction that allocated the
	// UID (RA6X-047). It used to be a second query after commit, which could
	// report a validity from a later rename, or zero if the folder had since
	// been deleted — and a UIDPLUS response pairing a real UID with a zero
	// validity is worse than no UIDPLUS response at all, because a client
	// trusts it.
	//
	// APPENDUID is optional, so if the pair cannot be made valid the whole
	// response is omitted rather than sent half-right.
	if res.UIDValidity <= 0 || res.UIDValidity > math.MaxUint32 {
		s.be.Logger.Warn("APPEND: destination uidvalidity is not representable; omitting APPENDUID",
			"folder_id", res.FolderID, "uidvalidity", res.UIDValidity)
		return nil, nil
	}
	return &imap.AppendData{
		UID:         imap.UID(res.UID),
		UIDValidity: uint32(res.UIDValidity),
	}, nil
}

// writeRawBlob writes the raw RFC 5322 bytes to the blob store under
// bucket. Returns the sha256 hex.
func (s *Session) writeRawBlob(raw []byte, bucket blob.Bucket) (string, *imap.Error, error) {
	w, err := s.be.BlobStore.NewWriter(blob.KindRaw, s.tenant, bucket)
	if err != nil {
		return "", appendBackendErr("blob writer"), err
	}
	if _, err := w.Write(raw); err != nil {
		_ = w.Abort()
		return "", appendBackendErr("blob write"), err
	}
	sha, _, _, err := w.Close()
	if err != nil {
		return "", appendBackendErr("blob close"), err
	}
	return sha, nil, nil
}

func (s *Session) writeAttachmentBlobs(atts []ingest.Attachment, bucket blob.Bucket) ([]storage.AttachmentParams, *imap.Error, error) {
	out := make([]storage.AttachmentParams, 0, len(atts))
	for _, a := range atts {
		w, err := s.be.BlobStore.NewWriter(blob.KindAttachment, s.tenant, bucket)
		if err != nil {
			return nil, appendBackendErr("attachment writer"), err
		}
		if _, err := w.Write(a.Data); err != nil {
			_ = w.Abort()
			return nil, appendBackendErr("attachment write"), err
		}
		sha, _, _, err := w.Close()
		if err != nil {
			return nil, appendBackendErr("attachment close"), err
		}
		out = append(out, storage.AttachmentParams{
			PartNumber:  a.PartNumber,
			Filename:    a.Filename,
			ContentType: a.ContentType,
			ContentID:   a.ContentID,
			Disposition: a.Disposition,
			Size:        a.Size,
			SHA256Hex:   sha,
		})
	}
	return out, nil, nil
}

// flagsFromOptions converts APPEND flags through the same canonicalizing
// seam STORE/COPY use, so `APPEND ... (\SEEN)` persists `\Seen` and epistula-api's
// byte-exact flag filters agree with what IMAP wrote (R-063).
func flagsFromOptions(o *imap.AppendOptions) []string {
	if o == nil || len(o.Flags) == 0 {
		return nil
	}
	return flagSliceToStrings(o.Flags)
}

func appendBackendErr(text string) *imap.Error {
	return &imap.Error{
		Type: imap.StatusResponseTypeNo,
		Code: imap.ResponseCodeServerBug,
		Text: "APPEND " + text + " failed",
	}
}

// overQuotaPreCheck reports whether adding rawSize bytes would push this
// session's mailbox past its quota. Advisory gate before any blob is written
// (RO5X-009); mirrors epistula-database's deliver-side helper.
//
// Fail-open by design: on a query error, or when quota_bytes is NULL
// (unlimited), it returns false and lets the authoritative in-tx check in
// storage.Ingest decide. It must never be the sole reason an APPEND is
// refused.
func (s *Session) overQuotaPreCheck(ctx context.Context, rawSize int64) bool {
	var used int64
	var quota *int64
	if err := s.be.Pool.QueryRow(ctx,
		`SELECT used_bytes, quota_bytes FROM mailboxes WHERE id = $1`, s.mailboxID,
	).Scan(&used, &quota); err != nil {
		s.be.Logger.Debug("APPEND over-quota pre-check failed; deferring to the ingest tx",
			"mailbox", s.mailboxName, "err", err)
		return false
	}
	if quota == nil {
		return false // unlimited
	}
	// Match the in-tx comparison exactly.
	return used+rawSize > *quota
}

// appendDateFloor is the earliest date-time APPEND will honour. 1970/01/01 is
// reserved as the store's "date unknown" sentinel bucket, so a client must not
// be able to write real mail into it; 1970/01/02 is the first ordinary day.
var appendDateFloor = time.Date(1970, 1, 2, 0, 0, 0, 0, time.UTC)

// appendDateFutureSlack bounds how far ahead of the server's clock a client's
// date-time may be. A day absorbs any plausible clock skew between an MUA and
// the server.
const appendDateFutureSlack = 24 * time.Hour

// clampAppendDate bounds a client-supplied APPEND date-time.
//
// The date-time becomes both the INTERNALDATE and the on-disk bucket, which is
// deliberate and mostly right — importing historical mail via APPEND should
// land in the correct raw/yyyy/mm/dd. But it was unvalidated, so a client
// could APPEND with 31-Dec-9999 and create <tenant>/raw/9999/12/31/…, or
// scatter one blob per day across thousands of directories. The stated
// rationale for date partitioning ("backup only what's new since last week",
// "any single day's bucket stays bounded") was a client-controlled property.
//
// There is no correctness bug — the date is stored in raw_blob_date so GC
// reference-checks a future bucket correctly — only an operational one, so
// this clamps and warns rather than refusing. In-range past dates, the useful
// case, are honoured untouched (RO5X-041).
func clampAppendDate(t, now time.Time, logger *slog.Logger, mailbox string) time.Time {
	switch {
	case t.After(now.Add(appendDateFutureSlack)):
		if logger != nil {
			logger.Warn("APPEND date-time is too far in the future; clamping to now",
				"mailbox", mailbox, "requested", t.Format(time.RFC3339), "clamped_to", now.Format(time.RFC3339))
		}
		return now
	case t.Before(appendDateFloor):
		if logger != nil {
			logger.Warn("APPEND date-time predates the store's epoch; clamping",
				"mailbox", mailbox, "requested", t.Format(time.RFC3339),
				"clamped_to", appendDateFloor.Format(time.RFC3339))
		}
		return appendDateFloor
	default:
		return t
	}
}
