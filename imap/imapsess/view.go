package imapsess

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/jackc/pgx/v5"
)

// sessionView is what this session has told its client the selected folder
// contains: an ordered list of UIDs, plus the mod_seq it last reported for
// each (RA6X-001).
//
// Without it, every command derived sequence numbers from a fresh
// `row_number() OVER (ORDER BY uid)` against the current database. Two clients
// in one mailbox is the ordinary case, not an exotic one, and the consequence
// is silent: if A sees UIDs [10,20,30], B expunges 10, and A then issues
// `STORE 2 +FLAGS (\Deleted)`, the server resolves 2 to UID 30 — while A still
// believes 2 is UID 20. A subsequent EXPUNGE makes the wrong message's
// deletion permanent. The protocol's answer is that sequence numbers may only
// change through an EXPUNGE the client has been sent, which requires the
// server to remember what it has sent.
//
// The view is the authority for translating a client's sequence numbers, and
// the database is the authority for what the view should become: reconcile
// re-reads the folder and computes the untagged responses that carry the
// client from its current view to the new one. Nothing renumbers a client
// without the matching wire response.
//
// A view is per-session state; it needs no schema.
type sessionView struct {
	// uids is ascending, and its index+1 is the client's sequence number.
	uids []int64
	// modSeq records the mod_seq last reported for each UID, so a flag change
	// made by another session can be detected and pushed as FETCH FLAGS.
	modSeq map[int64]int64
}

// viewRow is one row of the folder as the database currently has it.
type viewRow struct {
	uid    int64
	modSeq int64
}

// newSessionView builds a view from a folder snapshot.
func newSessionView(rows []viewRow) *sessionView {
	v := &sessionView{
		uids:   make([]int64, 0, len(rows)),
		modSeq: make(map[int64]int64, len(rows)),
	}
	for _, r := range rows {
		v.uids = append(v.uids, r.uid)
		v.modSeq[r.uid] = r.modSeq
	}
	return v
}

// len reports how many messages the client believes are in the folder — the
// number a bare EXISTS would carry.
func (v *sessionView) len() int {
	if v == nil {
		return 0
	}
	return len(v.uids)
}

// maxUID is the largest UID in the view, which is what `*` expands to in a UID
// set. It must come from the view rather than the database: `*` means "the
// largest UID this client knows about".
func (v *sessionView) maxUID() imap.UID {
	if v == nil || len(v.uids) == 0 {
		return 0
	}
	return imap.UID(v.uids[len(v.uids)-1])
}

// contains reports whether uid is addressable by this client.
func (v *sessionView) contains(uid int64) bool {
	if v == nil {
		return false
	}
	_, ok := v.modSeq[uid]
	return ok
}

// seqOf returns the 1-based sequence number of uid, or 0 when the client does
// not know about it.
func (v *sessionView) seqOf(uid int64) uint32 {
	if v == nil {
		return 0
	}
	i, found := slices.BinarySearch(v.uids, uid)
	if !found {
		return 0
	}
	return uint32(i + 1)
}

// remove drops uid from the view. Used after this session's own EXPUNGE or
// MOVE, which have already written the matching untagged responses.
func (v *sessionView) remove(uid int64) {
	if v == nil {
		return
	}
	for i, u := range v.uids {
		if u == uid {
			v.uids = append(v.uids[:i], v.uids[i+1:]...)
			break
		}
	}
	delete(v.modSeq, uid)
}

// note records a mod_seq this session has just reported for uid, so a later
// reconcile does not push the session's own change back at it.
func (v *sessionView) note(uid, modSeq int64) {
	if v == nil {
		return
	}
	if _, known := v.modSeq[uid]; known {
		v.modSeq[uid] = modSeq
	}
}

// viewDelta is the set of untagged responses that carry a client from its
// current view to a new folder snapshot.
type viewDelta struct {
	// expunged holds sequence numbers in the order they must be written,
	// already adjusted for the renumbering each prior EXPUNGE causes.
	expunged []uint32
	// numMessages is the new EXISTS count, set only when it changed after the
	// expunges were applied.
	numMessages uint32
	hasExists   bool
	// flagChanged holds UIDs whose flags moved under this client, in
	// ascending order.
	flagChanged []int64
}

// diff computes the delta from the view to a snapshot WITHOUT mutating the
// view, so a caller that cannot write to the wire can decide whether to
// bother.
func (v *sessionView) diff(snapshot []viewRow) viewDelta {
	var d viewDelta
	if v == nil {
		return d
	}
	present := make(map[int64]int64, len(snapshot))
	for _, r := range snapshot {
		present[r.uid] = r.modSeq
	}

	// Expunges, in ascending sequence order with the renumbering rule applied:
	// each removal shifts every later sequence number down by one.
	removed := 0
	for i, uid := range v.uids {
		if _, ok := present[uid]; ok {
			continue
		}
		d.expunged = append(d.expunged, uint32(i+1-removed))
		removed++
	}

	// Flag changes, only for messages the client still has.
	for _, uid := range v.uids {
		newSeq, ok := present[uid]
		if !ok {
			continue
		}
		if newSeq != v.modSeq[uid] {
			d.flagChanged = append(d.flagChanged, uid)
		}
	}

	// EXISTS reflects the snapshot: arrivals plus whatever survived.
	if len(snapshot) != len(v.uids)-removed || removed > 0 {
		d.numMessages = uint32(len(snapshot))
		d.hasExists = true
	}
	return d
}

// apply replaces the view with the snapshot. Call only after the delta's
// responses have been written.
func (v *sessionView) apply(snapshot []viewRow) {
	if v == nil {
		return
	}
	v.uids = v.uids[:0]
	clear(v.modSeq)
	for _, r := range snapshot {
		v.uids = append(v.uids, r.uid)
		v.modSeq[r.uid] = r.modSeq
	}
}

// folderSnapshot reads the current (uid, mod_seq) list for a folder in
// ascending UID order — the database's view, against which a session's is
// reconciled.
func (s *Session) folderSnapshot(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, folderID int64) ([]viewRow, error) {
	rows, err := q.Query(ctx,
		`SELECT uid, mod_seq FROM messages WHERE folder_id = $1 ORDER BY uid`, folderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []viewRow
	for rows.Next() {
		var r viewRow
		if err := rows.Scan(&r.uid, &r.modSeq); err != nil {
			return nil, err
		}
		if err := protocolIDs(r.uid); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// errReconcileBackend marks a reconcile that failed in the database rather
// than on the wire (OPS-007). Nothing is lost when it does: a change the
// client was not told about still differs between the view and the folder,
// and the stamp that lets a reconcile skip an unchanged folder is recorded
// only on success, so the next reconcile repeats that part of the delta.
var errReconcileBackend = errors.New("imapsess: reconcile failed in the database")

// reconcile brings the client's view up to date with the database and writes
// the untagged responses that justify the change (RA6X-001).
//
// allowExpunge is the protocol's rule that EXPUNGE may not be sent while a
// command that references sequence numbers is in flight. When it is false the
// expunges are NOT applied to the view either — a client must never be
// renumbered without being told — so nothing is lost: the next reconcile that
// may speak will send them.
//
// A nil writer means "update quietly", used where there is no wire to write to.
//
// A failure to begin the snapshot or to read the folder is logged and returns
// nil. A failed commit or flag read wraps errReconcileBackend, so a caller
// that polls can retry it rather than stop.
func (s *Session) reconcile(ctx context.Context, w *imapserver.UpdateWriter, allowExpunge bool) error {
	if s.selectedFolderID == 0 || s.view == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, s.setupTimeout())
	defer cancel()
	tx, err := s.be.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		s.be.Logger.Warn("reconcile: begin snapshot", "err", err)
		return nil
	}
	defer tx.Rollback(ctx)
	var generation, stamp int64
	err = tx.QueryRow(ctx, `SELECT uidvalidity,highest_modseq FROM folders WHERE id=$1 AND mailbox_id=$2`, s.selectedFolderID, s.mailboxID).Scan(&generation, &stamp)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && s.selectedUIDValidity != 0 && generation != s.selectedUIDValidity) {
		s.selectedFolderID = 0
		s.view = nil
		s.viewStampKnown = false
		s.closeStaleSession()
		return responseCannot("selected mailbox generation changed; reconnect")
	}
	if err != nil {
		s.be.Logger.Warn("reconcile: folder counters", "err", err)
		return nil
	}
	if err := protocolIDs(generation); err != nil {
		s.closeStaleSession()
		return err
	}
	if s.selectedUIDValidity == 0 {
		s.selectedUIDValidity = generation
	}
	// All supported flag/arrival/expunge writers advance this counter. A stable
	// folder requires one indexed row read, not a transfer of the entire UID set.
	if s.viewStampKnown && stamp == s.viewFolderModSeq {
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("%w: commit: %w", errReconcileBackend, err)
		}
		return nil
	}
	snapshot, err := s.folderSnapshot(ctx, tx, s.selectedFolderID)
	if err != nil {
		s.be.Logger.Warn("reconcile: folder snapshot", "err", err)
		return nil
	}

	d := s.view.diff(snapshot)
	if len(d.expunged) > 0 && !allowExpunge {
		// Defer the WHOLE delta. Applying the arrivals now would renumber
		// nothing (they are appended), but applying the flag changes without
		// the expunges would leave the view inconsistent with what the client
		// was told, and an EXISTS that implied the expunges had happened would
		// be worse still.
		return nil
	}
	if w == nil {
		// Nothing to write to; leave the view alone so the responses are not
		// silently skipped.
		return nil
	}

	for _, seq := range d.expunged {
		if err := w.WriteExpunge(seq); err != nil {
			return err
		}
	}
	if d.hasExists {
		if err := w.WriteNumMessages(d.numMessages); err != nil {
			return err
		}
	}
	// Flag updates are written after EXISTS so a client that learns about a
	// message and its flags in one poll sees them in a coherent order. They
	// are addressed by the NEW sequence numbers, so the view is applied first.
	previous := make(map[int64]int64, len(d.flagChanged))
	for _, uid := range d.flagChanged {
		previous[uid] = s.view.modSeq[uid]
	}
	s.view.apply(snapshot)
	// A later flag query can fail or observe a newer commit. Only mark the
	// version actually written to the client; retain unreported changes.
	for uid, modSeq := range previous {
		s.view.note(uid, modSeq)
	}
	if len(d.flagChanged) > 0 {
		if err := s.writeFlagUpdatesOn(ctx, tx, w, d.flagChanged); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: commit: %w", errReconcileBackend, err)
	}
	s.viewFolderModSeq = stamp
	s.viewStampKnown = true
	return nil
}

// writeFlagUpdates emits FETCH FLAGS for the given UIDs using the view's
// current sequence numbers.
func (s *Session) writeFlagUpdates(ctx context.Context, w *imapserver.UpdateWriter, uids []int64) error {
	return s.writeFlagUpdatesOn(ctx, s.be.Pool, w, uids)
}

func (s *Session) writeFlagUpdatesOn(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, w *imapserver.UpdateWriter, uids []int64) error {
	rows, err := q.Query(ctx,
		`SELECT uid, flags, mod_seq FROM messages WHERE folder_id = $1 AND uid = ANY($2) ORDER BY uid`,
		s.selectedFolderID, uids)
	if err != nil {
		s.be.Logger.Warn("reconcile: flag update query", "err", err)
		return fmt.Errorf("%w: flag update query: %w", errReconcileBackend, err)
	}
	defer rows.Close()
	for rows.Next() {
		var uid, modSeq int64
		var flags []string
		if err := rows.Scan(&uid, &flags, &modSeq); err != nil {
			s.be.Logger.Warn("reconcile: flag update scan", "err", err)
			return fmt.Errorf("%w: flag update scan: %w", errReconcileBackend, err)
		}
		seq := s.view.seqOf(uid)
		if seq == 0 {
			continue
		}
		if err := w.WriteMessageFlags(seq, imap.UID(uid), toIMAPFlags(flags)); err != nil {
			return err
		}
		s.view.note(uid, modSeq)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: flag update rows: %w", errReconcileBackend, err)
	}
	return nil
}

// notifyFolderChanged publishes a mail_arrived NOTIFY for each folder this
// transaction changed, so IDLE sessions watching them wake immediately instead
// of waiting out the heartbeat (RA6X-001).
//
// Delivery has always done this; IMAP mutations did not, so an expunge or a
// flag change made by one client was invisible to another until its next
// command or the 29-minute re-poll. The channel name and payload are exactly
// what storage.Ingest uses, because the listener is the same one.
//
// The notification is a hint only. What a woken session does is reconcile
// against the database, which is the authority — a lost or duplicated NOTIFY
// changes when the client is told, never what it is told.
func notifyFolderChanged(ctx context.Context, tx pgx.Tx, logger interface {
	Warn(string, ...any)
}, folderIDs ...int64) {
	seen := make(map[int64]struct{}, len(folderIDs))
	for _, id := range folderIDs {
		if id == 0 {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if _, err := tx.Exec(ctx, `SELECT pg_notify('mail_arrived', $1)`,
			strconv.FormatInt(id, 10)); err != nil {
			// Never fail a committed mutation over a notification: the
			// heartbeat reconcile picks the change up regardless.
			logger.Warn("pg_notify failed", "err", err, "folder_id", id)
		}
	}
}

// ensureView guarantees the session has a UID view for the selected folder.
//
// Select always builds one and Unselect always clears one, so in the running
// server a session with selectedFolderID != 0 always has a view and this is a
// no-op. It exists for in-process callers that set the selected folder
// directly — the package's own integration tests do, to avoid re-deriving a
// whole SELECT for every fixture.
//
// Building it here is exactly what SELECT does: a view assembled from the
// current folder contents describes a client that has been told about all of
// them, which is true of a caller that has been told nothing yet.
func (s *Session) ensureView(ctx context.Context) error {
	if s.view != nil || s.selectedFolderID == 0 {
		return nil
	}
	snapshot, err := s.folderSnapshot(ctx, s.be.Pool, s.selectedFolderID)
	if err != nil {
		return err
	}
	s.view = newSessionView(snapshot)
	return nil
}
