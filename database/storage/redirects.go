package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrFolderRedirectCycle is returned when following folder redirects comes
// back to a folder already visited, so no destination can be chosen.
var ErrFolderRedirectCycle = errors.New("folder redirects form a cycle")

// FolderRedirect is one folder_redirects row (migration 021): an
// `admin folder-merge` emptied From, and with Subtree every folder below it,
// into To as journal batch Batch.
type FolderRedirect struct {
	From    string
	Subtree bool
	To      string
	Batch   string
}

// RecordFolderRedirect records a completed merge, so a later import into a
// folder it emptied follows it (ResolveImportFolder). Recording the same
// source and batch again replaces the row.
func (db *DB) RecordFolderRedirect(ctx context.Context, mailboxID int64, r FolderRedirect) error {
	if r.From == r.To {
		return fmt.Errorf("folder redirect from %q to itself", r.From)
	}
	_, err := db.pool.Exec(ctx, `
		INSERT INTO folder_redirects (mailbox_id, from_folder, subtree, to_folder, batch)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (mailbox_id, from_folder, batch)
		DO UPDATE SET subtree = EXCLUDED.subtree, to_folder = EXCLUDED.to_folder`,
		mailboxID, r.From, r.Subtree, r.To, r.Batch)
	if err != nil {
		return fmt.Errorf("record folder redirect %q -> %q: %w", r.From, r.To, err)
	}
	return nil
}

// CountFolderRedirects reports how many redirects batch recorded.
func (db *DB) CountFolderRedirects(ctx context.Context, mailboxID int64, batch string) (int, error) {
	var n int
	if err := db.pool.QueryRow(ctx,
		`SELECT count(*) FROM folder_redirects WHERE mailbox_id = $1 AND batch = $2`,
		mailboxID, batch,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count folder redirects: %w", err)
	}
	return n, nil
}

// DeleteFolderRedirects removes the redirects batch recorded, when the merge
// it describes is undone, and reports how many there were.
func (db *DB) DeleteFolderRedirects(ctx context.Context, mailboxID int64, batch string) (int, error) {
	tag, err := db.pool.Exec(ctx,
		`DELETE FROM folder_redirects WHERE mailbox_id = $1 AND batch = $2`, mailboxID, batch)
	if err != nil {
		return 0, fmt.Errorf("delete folder redirects: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ResolveImportFolder returns the folder an import into name should write
// to, and whether that differs from name.
//
// A folder that exists is its own answer, whatever redirects mention it: an
// operator who re-creates a merged folder on purpose gets it as named. For a
// missing folder the redirect whose source is name, or a subtree redirect
// whose source is an ancestor of name, supplies the next candidate. The most
// specific source wins, then the latest merge. Candidates are followed until
// one exists or none redirects further, so a folder merged into one that was
// itself merged later resolves to the last destination; a missing final
// destination is returned for the import to create.
func (db *DB) ResolveImportFolder(ctx context.Context, mailboxID int64, name string) (string, bool, error) {
	seen := map[string]bool{}
	current := name
	for {
		if _, err := db.LookupFolder(ctx, mailboxID, current); err == nil {
			return current, current != name, nil
		} else if !errors.Is(err, ErrNotFound) {
			return "", false, fmt.Errorf("look up folder %q: %w", current, err)
		}
		seen[current] = true
		var next string
		err := db.pool.QueryRow(ctx, `
			SELECT to_folder FROM folder_redirects
			 WHERE mailbox_id = $1
			   AND (from_folder = $2 OR (subtree AND starts_with($2, from_folder || '/')))
			 ORDER BY length(from_folder) DESC, created_at DESC, batch DESC
			 LIMIT 1`, mailboxID, current,
		).Scan(&next)
		if errors.Is(err, pgx.ErrNoRows) {
			return current, current != name, nil
		}
		if err != nil {
			return "", false, fmt.Errorf("read folder redirects for %q: %w", current, err)
		}
		if seen[next] {
			return "", false, fmt.Errorf("%w: %q leads back to %q", ErrFolderRedirectCycle, name, next)
		}
		current = next
	}
}
