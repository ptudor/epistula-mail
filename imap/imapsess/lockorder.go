package imapsess

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The canonical lock order for every writer in the mail store (RA6X-021).
//
// Each transaction here was individually atomic, but they disagreed about the
// order in which they took locks, so ordinary simultaneous mail-client activity
// could form a cycle. A concrete one: STORE bumps the
// folder's modseq and then updates message rows, while EXPUNGE deleted message
// rows and only afterwards updated the mailbox counter and the folder — so
// STORE could hold a folder waiting for a message that EXPUNGE held, while
// EXPUNGE waited for that folder. Delivery and folder DELETE formed a second
// pair over (folder allocation, mailbox accounting).
//
// The order is:
//
//	1. the mailbox row          (mailboxes, FOR UPDATE)
//	2. blob advisory locks      (pg_advisory_xact_lock, ascending, deduped)
//	3. folder rows              (folders, FOR UPDATE, ascending id)
//	4. message rows             (messages, FOR UPDATE, ascending id)
//
// Rules for adding a writer:
//
//   - Take only the levels you need, but always in this order. Skipping a level
//     is fine; taking one out of order is not.
//   - Level 3 and 4 take MULTIPLE rows, so they must be sorted. Two MOVEs in
//     opposite directions between the same pair of folders are safe only
//     because both sort the folder ids ascending.
//   - Level 1 is what storage.Ingest holds for the whole of every delivery,
//     APPEND and import, so anything that also touches mailbox accounting takes
//     it FIRST, not at the end next to the `UPDATE mailboxes SET used_bytes`
//     it happens to need.
//   - GC sweep takes a level-2 lock and no row locks, and the account-lifecycle
//     commands (`admin mailbox-maintenance`, `mailbox-rename`,
//     `mailbox-delete`) take level 1 and nothing below it. Neither can be part
//     of a cycle with the order above.
//
// Deadlock is not fully eliminated by ordering alone — PostgreSQL can still
// abort a transaction with a serialization failure — so writers whose work is
// safely replayable run under retryTx.

// lockMailbox takes level 1: the mailbox row, exclusively, for the rest of the
// transaction. Every writer that will touch `mailboxes.used_bytes`, or that
// must not interleave with an account-lifecycle operation, calls this first.
func lockMailbox(ctx context.Context, tx pgx.Tx, mailboxID int64) error {
	var id int64
	return tx.QueryRow(ctx,
		`SELECT id FROM mailboxes WHERE id = $1 FOR UPDATE`, mailboxID,
	).Scan(&id)
}

// lockFolders takes level 3: the named folder rows, exclusively, in ascending
// id order. Duplicate ids are collapsed. An id that no longer exists is not an
// error here — the caller decides what a vanished folder means, and gets the
// surviving set back so it can tell.
func lockFolders(ctx context.Context, tx pgx.Tx, folderIDs ...int64) ([]int64, error) {
	ids := sortedUnique(folderIDs)
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx,
		`SELECT id FROM folders WHERE id = ANY($1) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		got = append(got, id)
	}
	return got, rows.Err()
}

// lockMessagesByUID takes level 4: the message rows for the given UIDs within
// one folder, exclusively, in ascending id order. Returns the (uid, id,
// raw_size) of the rows that were actually there, which is the authoritative
// set for anything derived from them — a caller that computed a byte total from
// an earlier unlocked read is computing it from a snapshot that may already be
// wrong (RA6X-002, RA6X-020).
func lockMessagesByUID(ctx context.Context, tx pgx.Tx, folderID int64, uids []int64) ([]lockedMessage, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT id, uid, raw_size
		  FROM messages
		 WHERE folder_id = $1 AND uid = ANY($2)
		 ORDER BY id
		   FOR UPDATE`,
		folderID, sortedUnique(uids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []lockedMessage
	for rows.Next() {
		var m lockedMessage
		if err := rows.Scan(&m.id, &m.uid, &m.rawSize); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// lockedMessage is one message row held under a level-4 lock.
type lockedMessage struct {
	id      int64
	uid     int64
	rawSize int64
}

// lockAllMessagesInFolder is lockMessagesByUID for a whole folder, used where
// the operation's scope is the folder itself rather than a UID set.
func lockAllMessagesInFolder(ctx context.Context, tx pgx.Tx, folderID int64) ([]lockedMessage, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, uid, raw_size
		  FROM messages
		 WHERE folder_id = $1
		 ORDER BY id
		   FOR UPDATE`,
		folderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []lockedMessage
	for rows.Next() {
		var m lockedMessage
		if err := rows.Scan(&m.id, &m.uid, &m.rawSize); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func sortedUnique(in []int64) []int64 {
	if len(in) == 0 {
		return nil
	}
	cp := append([]int64(nil), in...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	out := cp[:1]
	for _, v := range cp[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

// maxTxAttempts bounds retryTx. Two retries is plenty: a deadlock victim is
// aborted precisely so the other transaction can finish, so the retry finds an
// uncontended path almost immediately. More attempts would just extend how long
// a genuinely hot row keeps a client waiting.
const maxTxAttempts = 3

// retryTx runs body in a transaction, retrying the WHOLE thing on a PostgreSQL
// serialization failure or deadlock (SQLSTATE class 40).
//
// A consistent lock order makes cycles far rarer but does not make them
// impossible: PostgreSQL may still choose a transaction as a deadlock victim,
// and an index-level or foreign-key ordering nobody wrote down can produce one.
// Aborting a user's STORE with "backend error" for a condition the server
// resolves by trying again is the wrong answer.
//
// Retrying is only sound because of how these commands are shaped: every one of
// them computes its protocol response inside the transaction and writes it
// AFTER commit. Nothing has been told to the client when a retry begins, so a
// replay cannot duplicate an EXPUNGE, a COPYUID or a FETCH FLAGS update. body
// must therefore not write to the wire, and must recompute rather than reuse
// anything it read on a previous attempt — it is called fresh each time.
func retryTx(ctx context.Context, begin func(context.Context) (pgx.Tx, error), body func(pgx.Tx) error) error {
	var lastErr error
	for attempt := 1; attempt <= maxTxAttempts; attempt++ {
		tx, err := begin(ctx)
		if err != nil {
			return err
		}
		err = body(tx)
		if err != nil {
			_ = tx.Rollback(ctx)
			if isRetryableTxError(err) && attempt < maxTxAttempts && ctx.Err() == nil {
				lastErr = err
				// A brief, growing pause so two transactions that just
				// deadlocked do not immediately re-collide.
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Duration(attempt) * 5 * time.Millisecond):
				}
				continue
			}
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			_ = tx.Rollback(ctx)
			if isRetryableTxError(err) && attempt < maxTxAttempts && ctx.Err() == nil {
				lastErr = err
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Duration(attempt) * 5 * time.Millisecond):
				}
				continue
			}
			return err
		}
		return nil
	}
	return lastErr
}

// isRetryableTxError reports whether err is a PostgreSQL transaction rollback
// the server invites us to retry: 40001 serialization_failure and 40P01
// deadlock_detected.
//
// Deliberately narrow. A constraint violation, a permission error or a syntax
// error would fail identically on replay, and retrying them would turn one
// clear failure into three.
func isRetryableTxError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return len(pgErr.Code) >= 2 && pgErr.Code[:2] == "40"
}

// txAborted renders a retry-exhausted transaction as a protocol error the
// client can act on: try again, rather than "the server is broken".
func txAborted(command string) error {
	return &imap.Error{
		Type: imap.StatusResponseTypeNo,
		Code: imap.ResponseCodeInUse,
		Text: command + ": mailbox busy, please retry",
	}
}
