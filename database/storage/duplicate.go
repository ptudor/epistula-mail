package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ptudor/epistula-mail/database/blob"
)

// StoredBlob is a persisted reference, including its original date and size.
type StoredBlob struct {
	Kind   blob.Kind
	Bucket blob.Bucket
	SHA    string
	Size   int64
}

// RepairDuplicate pins a duplicate and every blob it needs while the caller
// verifies or restores content. It never changes message identity or metadata.
// Canonical order: mailbox, sorted GC locks, folder, message. Other writers
// also take the mailbox lock, so references cannot change during discovery.
func (db *DB) RepairDuplicate(ctx context.Context, mailboxID int64, mailboxName, folderName, sha string, ensure func([]StoredBlob) error, committed func()) (bool, error) {
	id, err := db.repairDuplicate(ctx, mailboxID, mailboxName, folderName, 0, sha, ensure, committed)
	return id != 0, err
}

// RepairDuplicateInFolder preserves an import job's durable folder identity.
// A zero folderID resolves the current name after a concurrent first ingest.
// The returned ID is zero if the duplicate no longer exists.
func (db *DB) RepairDuplicateInFolder(ctx context.Context, mailboxID int64, mailboxName, folderName string, folderID int64, sha string, ensure func([]StoredBlob) error) (int64, error) {
	return db.repairDuplicate(ctx, mailboxID, mailboxName, folderName, folderID, sha, ensure, nil)
}

func (db *DB) repairDuplicate(ctx context.Context, mailboxID int64, mailboxName, folderName string, expectedFolderID int64, sha string, ensure func([]StoredBlob) error, committed func()) (int64, error) {
	var duplicateID int64
	err := db.RunTxCommitted(ctx, func(tx pgx.Tx) error {
		var current string
		var maintenance *time.Time
		if err := tx.QueryRow(ctx, `SELECT name,maintenance_at FROM mailboxes WHERE id=$1 FOR UPDATE`, mailboxID).Scan(&current, &maintenance); err != nil {
			return err
		}
		if maintenance != nil {
			return ErrMailboxInMaintenance
		}
		if current != mailboxName {
			return ErrMailboxIdentityChanged
		}
		var folderID, messageID int64
		var date time.Time
		raw := StoredBlob{Kind: blob.KindRaw, SHA: sha}
		err := tx.QueryRow(ctx, `SELECT f.id,m.id,m.raw_blob_date,m.raw_size FROM folders f JOIN messages m ON m.folder_id=f.id WHERE f.mailbox_id=$1 AND (($4::bigint=0 AND f.name=$2) OR ($4::bigint<>0 AND f.id=$4)) AND m.raw_sha256=decode($3,'hex') ORDER BY m.id LIMIT 1`, mailboxID, folderName, sha, expectedFolderID).Scan(&folderID, &messageID, &date, &raw.Size)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		raw.Bucket = blob.BucketFromTime(date)
		refs := []StoredBlob{raw}
		rows, err := tx.Query(ctx, `SELECT encode(sha256,'hex'),blob_date,size_bytes FROM attachments WHERE message_id=$1 ORDER BY id`, messageID)
		if err != nil {
			return err
		}
		for rows.Next() {
			r := StoredBlob{Kind: blob.KindAttachment}
			if err := rows.Scan(&r.SHA, &date, &r.Size); err != nil {
				rows.Close()
				return err
			}
			r.Bucket = blob.BucketFromTime(date)
			refs = append(refs, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		keys := make([]int64, len(refs))
		for i, r := range refs {
			keys[i] = BlobAdvisoryLockKey(mailboxName, string(r.Kind), string(r.Bucket), r.SHA)
		}
		if err := acquireBlobLocks(ctx, tx, keys); err != nil {
			return err
		}
		var pinned int64
		if err := tx.QueryRow(ctx, `SELECT id FROM folders WHERE id=$1 FOR UPDATE`, folderID).Scan(&pinned); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT id FROM messages WHERE id=$1 FOR UPDATE`, messageID).Scan(&pinned); err != nil {
			return err
		}
		if ensure == nil {
			return fmt.Errorf("duplicate verification callback required")
		}
		if err := ensure(refs); err != nil {
			return err
		}
		for _, r := range refs {
			if _, err := tx.Exec(ctx, `DELETE FROM gc_candidates WHERE tenant=$1 AND kind=$2 AND bucket=$3 AND sha256=decode($4,'hex')`, mailboxName, string(r.Kind), string(r.Bucket), r.SHA); err != nil {
				return err
			}
		}
		duplicateID = folderID
		return nil
	}, func() {
		if duplicateID != 0 && committed != nil {
			committed()
		}
	})
	if err != nil {
		return 0, err
	}
	return duplicateID, nil
}
