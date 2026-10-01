package imapsess

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ptudor/epistula-mail/database/storage"
)

// Delete means archive (ARCHIVE_SORTING.md, "Delete archives"). This is what
// Gmail does: a client that marks a message \Deleted and expunges it has
// archived it. Only a move to Trash destroys mail. It is how a mail client's
// Delete key, trash button and swipe become Archive without remapping
// anything: in Mac Mail, turn off "Move deleted messages to the Trash
// mailbox", and Delete becomes mark-and-expunge.
//
// The rule, with Backend.DeleteArchives on and the mailbox having an \Archive
// folder:
//
//   - In \Trash, \Drafts and \Junk, EXPUNGE destroys as always: Trash is the
//     deliberate way to delete, a draft is a working copy the client replaces
//     as you type, and junk is junk.
//   - Anywhere else, the LAST copy of a message in the mailbox is not
//     destroyed. Outside the archive it moves to the \Archive folder, where the
//     sorter files it. Inside the archive (the \Archive folder or below) it
//     stays where it is, because it is already archived. Either way its
//     \Deleted mark is cleared.
//   - A message with another copy elsewhere in the mailbox is removed as
//     before. That is what a copy-then-delete move looks like, and the content
//     survives in the copy. This covers a client without MOVE, whose drag to
//     Trash is COPY, then \Deleted, then EXPUNGE.

// deleteArchiveTarget is where an EXPUNGE in the selected folder sends the
// last copy of a message.
type deleteArchiveTarget struct {
	root storage.SpecialUseFolder
	// inArchive is true when the selected folder is the \Archive folder or
	// below it: the message stays where it is.
	inArchive bool
}

// deleteArchiveTarget returns nil when EXPUNGE in the selected folder
// destroys as usual: the feature is off, the folder is \Trash, \Drafts or
// \Junk, or the mailbox has no \Archive folder to archive into.
func (s *Session) deleteArchiveTarget(ctx context.Context, tx pgx.Tx) (*deleteArchiveTarget, error) {
	if !s.be.DeleteArchives {
		return nil, nil
	}
	var name string
	var special *string
	if err := tx.QueryRow(ctx,
		`SELECT name, special_use FROM folders WHERE id = $1`, s.selectedFolderID,
	).Scan(&name, &special); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The folder was deleted under the session; the EXPUNGE that
			// follows finds nothing to do either.
			return nil, nil
		}
		return nil, err
	}
	if special != nil {
		switch *special {
		case `\Trash`, `\Drafts`, `\Junk`:
			return nil, nil
		}
	}
	root, err := storage.FindSpecialUseFolder(ctx, tx, s.mailboxID, `\Archive`)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &deleteArchiveTarget{
		root:      root,
		inArchive: name == root.Name || storage.IsUnderFolder(name, root.Name),
	}, nil
}

// archiveInsteadOfExpunge handles the last copies among the expunge
// candidates (uids in the selected folder) and returns the UIDs that left the
// folder by being archived, and the UIDs still to be destroyed.
//
// The candidates are locked here, and \Deleted is re-checked under the lock,
// so a `STORE -FLAGS (\Deleted)` that commits first still rescues its message
// (RO5X-001). One that commits after finds the row archived. That is not a
// loss: the message still exists.
func (s *Session) archiveInsteadOfExpunge(ctx context.Context, tx pgx.Tx, target *deleteArchiveTarget, uids []int64) (map[int64]struct{}, []int64, error) {
	rows, err := tx.Query(ctx, `
		SELECT m.id, m.uid
		  FROM messages m
		 WHERE m.folder_id = $1 AND m.uid = ANY($2) AND '\Deleted' = ANY(m.flags)
		   AND NOT EXISTS (
		       SELECT 1 FROM messages o JOIN folders f ON f.id = o.folder_id
		        WHERE f.mailbox_id = $3 AND o.raw_sha256 = m.raw_sha256 AND o.id <> m.id)
		 ORDER BY m.id
		   FOR UPDATE OF m`,
		s.selectedFolderID, uids, s.mailboxID)
	if err != nil {
		return nil, nil, fmt.Errorf("find last copies: %w", err)
	}
	uidOf := map[int64]int64{}
	var ids []int64
	for rows.Next() {
		var id, uid int64
		if err := rows.Scan(&id, &uid); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("scan last copy: %w", err)
		}
		uidOf[id] = uid
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("find last copies: %w", err)
	}

	archived := map[int64]struct{}{}
	spared := map[int64]struct{}{}
	for _, uid := range uidOf {
		spared[uid] = struct{}{}
	}
	if len(ids) > 0 {
		if target.inArchive {
			// Already archived: it stays, and loses the mark. The flag change
			// gets a fresh modseq so every session reports it.
			var modSeq int64
			if err := tx.QueryRow(ctx,
				`UPDATE folders SET highest_modseq = highest_modseq + 1 WHERE id = $1 RETURNING highest_modseq`,
				s.selectedFolderID,
			).Scan(&modSeq); err != nil {
				return nil, nil, fmt.Errorf("bump modseq: %w", err)
			}
			if _, err := tx.Exec(ctx,
				`UPDATE messages SET flags = array_remove(flags, '\Deleted'), mod_seq = $2 WHERE id = ANY($1)`,
				ids, modSeq,
			); err != nil {
				return nil, nil, fmt.Errorf("clear deleted flag: %w", err)
			}
		} else {
			items := make([]storage.MoveItem, len(ids))
			for i, id := range ids {
				items[i] = storage.MoveItem{ID: id, FromFolderID: s.selectedFolderID}
			}
			moved, err := storage.ArchiveMessages(ctx, tx, s.mailboxID, items, target.root.Name)
			if err != nil {
				return nil, nil, fmt.Errorf("archive: %w", err)
			}
			// The rows were locked above, so all of them moved.
			if len(moved) != len(ids) {
				return nil, nil, fmt.Errorf("archive: moved %d of %d locked messages", len(moved), len(ids))
			}
			for _, m := range moved {
				archived[uidOf[m.ID]] = struct{}{}
			}
		}
	}

	remaining := make([]int64, 0, len(uids))
	for _, uid := range uids {
		if _, ok := spared[uid]; !ok {
			remaining = append(remaining, uid)
		}
	}
	return archived, remaining, nil
}
