// IMAP mutations: STORE, EXPUNGE, COPY, MOVE.
//
// All four run inside a single Postgres transaction so partial failure
// leaves the schema consistent (no half-applied UID renumbering, no
// half-decremented used_bytes counters). UID allocation in COPY/MOVE
// goes through an UPDATE ... RETURNING on the destination folder's
// uidnext, which holds a row lock for the duration of the tx — that's
// the only point of cross-folder contention.
package imapsess

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/jackc/pgx/v5"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/imapflags"
	"github.com/ptudor/epistula-mail/database/storage"
)

// Store applies a flag mutation (SET / ADD / REMOVE) to the messages
// matched by numSet inside the selected folder. Emits FETCH FLAGS
// updates per affected message via the FetchWriter unless StoreFlags.Silent.
func (s *Session) Store(w *imapserver.FetchWriter, numSet imap.NumSet, flags *imap.StoreFlags, options *imap.StoreOptions) (err error) {
	defer s.guard("STORE", &err)
	if err := s.requireAuth(); err != nil {
		return err
	}
	if s.selectedFolderID == 0 {
		return responseBadState("STORE: no mailbox selected")
	}
	if err := s.requireWritable("STORE"); err != nil {
		return err
	}
	// An EMPTY flag list is a valid replacement set (RA6X-004).
	//
	// `STORE 1 FLAGS ()` means "this message now has no flags", and the SQL
	// below already handles an empty array correctly — it was simply
	// unreachable, because the early return fired before Op was examined. A
	// client could therefore never clear all flags in one operation, including
	// \Deleted, and the server acknowledged the command as if it had.
	//
	// An empty ADD or REMOVE genuinely is a no-op: there is nothing to add or
	// remove, and RFC 9051 has no untagged response for "nothing happened".
	if flags == nil {
		return nil
	}
	if len(flags.Flags) == 0 && flags.Op != imap.StoreFlagsSet {
		return nil
	}

	ctx, cancel := s.queryCtx()
	defer cancel()

	flagStrs := flagSliceToStrings(flags.Flags)

	// Resolve numSet → list of UIDs, in sequence-number order so the
	// FETCH responses emit in a sensible order.
	rows, err := s.resolveTargets(ctx, numSet)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	uids := make([]int64, 0, len(rows))
	for _, r := range rows {
		uids = append(uids, r.uid)
	}

	// All three ops write the flag array in SORTED, DISTINCT order.
	//
	// Only ADD used to sort (array_agg(DISTINCT f) forces one), so SET
	// preserved client order while ADD did not, and two rows with identical
	// flag SETS could hold different array VALUES. Flag order is not
	// semantically meaningful in IMAP, so this is cosmetic — but it defeats
	// any future equality/index optimisation and makes diffing messages.flags
	// across a restore noisy. Sorted-everywhere is the simpler invariant, so
	// it is the one picked here (RO5X-027).
	var setExpr string
	switch flags.Op {
	case imap.StoreFlagsSet:
		setExpr = `flags = (SELECT COALESCE(array_agg(DISTINCT f ORDER BY f), '{}'::text[]) FROM unnest($2::text[]) f)`
	case imap.StoreFlagsAdd:
		setExpr = `flags = (SELECT COALESCE(array_agg(DISTINCT f ORDER BY f), '{}'::text[]) FROM unnest(flags || $2) f)`
	case imap.StoreFlagsDel:
		setExpr = `flags = (SELECT COALESCE(array_agg(DISTINCT f ORDER BY f), '{}'::text[]) FROM unnest(flags) f WHERE NOT (f = ANY($2)))`
	default:
		return &imap.Error{Type: imap.StatusResponseTypeBad, Text: fmt.Sprintf("STORE: unknown op %v", flags.Op)}
	}

	// CONDSTORE: bump highest_modseq for the folder and stamp the
	// new value onto every row we're about to touch. This runs in
	// the same SQL statement as the flag update so the stamp + flag
	// change are atomic.
	query := fmt.Sprintf(`UPDATE messages SET %s, mod_seq = $4
	                      WHERE folder_id = $1 AND uid = ANY($3)
	                      RETURNING uid, flags`, setExpr)

	// FETCH FLAGS updates are written only after commit, so a deadlock victim
	// can safely replay the whole thing (RA6X-021). STORE takes the folder
	// (level 3) and then the message rows (level 4), which is the canonical
	// order — it was EXPUNGE, taking them the other way round, that made this
	// pair a cycle.
	updatedFlags := make(map[int64][]string, len(uids))
	var newModSeq int64
	if err := retryTx(ctx, s.be.Pool.Begin, func(tx pgx.Tx) error {
		clear(updatedFlags)

		if err := tx.QueryRow(ctx,
			`UPDATE folders SET highest_modseq = highest_modseq + 1
			  WHERE id = $1 RETURNING highest_modseq`,
			s.selectedFolderID,
		).Scan(&newModSeq); err != nil {
			return fmt.Errorf("modseq bump: %w", err)
		}

		r2, err := tx.Query(ctx, query, s.selectedFolderID, flagStrs, uids, newModSeq)
		if err != nil {
			return fmt.Errorf("update: %w", err)
		}
		defer r2.Close()
		for r2.Next() {
			var uid int64
			var newFlags []string
			if err := r2.Scan(&uid, &newFlags); err != nil {
				return fmt.Errorf("scan: %w", err)
			}
			updatedFlags[uid] = newFlags
		}
		if err := r2.Err(); err != nil {
			return err
		}
		r2.Close()
		notifyFolderChanged(ctx, tx, s.be.Logger, s.selectedFolderID)
		return nil
	}); err != nil {
		s.be.Logger.Error("STORE", "folder", s.selectedFolderName, "err", err)
		if isRetryableTxError(err) {
			return txAborted("STORE")
		}
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "STORE failed"}
	}

	// Record the flags this session just wrote, so a later reconcile does not
	// push the session's own change back at its client as an unsolicited FETCH
	// (RA6X-001). Done even for a silent STORE: the client asked for no
	// response, and re-reporting it later would be one.
	for uid := range updatedFlags {
		s.view.note(uid, newModSeq)
	}

	if flags.Silent || w == nil {
		return nil
	}

	// Emit FETCH FLAGS for each updated message. Order by the original
	// sequence-number order to match what RFC 9051 callers expect.
	for _, r := range rows {
		newFlags, ok := updatedFlags[r.uid]
		if !ok {
			continue
		}
		resp := w.CreateMessage(r.seqNum)
		resp.WriteUID(imap.UID(r.uid))
		resp.WriteFlags(toIMAPFlags(newFlags))
		if err := resp.Close(); err != nil {
			return err
		}
	}
	return nil
}

// Expunge removes every message with \Deleted in flags from the
// selected folder, optionally constrained to a UID subset (UID
// EXPUNGE per RFC 9051). The transaction also decrements the owning
// mailbox's used_bytes by the sum of the deleted messages' raw_size.
//
// Sequence-number emission tracks the IMAP "expunge renumbers
// everything after it" rule: deletions are processed in ascending
// sequence-number order and each WriteExpunge call uses the *current*
// (post-prior-deletions) sequence number.
func (s *Session) Expunge(w *imapserver.ExpungeWriter, uids *imap.UIDSet) (err error) {
	defer s.guard("EXPUNGE", &err)
	if err := s.requireAuth(); err != nil {
		return err
	}
	if s.selectedFolderID == 0 {
		return responseBadState("EXPUNGE: no mailbox selected")
	}
	// On a read-only (EXAMINE) selection, EXPUNGE is a silent no-op success,
	// NOT an error: a read-only mailbox has no locally-settable \Deleted state
	// to act on, and the library implements CLOSE as an implicit expunge —
	// RFC 3501 §6.4.2 requires CLOSE to succeed on a read-only selection. If we
	// returned NO here the library would skip Unselect() and leave the session
	// in a wrong state (R-038). MOVE keeps its own requireWritable guard.
	if s.selectedReadOnly {
		return nil
	}

	ctx, cancel := s.queryCtx()
	defer cancel()

	if err := s.ensureView(ctx); err != nil {
		return err
	}

	type deletion struct {
		seqNum  uint32
		uid     int64
		rawSize int64
	}
	var deletions []deletion

	// One attempt of the whole EXPUNGE. Nothing is written to the wire until
	// after commit, so retryTx may replay this freely (RA6X-021).
	attempt := func(tx pgx.Tx) error {
		deletions = deletions[:0]

		// Canonical lock order (see lockorder.go): mailbox, then folder, then
		// messages. EXPUNGE used to run it backwards — delete the message rows
		// first, then update the mailbox counter, then the folder — which is
		// exactly the cycle STORE could complete from the other side.
		if err := lockMailbox(ctx, tx, s.mailboxID); err != nil {
			return fmt.Errorf("lock mailbox: %w", err)
		}
		// With delete-means-archive on, the \Archive folder may receive rows,
		// so it joins the level-3 lock set, ascending with the selected folder.
		target, err := s.deleteArchiveTarget(ctx, tx)
		if err != nil {
			return fmt.Errorf("delete-archives target: %w", err)
		}
		lockIDs := []int64{s.selectedFolderID}
		if target != nil && !target.inArchive {
			lockIDs = append(lockIDs, target.root.ID)
		}
		if _, err := lockFolders(ctx, tx, lockIDs...); err != nil {
			return fmt.Errorf("lock folder: %w", err)
		}

		// Pull candidates inside the tx to compute sequence numbers over the
		// full pre-delete ordering and to resolve the UID EXPUNGE subset.
		//
		// This read is NOT the authority on what gets deleted. At READ
		// COMMITTED (the server default; nothing here sets a stricter level)
		// the SELECT takes a statement snapshot, so a concurrent session that
		// commits `STORE -FLAGS (\Deleted)` between this read and the DELETE
		// is invisible here. The DELETE below therefore repeats the
		// `\Deleted` predicate and reports what it actually removed via
		// RETURNING; a row un-deleted in the interim is re-evaluated against
		// the latest committed version under the DELETE's row lock and is
		// skipped (RO5X-001).
		candidateQuery := `
			SELECT seqnum, uid, raw_size
			  FROM (
			    SELECT row_number() OVER (ORDER BY uid)::bigint AS seqnum,
			           uid, raw_size, flags
			      FROM messages
			     WHERE folder_id = $1
			  ) m
			 WHERE '\Deleted' = ANY(m.flags)
			 ORDER BY seqnum`

		// UID EXPUNGE: the subset filter is applied in Go via uidSetContains
		// so dynamic ranges ("5:*", "*") resolve against the folder's highest
		// UID per RFC 9051 — and a hostile "1:4000000000" never materializes
		// as an enumerated list.
		var maxUID int64
		if uids != nil {
			if err := tx.QueryRow(ctx,
				`SELECT COALESCE(MAX(uid), 0) FROM messages WHERE folder_id = $1`,
				s.selectedFolderID,
			).Scan(&maxUID); err != nil {
				return fmt.Errorf("max uid: %w", err)
			}
		}

		rows, err := tx.Query(ctx, candidateQuery, s.selectedFolderID)
		if err != nil {
			return fmt.Errorf("candidate query: %w", err)
		}
		var candidates []deletion
		for rows.Next() {
			var d deletion
			var seq int64
			if err := rows.Scan(&seq, &d.uid, &d.rawSize); err != nil {
				rows.Close()
				return fmt.Errorf("candidate scan: %w", err)
			}
			if uids != nil && !uidSetContains(*uids, imap.UID(d.uid), imap.UID(maxUID)) {
				continue
			}
			d.seqNum = uint32(seq)
			candidates = append(candidates, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("candidate rows: %w", err)
		}

		if len(candidates) == 0 {
			return nil
		}

		delUIDs := make([]int64, len(candidates))
		for i, d := range candidates {
			delUIDs[i] = d.uid
		}

		// Level 4 is left to the DELETE itself, deliberately. An explicit
		// SELECT ... FOR UPDATE here would also satisfy the lock order, but it
		// would BLOCK a concurrent `STORE -FLAGS (\Deleted)` until this
		// transaction commits — and the message would then be expunged, which
		// silently reverses RO5X-001's rescue semantics (a client that clears
		// \Deleted in the race window keeps its message). The DELETE below
		// takes its own row locks, after the folder lock, so the canonical
		// order still holds and the rescue still works.
		if s.expungeRaceHook != nil {
			s.expungeRaceHook()
		}

		// Delete means archive: the last copy of a message leaves for the
		// \Archive folder (or, already in the archive, stays) instead of
		// being destroyed. What remains of delUIDs is destroyed below.
		archived := map[int64]struct{}{}
		if target != nil {
			var err error
			archived, delUIDs, err = s.archiveInsteadOfExpunge(ctx, tx, target, delUIDs)
			if err != nil {
				return err
			}
		}

		// Repeat the \Deleted predicate here: DELETE re-evaluates its WHERE
		// against the latest committed row version under a row lock, so a
		// message whose \Deleted was cleared after the candidate read is
		// skipped rather than destroyed. RETURNING reports what was really
		// removed, which is what drives used_bytes and the wire responses.
		delRows, err := tx.Query(ctx,
			`DELETE FROM messages
			  WHERE folder_id = $1 AND uid = ANY($2) AND '\Deleted' = ANY(flags)
			 RETURNING uid, raw_size`,
			s.selectedFolderID, delUIDs,
		)
		if err != nil {
			return fmt.Errorf("delete: %w", err)
		}
		deletedUIDs := make(map[int64]struct{}, len(candidates))
		for uid := range archived {
			deletedUIDs[uid] = struct{}{}
		}
		var totalBytes int64
		for delRows.Next() {
			var uid, rawSize int64
			if err := delRows.Scan(&uid, &rawSize); err != nil {
				delRows.Close()
				return fmt.Errorf("delete scan: %w", err)
			}
			deletedUIDs[uid] = struct{}{}
			// The byte total comes from the DELETE's own RETURNING, not from
			// the earlier snapshot: only rows this statement actually removed
			// may be debited (RA6X-020).
			totalBytes += rawSize
		}
		delRows.Close()
		if err := delRows.Err(); err != nil {
			return fmt.Errorf("delete rows: %w", err)
		}

		// Keep only the candidates the DELETE actually removed, preserving
		// ascending sequence-number order for the renumbering rule below.
		for _, d := range candidates {
			if _, ok := deletedUIDs[d.uid]; ok {
				deletions = append(deletions, d)
			}
		}
		if len(deletions) == 0 {
			// Every candidate was un-deleted concurrently. Nothing was
			// removed, so used_bytes and highest_modseq must not move.
			return nil
		}

		if totalBytes > 0 {
			if _, err := tx.Exec(ctx,
				`UPDATE mailboxes SET used_bytes = GREATEST(0, used_bytes - $1), updated_at = now()
				  WHERE id = $2`,
				totalBytes, s.mailboxID,
			); err != nil {
				return fmt.Errorf("used_bytes: %w", err)
			}
		}

		// CONDSTORE: bump highest_modseq so a resuming client knows the
		// folder mutated. (Full QRESYNC also needs a vanished-uid table
		// to deliver "what disappeared while you were away" — deferred;
		// the bump alone keeps the modseq monotone.)
		if _, err := tx.Exec(ctx,
			`UPDATE folders SET highest_modseq = highest_modseq + 1 WHERE id = $1`,
			s.selectedFolderID,
		); err != nil {
			return fmt.Errorf("modseq bump: %w", err)
		}
		notifyFolderChanged(ctx, tx, s.be.Logger, s.selectedFolderID)
		return nil
	}

	if err := retryTx(ctx, s.be.Pool.Begin, attempt); err != nil {
		s.be.Logger.Error("EXPUNGE", "folder", s.selectedFolderName, "err", err)
		if isRetryableTxError(err) {
			return txAborted("EXPUNGE")
		}
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "EXPUNGE failed"}
	}

	// Address removals through the client's view, including expunges by a peer
	// that Poll has not announced yet. A fresh database row_number would name
	// a different message on the wire. Remove each UID only with its response.
	for _, d := range deletions {
		seq := s.view.seqOf(d.uid)
		if seq == 0 {
			continue
		} // arrival never announced to this client
		if w != nil {
			if err := w.WriteExpunge(seq); err != nil {
				return err
			}
		}
		s.view.remove(d.uid)
	}
	return nil
}

// Copy duplicates the matched messages into dest as new rows pointing
// at the same raw blobs (no data is moved on disk). Attachment rows
// are likewise duplicated under the new message_ids. Returns
// SourceUIDs / DestUIDs for the UIDPLUS COPYUID response.
func (s *Session) Copy(numSet imap.NumSet, dest string) (copyData *imap.CopyData, err error) {
	defer s.guard("COPY", &err)
	if err := s.requireAuth(); err != nil {
		return nil, err
	}
	if s.selectedFolderID == 0 {
		return nil, responseBadState("COPY: no mailbox selected")
	}

	ctx, cancel := s.queryCtx()
	defer cancel()

	rows, err := s.resolveTargets(ctx, numSet)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "COPY: no matching messages"}
	}

	// COPYUID is written only after commit, so a deadlock victim can safely
	// replay the whole thing (RA6X-021).
	var data *imap.CopyData
	if err := retryTx(ctx, s.be.Pool.Begin, func(tx pgx.Tx) error {
		var err error
		data, err = s.copyInTx(ctx, tx, rows, dest, 0)
		return err
	}); err != nil {
		if isRetryableTxError(err) {
			s.be.Logger.Warn("COPY aborted after retries", "dest", dest, "err", err)
			return nil, txAborted("COPY")
		}
		var ierr *imap.Error
		if errors.As(err, &ierr) {
			return nil, ierr
		}
		s.be.Logger.Error("COPY", "dest", dest, "err", err)
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "COPY failed"}
	}
	return data, nil
}

// copyInTx performs the row-by-row copy inside the caller's
// transaction. It does NOT commit — that's the caller's job, so MOVE
// can fold the subsequent DELETE into the same tx.
//
// rows are the resolved source messages (already from resolveTargets);
// dest is the destination folder name (auto-created if absent).
// blobRef is one content-addressed blob a COPY/MOVE will create a new
// reference to: (kind, bucket, sha-hex) under the session's tenant.
type blobRef struct {
	kind   blob.Kind
	bucket blob.Bucket
	sha    string // lowercase hex
	size   int64
}

// lockCopyBlobs implements the R-061 fix: for every raw + attachment blob the
// source rows reference, take the per-blob advisory lock (sorted + deduped, the
// same key derivation and ordering storage.Ingest uses), drop any gc_candidates
// row, and verify its size and digest — failing the COPY if it is missing
// or corrupt (EnsureBlobs-from-memory isn't possible here; the bytes
// aren't in hand). COPY/MOVE is always intra-mailbox, so every blob stays under
// s.tenant. Locks release at tx commit/rollback.
func (s *Session) lockCopyBlobs(ctx context.Context, tx pgx.Tx, rows []targetRef) error {
	if len(rows) == 0 {
		return nil
	}
	uids := make([]int64, len(rows))
	for i, r := range rows {
		uids[i] = r.uid
	}

	var refs []blobRef
	collect := func(query string, kind blob.Kind) error {
		q, err := tx.Query(ctx, query, s.selectedFolderID, uids)
		if err != nil {
			return err
		}
		defer q.Close()
		for q.Next() {
			var sha string
			var d time.Time
			var size int64
			if err := q.Scan(&sha, &d, &size); err != nil {
				return err
			}
			refs = append(refs, blobRef{kind: kind, bucket: blob.BucketFromTime(d), sha: sha, size: size})
		}
		return q.Err()
	}
	if err := collect(
		`SELECT DISTINCT encode(raw_sha256, 'hex'), raw_blob_date, raw_size
		   FROM messages WHERE folder_id = $1 AND uid = ANY($2)`,
		blob.KindRaw); err != nil {
		s.be.Logger.Error("COPY blob-lock: scan raw refs", "err", err)
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "COPY blob lock failed"}
	}
	if err := collect(
		`SELECT DISTINCT encode(a.sha256, 'hex'), a.blob_date, a.size_bytes
		   FROM attachments a JOIN messages m ON m.id = a.message_id
		  WHERE m.folder_id = $1 AND m.uid = ANY($2)`,
		blob.KindAttachment); err != nil {
		s.be.Logger.Error("COPY blob-lock: scan attachment refs", "err", err)
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "COPY blob lock failed"}
	}
	if len(refs) == 0 {
		return nil
	}

	// Acquire advisory locks in a stable sorted+deduped order (deadlock-free
	// against Ingest, which does the same; sweep takes one key per tx).
	keys := make([]int64, len(refs))
	for i, r := range refs {
		keys[i] = storage.BlobAdvisoryLockKey(string(s.tenant), string(r.kind), string(r.bucket), r.sha)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	var prev int64
	for i, k := range keys {
		if i > 0 && k == prev {
			continue
		}
		prev = k
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, k); err != nil {
			s.be.Logger.Error("COPY blob-lock: acquire", "err", err)
			return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "COPY blob lock failed"}
		}
	}

	// Holding the locks: resurrect any candidates and verify the blobs survive.
	for _, r := range refs {
		if _, err := tx.Exec(ctx,
			`DELETE FROM gc_candidates
			  WHERE tenant = $1 AND sha256 = decode($2, 'hex') AND kind = $3 AND bucket = $4`,
			string(s.tenant), r.sha, string(r.kind), string(r.bucket)); err != nil {
			s.be.Logger.Error("COPY blob-lock: clear candidate", "err", err)
			return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "COPY blob lock failed"}
		}
		exists, err := s.be.BlobStore.ContentMatches(r.kind, s.tenant, r.bucket, r.sha, r.size)
		if err != nil {
			s.be.Logger.Error("COPY blob-lock: stat blob", "err", err,
				"sha16", safeShortSHA(r.sha), "kind", r.kind, "bucket", r.bucket)
			return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "COPY blob lock failed"}
		}
		if !exists {
			s.be.Logger.Error("COPY source blob missing or corrupt",
				"mailbox", s.mailboxName, "sha16", safeShortSHA(r.sha),
				"kind", r.kind, "bucket", r.bucket)
			return &imap.Error{
				Type: imap.StatusResponseTypeNo,
				Code: imap.ResponseCodeServerBug,
				Text: "COPY source blob unavailable",
			}
		}
	}
	return nil
}

// copyInTx duplicates rows into dest inside an existing transaction.
//
// quotaOffset is a byte count to subtract from the destination mailbox's
// used_bytes in the SAME statement as the copy's increment. MOVE passes the
// source bytes it is about to delete; COPY passes 0. Applying both halves in
// one update is what stops a same-mailbox MOVE transiently doubling the
// accounting and tripping the quota check on bytes it is simultaneously
// freeing (RO5X-011).
func (s *Session) copyInTx(ctx context.Context, tx pgx.Tx, rows []targetRef, dest string, quotaOffset int64) (*imap.CopyData, error) {
	// Canonical lock order (lockorder.go): mailbox, blob advisory locks,
	// folders, messages. The accounting UPDATE at the end of this function
	// needs the mailbox row, so it is taken HERE rather than there — otherwise
	// COPY holds folders and message rows while waiting for a mailbox that
	// delivery already holds while waiting for the same folder (RA6X-021).
	if err := lockMailbox(ctx, tx, s.mailboxID); err != nil {
		return nil, fmt.Errorf("lock mailbox: %w", err)
	}

	// Join the GC advisory-lock protocol before creating new references to the
	// source blobs. Without this, COPY snapshots raw_sha256/raw_blob_date into
	// a new row outside the lock a concurrent `gc sweep` holds, so a sweep that
	// unlinked a grace-expired candidate between the snapshot and this commit
	// would leave a dangling row → NO [SERVERBUG] / epistula-api 500 forever
	// (R-061).
	if err := s.lockCopyBlobs(ctx, tx, rows); err != nil {
		return nil, err
	}

	destFolderID, destUIDValidity, err := s.lookupOrCreateFolderInTx(ctx, tx, dest)
	if err != nil {
		return nil, err
	}

	if len(rows) == 0 {
		var empty imap.UIDSet
		return &imap.CopyData{
			UIDValidity: uint32(destUIDValidity),
			SourceUIDs:  empty,
			DestUIDs:    empty,
		}, nil
	}

	// Level 3: both folders, ascending. Sorting is what makes two MOVEs in
	// opposite directions between the same pair of folders safe.
	if _, err := lockFolders(ctx, tx, s.selectedFolderID, destFolderID); err != nil {
		return nil, fmt.Errorf("lock folders: %w", err)
	}

	// Level 4: pin the SOURCE rows for the rest of the transaction (RA6X-002).
	//
	// Every statement below reads the source again — the destination
	// INSERT ... SELECT, and the attachment INSERT that re-joins `messages` to
	// find the parts to duplicate. Without this lock those are separate READ
	// COMMITTED reads, so a concurrent EXPUNGE or folder DELETE committing
	// between them produced a destination message with ZERO attachments and no
	// error at all. It also makes the byte total below authoritative: MOVE used
	// to hand copyInTx a pre-read count and assume those bytes still existed.
	locked, err := lockMessagesByUID(ctx, tx, s.selectedFolderID, sourceUIDsOf(rows))
	if err != nil {
		return nil, fmt.Errorf("lock source messages: %w", err)
	}
	// A source that vanished before this lock could be taken means the caller's
	// target set no longer describes reality. COPY and MOVE are all-or-nothing
	// (RFC 4315's COPYUID contract requires the two sets to correspond), so
	// this fails cleanly instead of silently copying a subset.
	if len(locked) != len(rows) {
		s.be.Logger.Warn("COPY/MOVE source set changed before it could be locked",
			"mailbox", s.mailboxName, "folder", s.selectedFolderName,
			"requested", len(rows), "present", len(locked))
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "COPY: source messages changed, please retry",
		}
	}

	// Set-wise copy: four statements total, not four per message.
	//
	// The per-row loop cost four round trips each (UID+modseq allocation,
	// the messages INSERT, the attachments INSERT, the used_bytes UPDATE), so
	// `COPY 1:*` over a 20 000-message folder was 80 000 statements in one
	// transaction. Worse, the very first statement takes an exclusive row
	// lock on the destination folders row and holds it until commit — and the
	// LDA's delivery path runs the same `UPDATE folders … RETURNING`, so every
	// concurrent delivery into that folder blocked for the whole COPY. With
	// the delivery watchdog at 60 s, a COPY longer than that made Postfix
	// defer every message to that folder: a self-inflicted mail delay
	// triggered by a user dragging a folder in Mail.app (RO5X-019).
	srcUIDs := make([]imap.UID, 0, len(rows))
	for _, r := range rows {
		srcUIDs = append(srcUIDs, imap.UID(r.uid))
	}
	srcUIDList := make([]int64, 0, len(rows))
	for _, r := range rows {
		srcUIDList = append(srcUIDList, r.uid)
	}

	// 1. Reserve the whole UID block and modseq range in ONE statement, so
	//    the folder row lock is taken once and held for a fraction of the
	//    time.
	n := int64(len(rows))
	var firstUID, firstModSeq int64
	if err := tx.QueryRow(ctx,
		`UPDATE folders
		    SET uidnext        = uidnext + $2,
		        highest_modseq = highest_modseq + $2
		  WHERE id = $1
		 RETURNING uidnext - $2, highest_modseq - $2 + 1`,
		destFolderID, n,
	).Scan(&firstUID, &firstModSeq); err != nil {
		s.be.Logger.Error("COPY uid block alloc", "err", err)
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "COPY uid alloc failed"}
	}

	if err := protocolIDs(firstUID, firstUID+n); err != nil {
		return nil, err
	}

	// 2. Insert every message row in one statement, assigning UIDs by
	//    ascending SOURCE uid.
	//
	//    RFC 4315 requires COPYUID's source and destination sets to
	//    correspond positionally in ascending source order. resolveTargets
	//    already returns ascending uid order, and row_number() OVER (ORDER BY
	//    m.uid) preserves it — asserted in the test.
	//
	//    RETURNING carries the source uid back out so the attachment copy can
	//    join old-uid → new-message-id without a second lookup.
	type newRow struct {
		newID  int64
		newUID int64
		srcUID int64
	}
	insRows, err := tx.Query(ctx, `
		WITH src AS (
			SELECT m.*, row_number() OVER (ORDER BY m.uid) - 1 AS ord
			  FROM messages m
			 WHERE m.folder_id = $1 AND m.uid = ANY($2)
		)
		INSERT INTO messages (
			folder_id, uid, raw_sha256, raw_blob_date, raw_size,
			internal_date, message_id, in_reply_to, subject,
			from_addr, to_addrs, cc_addrs, sent_date, sent_date_local, headers,
			text_body, html_body, bodystructure, flags, mod_seq
		)
		SELECT $3, $4 + src.ord, src.raw_sha256, src.raw_blob_date, src.raw_size,
		       src.internal_date, src.message_id, src.in_reply_to, src.subject,
		       src.from_addr, src.to_addrs, src.cc_addrs, src.sent_date, src.sent_date_local, src.headers,
		       src.text_body, src.html_body, src.bodystructure, src.flags,
		       $5 + src.ord
		  FROM src
		 ORDER BY src.uid
		RETURNING id, uid, (SELECT s2.uid FROM src s2 WHERE $4 + s2.ord = messages.uid)`,
		s.selectedFolderID, srcUIDList, destFolderID, firstUID, firstModSeq,
	)
	if err != nil {
		s.be.Logger.Error("COPY message insert", "err", err)
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "COPY insert failed"}
	}
	inserted := make([]newRow, 0, len(rows))
	for insRows.Next() {
		var nr newRow
		if err := insRows.Scan(&nr.newID, &nr.newUID, &nr.srcUID); err != nil {
			insRows.Close()
			s.be.Logger.Error("COPY insert scan", "err", err)
			return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "COPY insert failed"}
		}
		inserted = append(inserted, nr)
	}
	insRows.Close()
	if err := insRows.Err(); err != nil {
		s.be.Logger.Error("COPY insert rows", "err", err)
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "COPY insert failed"}
	}
	if len(inserted) != len(rows) {
		s.be.Logger.Error("COPY inserted row count mismatch",
			"want", len(rows), "got", len(inserted))
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "COPY insert failed"}
	}

	if s.copyRaceHook != nil {
		s.copyRaceHook()
	}

	// 3. Duplicate attachments for the whole set in one statement, joining on
	//    the (source uid → new message id) map built above.
	oldUIDs := make([]int64, 0, len(inserted))
	newIDs := make([]int64, 0, len(inserted))
	for _, nr := range inserted {
		oldUIDs = append(oldUIDs, nr.srcUID)
		newIDs = append(newIDs, nr.newID)
	}
	if _, err := tx.Exec(ctx, `
		WITH map AS (
			SELECT * FROM unnest($1::bigint[], $2::bigint[]) AS t(old_uid, new_id)
		)
		INSERT INTO attachments (
			message_id, part_number, filename, content_type,
			content_id, disposition, size_bytes, sha256, blob_date
		)
		SELECT map.new_id, a.part_number, a.filename, a.content_type,
		       a.content_id, a.disposition, a.size_bytes, a.sha256, a.blob_date
		  FROM map
		  JOIN messages sm ON sm.folder_id = $3 AND sm.uid = map.old_uid
		  JOIN attachments a ON a.message_id = sm.id`,
		oldUIDs, newIDs, s.selectedFolderID,
	); err != nil {
		s.be.Logger.Error("COPY attachments", "err", err)
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "COPY attachments failed"}
	}

	// 4. Carry the derived annotation sidecars across (RA6X-003).
	//
	// message_annotations is keyed (message_id, model) and cascades on delete,
	// so a MOVE — which creates a new message id and deletes the old one —
	// destroyed every summary, category, tag, model attribution and token
	// count the annotation worker had produced, simply because a user filed a
	// message. COPY produced an unannotated duplicate for the worker to redo.
	//
	// Copying rather than re-inferring is the point: the annotation belongs to
	// the message's content, which is byte-identical (both rows reference the
	// same immutable blob), so re-running a model would spend GPU time to
	// arrive at the same answer with a later created_at. created_at is
	// preserved for that reason — it records when the model looked at this
	// mail, not when the mail was filed.
	//
	// The source rows are pinned FOR UPDATE (RA6X-002), so a concurrent
	// annotation PUT either lands before this reads, or waits and applies to
	// the source that is about to be deleted; either way the destination gets
	// a coherent snapshot rather than a half-updated one.
	if _, err := tx.Exec(ctx, `
		WITH map AS (
			SELECT * FROM unnest($1::bigint[], $2::bigint[]) AS t(old_uid, new_id)
		)
		INSERT INTO message_annotations (
			message_id, model, tags, category, summary, tokens_in, tokens_out, created_at
		)
		SELECT map.new_id, an.model, an.tags, an.category, an.summary,
		       an.tokens_in, an.tokens_out, an.created_at
		  FROM map
		  JOIN messages sm ON sm.folder_id = $3 AND sm.uid = map.old_uid
		  JOIN message_annotations an ON an.message_id = sm.id
		ON CONFLICT (message_id, model) DO NOTHING`,
		oldUIDs, newIDs, s.selectedFolderID,
	); err != nil {
		s.be.Logger.Error("COPY annotations", "err", err)
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "COPY annotations failed"}
	}

	// 5. And the archive classification (migration 020), for the same reason
	//    and with more at stake: archiving a message from INBOX is a MOVE into
	//    the \Archive folder, and the live sorter files it by exactly this
	//    row. Losing it here would leave every archived message unsorted until
	//    the worker classified the same content a second time.
	if _, err := tx.Exec(ctx, `
		WITH map AS (
			SELECT * FROM unnest($1::bigint[], $2::bigint[]) AS t(old_uid, new_id)
		)
		INSERT INTO message_classifications (message_id, category, confidence, model, created_at)
		SELECT map.new_id, c.category, c.confidence, c.model, c.created_at
		  FROM map
		  JOIN messages sm ON sm.folder_id = $3 AND sm.uid = map.old_uid
		  JOIN message_classifications c ON c.message_id = sm.id
		ON CONFLICT (message_id) DO NOTHING`,
		oldUIDs, newIDs, s.selectedFolderID,
	); err != nil {
		s.be.Logger.Error("COPY classifications", "err", err)
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "COPY classifications failed"}
	}

	// 6. And the annotation pipeline's marker (migration 022): a message the
	//    worker has not finished yet stays queued under its new id, which a
	//    MOVE would otherwise drop with the source row. A finished one has no
	//    marker, and its copy arrives finished, sidecars and all.
	if _, err := tx.Exec(ctx, `
		WITH map AS (
			SELECT * FROM unnest($1::bigint[], $2::bigint[]) AS t(old_uid, new_id)
		)
		INSERT INTO annotation_pass_required (message_id, marked_at)
		SELECT map.new_id, p.marked_at
		  FROM map
		  JOIN messages sm ON sm.folder_id = $3 AND sm.uid = map.old_uid
		  JOIN annotation_pass_required p ON p.message_id = sm.id
		ON CONFLICT (message_id) DO NOTHING`,
		oldUIDs, newIDs, s.selectedFolderID,
	); err != nil {
		s.be.Logger.Error("COPY annotation pass markers", "err", err)
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "COPY annotation pass markers failed"}
	}

	// Destination UIDs, in ascending source order (the RFC 4315 contract).
	destUIDs := make([]imap.UID, 0, len(inserted))
	var copiedBytes int64
	sort.Slice(inserted, func(i, j int) bool { return inserted[i].srcUID < inserted[j].srcUID })
	for _, nr := range inserted {
		destUIDs = append(destUIDs, imap.UID(nr.newUID))
	}
	// Bytes come from the LOCKED source rows, not from the caller's earlier
	// unlocked read: those are the rows that were actually copied, and they
	// cannot change underneath this transaction (RA6X-002).
	for _, m := range locked {
		copiedBytes += m.rawSize
	}

	// One accounting update for the whole copy, with the quota enforced once
	// against the final value.
	//
	// COPY used to increment used_bytes per row and never compare it to
	// quota_bytes, so an authenticated user at 100% of quota could
	// `COPY 1:* Archive` repeatedly and inflate the counter without bound.
	// No new blobs are written (COPY is intra-mailbox and the rows point at
	// the same content), but used_bytes is the number epistula-api reports and
	// `gc reconcile-quotas` reconciles, and once inflated, delivery to that
	// mailbox starts bouncing EX_CANTCREAT against a quota the user
	// manufactured (RO5X-011).
	delta := copiedBytes - quotaOffset
	if delta != 0 {
		var usedBytes int64
		var quotaBytes *int64
		if err := tx.QueryRow(ctx,
			`UPDATE mailboxes
			    SET used_bytes = GREATEST(0, used_bytes + $1), updated_at = now()
			  WHERE id = $2
			 RETURNING used_bytes, quota_bytes`,
			delta, s.mailboxID,
		).Scan(&usedBytes, &quotaBytes); err != nil {
			s.be.Logger.Error("COPY used_bytes", "err", err)
			return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "COPY quota update failed"}
		}
		// Only a copy that actually ADDS bytes can breach the quota. A
		// net-zero or net-negative operation (a same-mailbox MOVE, or a
		// MOVE that frees more than it adds) must never be refused — it
		// does not grow the mailbox, and refusing it would strand a user
		// whose quota an operator lowered below current usage.
		//
		// NULL quota_bytes means unlimited. Returning here rolls the whole
		// transaction back, which is the right all-or-nothing semantics for
		// the RFC 4315 COPYUID contract.
		if delta > 0 && quotaBytes != nil && usedBytes > *quotaBytes {
			return nil, &imap.Error{
				Type: imap.StatusResponseTypeNo,
				Code: imap.ResponseCodeOverQuota,
				Text: "mailbox over quota",
			}
		}
	}

	notifyFolderChanged(ctx, tx, s.be.Logger, s.selectedFolderID, destFolderID)

	var srcSet imap.UIDSet
	srcSet.AddNum(srcUIDs...)
	var destSet imap.UIDSet
	destSet.AddNum(destUIDs...)
	return &imap.CopyData{
		UIDValidity: uint32(destUIDValidity),
		SourceUIDs:  srcSet,
		DestUIDs:    destSet,
	}, nil
}

// Move implements RFC 6851 IMAP MOVE: atomic copy + delete of the
// matched messages in a single Postgres transaction. A crash between
// the copy and the delete cannot leave duplicates — either both halves
// commit or neither does.
//
// Note "delete" here is unconditional (every matched source row), not
// EXPUNGE's \Deleted-flag gate. That's the RFC 6851 semantics; the
// EXPUNGE responses we emit afterwards are purely the on-the-wire
// notification.
func (s *Session) Move(w *imapserver.MoveWriter, numSet imap.NumSet, dest string) (err error) {
	defer s.guard("MOVE", &err)
	if err := s.requireAuth(); err != nil {
		return err
	}
	if s.selectedFolderID == 0 {
		return responseBadState("MOVE: no mailbox selected")
	}
	// COPY out of an EXAMINEd mailbox is legal (the source is only read);
	// MOVE is not — it deletes from the read-only selection.
	if err := s.requireWritable("MOVE"); err != nil {
		return err
	}

	ctx, cancel := s.queryCtx()
	defer cancel()

	rows, err := s.resolveTargets(ctx, numSet)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "MOVE: no matching messages"}
	}

	// The bytes the delete half is about to free. Handed to copyInTx as the
	// quota offset so the increment and the decrement land in ONE statement:
	// COPY/MOVE are always intra-mailbox (the destination folder is resolved
	// under s.mailboxID), so a MOVE is quota-neutral in aggregate and must
	// never transiently double the accounting on its way there (RO5X-011).
	uids := make([]int64, 0, len(rows))
	var totalBytes int64
	for _, r := range rows {
		uids = append(uids, r.uid)
		totalBytes += r.rawSize
	}

	// COPYUID and the EXPUNGE responses are written only after commit, so a
	// deadlock victim can safely replay the whole thing (RA6X-021).
	var copyData *imap.CopyData
	moveTx := func(tx pgx.Tx) error {
		// Copy half.
		var err error
		copyData, err = s.copyInTx(ctx, tx, rows, dest, totalBytes)
		if err != nil {
			return err
		}

		// Delete half: unconditional removal of the source UIDs, in the same tx
		// as the copy. The used_bytes decrement already happened above, folded
		// into copyInTx's single accounting update.
		//
		// The affected-row count is CHECKED, not ignored (RA6X-002). copyInTx
		// pinned these exact rows FOR UPDATE, so a mismatch here cannot be a
		// concurrent expunge — it would be a bug in the set arithmetic, and
		// committing a MOVE that deleted a different number of rows than it
		// copied would leave the mailbox counter wrong in a way `gc
		// reconcile-quotas` would silently paper over later.
		tag, err := tx.Exec(ctx,
			`DELETE FROM messages WHERE folder_id = $1 AND uid = ANY($2)`,
			s.selectedFolderID, uids,
		)
		if err != nil {
			s.be.Logger.Error("MOVE delete", "err", err)
			return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "MOVE delete failed"}
		}
		if got, want := tag.RowsAffected(), int64(len(uids)); got != want {
			s.be.Logger.Error("MOVE deleted a different number of rows than it copied",
				"mailbox", s.mailboxName, "folder", s.selectedFolderName,
				"deleted", got, "copied", want)
			return &imap.Error{
				Type: imap.StatusResponseTypeNo,
				Code: imap.ResponseCodeServerBug,
				Text: "MOVE source set changed",
			}
		}

		// Bump source folder's highest_modseq so a resuming client sees
		// "something changed" on the source. (The dest folder's modseq was
		// already bumped per-row inside copyInTx.)
		if _, err := tx.Exec(ctx,
			`UPDATE folders SET highest_modseq = highest_modseq + 1 WHERE id = $1`,
			s.selectedFolderID,
		); err != nil {
			s.be.Logger.Error("MOVE source modseq bump", "err", err)
			return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "MOVE source modseq bump failed"}
		}
		return nil
	}

	if err := retryTx(ctx, s.be.Pool.Begin, moveTx); err != nil {
		if isRetryableTxError(err) {
			s.be.Logger.Warn("MOVE aborted after retries", "dest", dest, "err", err)
			return txAborted("MOVE")
		}
		var ierr *imap.Error
		if errors.As(err, &ierr) {
			return ierr
		}
		s.be.Logger.Error("MOVE", "dest", dest, "err", err)
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "MOVE failed"}
	}

	// The source messages have left this folder and the client is about to be
	// told so; the view must match the wire (RA6X-001).
	for _, r := range rows {
		s.view.remove(r.uid)
	}

	// Post-commit: emit the protocol responses. The MoveWriter
	// contract is COPYUID once, then EXPUNGE per source row.
	if w != nil {
		if err := w.WriteCopyData(copyData); err != nil {
			return err
		}
		for i, r := range rows {
			if err := w.WriteExpunge(r.seqNum - uint32(i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// lookupOrCreateFolderInTx returns (folderID, uidvalidity), creating
// the folder if it doesn't exist. Caller's transaction holds throughout.
func (s *Session) lookupOrCreateFolderInTx(ctx context.Context, tx pgx.Tx, name string) (int64, int64, error) {
	// COPY/MOVE destinations go through the SAME gate as CREATE (RA6X-009).
	//
	// This path validated nothing and canonicalized nothing, so a name CREATE
	// refuses — a control character, a 300-byte name — walked into the store
	// through a drag-and-drop, and `COPY 1 inbox` created a second, literal
	// "inbox" alongside the real INBOX that SELECT could never open.
	//
	// Auto-creation itself is deliberate and unchanged: dragging to a folder
	// that does not exist yet is what Mail.app expects, and the destination
	// having to be legal does not make it any less automatic.
	name, verr := prepareFolderWrite(name)
	if verr != nil {
		return 0, 0, verr
	}

	// Look up, or create on the fly so COPY/MOVE to a new folder works the
	// way Mail.app expects ("drag to a folder that doesn't exist yet").
	// storage.EnsureFolder is the creation path every writer shares
	// (OPS-001): missing ancestors first, so an auto-created `Archive/2026`
	// does not leave a child with no parent in LIST — the hierarchy rule
	// CREATE follows (RO5X-008, RA6X-009) — and uidvalidity from the shared
	// folder_uidvalidity_seq (R-062), returned so the caller's
	// COPYUID/UIDVALIDITY responses report the real one. copyInTx already
	// holds the mailbox row lock it requires.
	folder, _, err := storage.EnsureFolder(ctx, tx, s.mailboxID, name, nil)
	if err != nil {
		s.be.Logger.Error("auto-create folder", "name", name, "err", err)
		return 0, 0, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "folder create failed"}
	}
	return folder.ID, folder.UIDValidity, protocolIDs(folder.UIDValidity)
}

// flagSliceToStrings canonicalizes the flags a client sent before they are
// persisted (R-063): system flags are case-insensitive per RFC 9051 §2.3.2,
// but STORE used to persist whatever case arrived, so `+FLAGS (\SEEN)` left
// `\SEEN` in messages.flags and every byte-exact comparison downstream
// silently disagreed.
//
// The map itself now lives in epistula-database's imapflags package so the read
// paths here and in epistula-api share exactly one copy (RO5X-013). Keyword flags
// pass through untouched.
func flagSliceToStrings(fs []imap.Flag) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, imapflags.Canonical(string(f)))
	}
	return out
}

// sourceUIDsOf extracts the UID list from a resolved target set.
func sourceUIDsOf(rows []targetRef) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.uid)
	}
	return out
}
