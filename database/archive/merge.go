package archive

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/ptudor/epistula-mail/database/storage"
)

// MergeOptions configures MergeFolders.
type MergeOptions struct {
	// From is the folder to empty; with Subtree, every folder below it too.
	From    string
	Subtree bool
	// To is the folder that receives the messages. It is created when
	// missing, and it (and anything below it) is never a source.
	To string
	// DropDuplicates destroys a source message whose exact content is
	// already in To instead of skipping it, so the merged folder holds each
	// message once. The content itself survives in To.
	DropDuplicates bool
	DryRun         bool
	// Batch names the journal entries, so archive-undo can reverse the
	// merge. Required unless DryRun.
	Batch     string
	BatchSize int
}

// MergeSource is what a merge does to one source folder.
type MergeSource struct {
	Folder     string
	Moved      int
	Duplicates int // already in To: skipped, or dropped with DropDuplicates
}

// MergeStats reports a merge.
type MergeStats struct {
	Sources []MergeSource
	Moved   int
	Dropped int
	Skipped int
}

// MergeFolders moves every message of one folder, or of a folder and its
// subtree, into another folder of the same mailbox, journaled like a
// reorganization batch. It is how an import's scattered copies of one
// folder's role ("Sent", "Sent/2005", "Sent/pre-2008") are combined into the
// folder clients use (\Sent). Afterwards `admin folder-prune-empty` removes
// the emptied sources.
func MergeFolders(ctx context.Context, db *storage.DB, mailboxID int64, opts MergeOptions) (MergeStats, error) {
	if err := storage.ValidateFolderName(opts.From); err != nil {
		return MergeStats{}, fmt.Errorf("from: %w", err)
	}
	if err := storage.ValidateFolderName(opts.To); err != nil {
		return MergeStats{}, fmt.Errorf("to: %w", err)
	}
	if opts.From == opts.To {
		return MergeStats{}, errors.New("archive: merge source and destination are the same folder")
	}
	if !opts.DryRun && opts.Batch == "" {
		return MergeStats{}, errors.New("archive: merge needs a batch name")
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 500
	}

	type source struct {
		id   int64
		name string
	}
	var sources []source
	rows, err := db.Pool().Query(ctx,
		`SELECT id, name FROM folders WHERE mailbox_id = $1 ORDER BY name`, mailboxID)
	if err != nil {
		return MergeStats{}, fmt.Errorf("list folders: %w", err)
	}
	for rows.Next() {
		var s source
		if err := rows.Scan(&s.id, &s.name); err != nil {
			rows.Close()
			return MergeStats{}, fmt.Errorf("scan folder: %w", err)
		}
		inFrom := s.name == opts.From || (opts.Subtree && storage.IsUnderFolder(s.name, opts.From))
		inTo := s.name == opts.To || storage.IsUnderFolder(s.name, opts.To)
		if inFrom && !inTo {
			sources = append(sources, s)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return MergeStats{}, fmt.Errorf("list folders: %w", err)
	}
	found := false
	for _, s := range sources {
		found = found || s.name == opts.From
	}
	// With Subtree the folder itself may be missing (an import that predates
	// OPS-001 left only its children); without, it is the whole source.
	if len(sources) == 0 || (!found && !opts.Subtree) {
		return MergeStats{}, fmt.Errorf("%w: folder %q", storage.ErrNotFound, opts.From)
	}

	// Content already in the destination, so a duplicate is recognised, and
	// grown as messages move so two sources holding the same message do not
	// both land in it.
	inDest := map[string]bool{}
	rows, err = db.Pool().Query(ctx, `
		SELECT encode(m.raw_sha256, 'hex') FROM messages m JOIN folders f ON f.id = m.folder_id
		 WHERE f.mailbox_id = $1 AND f.name = $2`, mailboxID, opts.To)
	if err != nil {
		return MergeStats{}, fmt.Errorf("read destination: %w", err)
	}
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			rows.Close()
			return MergeStats{}, fmt.Errorf("scan destination: %w", err)
		}
		inDest[sha] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return MergeStats{}, fmt.Errorf("read destination: %w", err)
	}

	var gin *ginFlusher
	if !opts.DryRun {
		if gin, err = newGINFlusher(ctx, db); err != nil {
			return MergeStats{}, err
		}
	}
	var stats MergeStats
	for _, src := range sources {
		var moves []Move
		var dups []int64
		rows, err := db.Pool().Query(ctx,
			`SELECT id, encode(raw_sha256, 'hex') FROM messages WHERE folder_id = $1 ORDER BY internal_date, id`, src.id)
		if err != nil {
			return stats, fmt.Errorf("read %q: %w", src.name, err)
		}
		for rows.Next() {
			var id int64
			var sha string
			if err := rows.Scan(&id, &sha); err != nil {
				rows.Close()
				return stats, fmt.Errorf("scan %q: %w", src.name, err)
			}
			if inDest[sha] {
				dups = append(dups, id)
				continue
			}
			inDest[sha] = true
			moves = append(moves, Move{MessageID: id, FromFolderID: src.id, FromFolder: src.name, ToFolder: opts.To})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return stats, fmt.Errorf("read %q: %w", src.name, err)
		}
		entry := MergeSource{Folder: src.name, Moved: len(moves), Duplicates: len(dups)}

		if !opts.DryRun {
			entry.Moved = 0
			for start := 0; start < len(moves); start += opts.BatchSize {
				chunk := moves[start:min(start+opts.BatchSize, len(moves))]
				n, err := moveAndJournal(ctx, db, mailboxID, chunk, opts.Batch, "reorg", mergeDestination)
				if err != nil {
					return stats, fmt.Errorf("move from %q: %w", src.name, err)
				}
				entry.Moved += n
				if err := gin.flush(ctx); err != nil {
					return stats, err
				}
			}
			if opts.DropDuplicates && len(dups) > 0 {
				sort.Slice(dups, func(i, j int) bool { return dups[i] < dups[j] })
				for start := 0; start < len(dups); start += opts.BatchSize {
					chunk := dups[start:min(start+opts.BatchSize, len(dups))]
					var res storage.PurgeResult
					if err := db.RunTx(ctx, func(tx pgx.Tx) error {
						var err error
						res, err = storage.PurgeMessages(ctx, tx, mailboxID, chunk, false)
						return err
					}); err != nil {
						return stats, fmt.Errorf("drop duplicates from %q: %w", src.name, err)
					}
					stats.Dropped += int(res.Messages)
				}
			}
		}
		if !opts.DropDuplicates {
			stats.Skipped += len(dups)
		}
		stats.Moved += entry.Moved
		stats.Sources = append(stats.Sources, entry)
	}
	// Recorded once every source is empty, so a later import into a folder
	// this merge emptied (and prune removed) lands in To rather than
	// re-creating the source and storing its messages a second time. A
	// source whose messages were all dropped as duplicates is covered too.
	if !opts.DryRun {
		if err := db.RecordFolderRedirect(ctx, mailboxID, storage.FolderRedirect{
			From: opts.From, Subtree: opts.Subtree, To: opts.To, Batch: opts.Batch,
		}); err != nil {
			return stats, err
		}
	}
	return stats, nil
}
