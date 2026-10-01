package storage

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned by lookup helpers when no row matches.
var ErrNotFound = errors.New("storage: not found")

// ErrOverQuota is returned by Ingest when accepting the message would push the
// mailbox's used_bytes past its quota_bytes. The transaction is rolled back so
// the rejected message is not counted. deliver maps it to EX_CANTCREAT and
// IMAP APPEND to NO [OVERQUOTA]. A NULL quota_bytes means unlimited (R-029).
var ErrOverQuota = errors.New("storage: mailbox over quota")

// ErrMailboxInMaintenance is returned by Ingest when the target mailbox has a
// non-NULL maintenance_at: an operator is moving its blob tenant tree and the
// database and the filesystem currently disagree about where this account's
// blobs live (RA6X-013). A write accepted during that window can land its blob
// under one name and its row under another, which is acknowledged, permanently
// unreadable mail.
//
// It is a TRANSIENT condition by construction — the barrier is lifted when the
// operator finishes — so the LDA must defer, not bounce.
var ErrMailboxInMaintenance = errors.New("storage: mailbox is quiesced for maintenance")

// ErrFolderGone is returned by Ingest when MustExistFolderID names a folder
// that no longer exists. IMAP APPEND turns it into NO [TRYCREATE]: the folder
// the client named has been deleted, and recreating it would let a concurrent
// DELETE be silently undone by an APPEND (RA6X-047).
var ErrFolderGone = errors.New("storage: destination folder no longer exists")

// ErrMailboxIdentityChanged is returned by Ingest when the caller's
// MailboxName no longer matches the mailbox row it names by ID.
//
// The mailbox name IS the on-disk blob tenant path, and long-lived callers
// cache it: an authenticated IMAP session resolves it once at LOGIN and keeps
// it until disconnect. Without this check such a session could write a blob
// under the pre-rename tenant while inserting the row into the renamed
// mailbox, and the message would be acknowledged and unreadable forever
// (RA6X-013). Revalidating inside the transaction, under a row lock the rename
// must wait for, makes that impossible: either the write commits before the
// rename, or it fails.
//
// The caller should re-resolve the mailbox and retry.
var ErrMailboxIdentityChanged = errors.New("storage: mailbox name changed since it was resolved")

// ErrDuplicate is returned by Ingest when IngestParams.DedupOnRawSHA is set and
// a message with the same raw_sha256 already exists in the target folder. It is
// raised inside the transaction AFTER the per-blob advisory locks are held, so
// two concurrent imports of the same message serialize and the loser sees the
// winner's committed row instead of inserting a duplicate. The import paths map
// it to a "duplicate" outcome; deliver and IMAP APPEND leave DedupOnRawSHA
// false (identical bytes delivered twice are legitimately two messages) (R-044).
var ErrDuplicate = errors.New("storage: message already present in folder")

// LookupMailboxByName returns the mailbox id. ErrNotFound if missing.
func (db *DB) LookupMailboxByName(ctx context.Context, name string) (int64, error) {
	var id int64
	err := db.pool.QueryRow(ctx,
		`SELECT id FROM mailboxes WHERE name = $1`, name,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// LookupOrCreateFolder returns the folder id, creating the row — and any
// missing hierarchical ancestors — if absent (OPS-001). It takes the mailbox
// row lock EnsureFolder requires; a mailbox that no longer exists is
// ErrNotFound.
func (db *DB) LookupOrCreateFolder(ctx context.Context, mailboxID int64, name string) (int64, error) {
	var id int64
	err := db.runTx(ctx, func(tx pgx.Tx) error {
		if err := lockMailboxRow(ctx, tx, mailboxID); err != nil {
			return err
		}
		folder, _, err := EnsureFolder(ctx, tx, mailboxID, name, nil)
		if err != nil {
			return err
		}
		id = folder.ID
		return nil
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// LookupFolder returns the folder id for (mailboxID, name) WITHOUT creating it.
// Returns ErrNotFound if the folder does not exist. Read-only/dry-run paths
// (import -dry-run, import-verify) use this so inspecting a mailbox never adds a
// folder row that would change IMAP LIST for real users (R-023).
func (db *DB) LookupFolder(ctx context.Context, mailboxID int64, name string) (int64, error) {
	var id int64
	err := db.pool.QueryRow(ctx,
		`SELECT id FROM folders WHERE mailbox_id = $1 AND name = $2`,
		mailboxID, name,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// MessageExists reports whether a message with the given content hash is
// already present in the folder. Importer uses this for idempotent dedup.
func (db *DB) MessageExists(ctx context.Context, folderID int64, sha256Hex string) (bool, error) {
	sha, err := hex.DecodeString(sha256Hex)
	if err != nil {
		return false, fmt.Errorf("invalid sha256hex: %w", err)
	}
	var exists bool
	if err := db.pool.QueryRow(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM messages WHERE folder_id = $1 AND raw_sha256 = $2
		)`, folderID, sha,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("dedup check: %w", err)
	}
	return exists, nil
}

// ErrProtocolIDExhausted reports an allocated UID or UIDVALIDITY that does not
// fit the 32-bit field IMAP carries it in (RA6X-052).
//
// The columns are BIGINT and folder_uidvalidity_seq is unrestricted, so
// nothing in the schema stops a counter passing 2^32-1. An unchecked
// conversion emits zero — which RFC 9051 forbids for UIDVALIDITY — or reuses
// an identifier a client has cached against, which is silently worse: the
// client believes its cache is still valid and shows the wrong messages.
//
// Refused at ALLOCATION, before commit, so the failed operation leaves the
// message, the quota and the counters untouched. Recovery is an operational
// decision (a new mailbox, or a migration that renumbers with fresh
// UIDVALIDITY), never a sequence reset to a convenient lower value, which
// would reuse the same (mailbox, UIDVALIDITY, UID) tuples.
var ErrProtocolIDExhausted = errors.New("storage: identifier exceeds the 32-bit IMAP protocol range")

// CheckProtocolID rejects a value that cannot be carried in an IMAP 32-bit
// identifier field. Zero is rejected too: the protocol reserves it.
func CheckProtocolID(name string, v int64) error {
	if v <= 0 || v > math.MaxUint32 {
		return fmt.Errorf("%w: %s = %d", ErrProtocolIDExhausted, name, v)
	}
	return nil
}

// CheckProtocolIDForTest exposes CheckProtocolID so the package's consumers can
// pin the 32-bit boundary without duplicating the rule (RA6X-052).
func CheckProtocolIDForTest(name string, v int64) error { return CheckProtocolID(name, v) }
