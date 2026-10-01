package archive

import (
	"context"
	"errors"
	"fmt"

	"github.com/ptudor/epistula-mail/database/storage"
)

// ReclassifyOptions configures Reclassify.
type ReclassifyOptions struct {
	// Key is the category whose classifications are cleared.
	Key string
	// Refile also moves the messages the sorter or a reorganization filed
	// under Key back to the \Archive folder, so they are filed again by
	// their new classification. Messages the owner filed by hand, and filed
	// messages the owner has moved since, are left where they are.
	Refile bool
	DryRun bool
	// Batch names the refile's journal entries (archive-undo reverses them).
	Batch     string
	BatchSize int
}

// ReclassifyStats reports a Reclassify (or, dry, what it would do).
type ReclassifyStats struct {
	Cleared int
	Refiled int
	// MovedSince counts messages filed under Key that the owner has moved
	// since; -refile leaves them alone.
	MovedSince int
}

// Reclassify clears one category's classifications in a mailbox, so the
// worker's classification pass classifies those messages again against the
// current list. That is what a new sibling key needs: the messages that went
// to "…/other" because nothing fitted get another look now that something
// does.
//
// The classifications are cleared first and the refile runs after. The
// sorter files only classified messages, so it cannot re-file a refiled
// message under its old classification in between.
func Reclassify(ctx context.Context, db *storage.DB, mailboxID int64, opts ReclassifyOptions) (ReclassifyStats, error) {
	if !storage.ValidArchiveKey(opts.Key) {
		return ReclassifyStats{}, fmt.Errorf("%w: key %q", storage.ErrInvalidArchiveCategory, opts.Key)
	}
	if !opts.DryRun && opts.Refile && opts.Batch == "" {
		return ReclassifyStats{}, errors.New("archive: refile needs a batch name")
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 500
	}
	var stats ReclassifyStats

	var root storage.SpecialUseFolder
	var refile []Move
	if opts.Refile {
		var err error
		root, err = storage.FindSpecialUseFolder(ctx, db.Pool(), mailboxID, `\Archive`)
		if errors.Is(err, storage.ErrNotFound) {
			return stats, storage.ErrNoArchiveFolder
		}
		if err != nil {
			return stats, err
		}
		// Each message's latest filing: refiled only if that filing was
		// under Key and the message is still where it was filed.
		rows, err := db.Pool().Query(ctx, `
			SELECT DISTINCT ON (j.message_id)
			       j.message_id, COALESCE(j.category, ''), j.to_folder, m.folder_id, f.name
			  FROM archive_moves j
			  JOIN messages m ON m.id = j.message_id
			  JOIN folders f ON f.id = m.folder_id
			 WHERE j.mailbox_id = $1 AND j.undone_at IS NULL
			 ORDER BY j.message_id, j.id DESC`, mailboxID)
		if err != nil {
			return stats, fmt.Errorf("read journal: %w", err)
		}
		for rows.Next() {
			var id, folderID int64
			var category, toFolder, current string
			if err := rows.Scan(&id, &category, &toFolder, &folderID, &current); err != nil {
				rows.Close()
				return stats, fmt.Errorf("scan journal: %w", err)
			}
			if category != opts.Key {
				continue
			}
			if current != toFolder || folderID == root.ID {
				stats.MovedSince++
				continue
			}
			refile = append(refile, Move{MessageID: id, FromFolderID: folderID, FromFolder: current, ToFolder: root.Name})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return stats, fmt.Errorf("read journal: %w", err)
		}
	}

	if opts.DryRun {
		if err := db.Pool().QueryRow(ctx, `
			SELECT count(*) FROM message_classifications c
			  JOIN messages m ON m.id = c.message_id
			  JOIN folders f ON f.id = m.folder_id
			 WHERE f.mailbox_id = $1 AND c.category = $2`, mailboxID, opts.Key,
		).Scan(&stats.Cleared); err != nil {
			return stats, fmt.Errorf("count classifications: %w", err)
		}
		stats.Refiled = len(refile)
		return stats, nil
	}

	tag, err := db.Pool().Exec(ctx, `
		DELETE FROM message_classifications c
		 USING messages m, folders f
		 WHERE m.id = c.message_id AND f.id = m.folder_id
		   AND f.mailbox_id = $1 AND c.category = $2`, mailboxID, opts.Key)
	if err != nil {
		return stats, fmt.Errorf("clear classifications: %w", err)
	}
	stats.Cleared = int(tag.RowsAffected())

	if len(refile) > 0 {
		gin, err := newGINFlusher(ctx, db)
		if err != nil {
			return stats, err
		}
		for start := 0; start < len(refile); start += opts.BatchSize {
			chunk := refile[start:min(start+opts.BatchSize, len(refile))]
			n, err := moveAndJournal(ctx, db, mailboxID, chunk, opts.Batch, "reorg", filingDestination)
			if err != nil {
				return stats, fmt.Errorf("refile to %q: %w", root.Name, err)
			}
			stats.Refiled += n
			stats.MovedSince += len(chunk) - n
			if err := gin.flush(ctx); err != nil {
				return stats, err
			}
		}
	}
	return stats, nil
}
