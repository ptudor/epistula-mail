package archive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ptudor/epistula-mail/database/storage"
)

// maxBatchesPerMailbox bounds one SortOnce or PurgeOnce pass per mailbox, so a
// large backlog is worked off over several ticks instead of one long pass that
// delays shutdown.
const maxBatchesPerMailbox = 20

// SortOptions configures the live sorter.
type SortOptions struct {
	// SettleDelay leaves a message alone until it has been in the \Archive
	// folder this long, so a mail client's Undo of an archive — a MOVE back
	// by UID — still finds it there.
	SettleDelay time.Duration
	// MinConfidence is the lowest classifier confidence that files a message.
	// Anything less stays in the \Archive folder, which is the unsorted pile.
	MinConfidence float64
	// BatchSize is how many messages one transaction files.
	BatchSize int
	Logger    *slog.Logger
}

// SortStats reports one SortOnce pass.
type SortStats struct {
	Mailboxes int
	Filed     int
}

// SortOnce files what users have archived. For every mailbox with an \Archive
// folder and at least one active category, each message in that folder that
// has settled, is classified into an active category at or above
// MinConfidence, and is not marked \Deleted, moves to its category's folder.
// Everything else stays in the \Archive folder until a classification arrives.
//
// The sorter never destroys mail and never files outside the \Archive
// folder's own subtree: a category whose folder is not strictly inside it, or
// is a special-use folder, is refused and logged, whatever the table says.
func SortOnce(ctx context.Context, db *storage.DB, opts SortOptions) (SortStats, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 500
	}
	batch := "sort-" + time.Now().UTC().Format("2006-01-02")

	type target struct {
		mailboxID int64
		root      storage.SpecialUseFolder
	}
	rows, err := db.Pool().Query(ctx, `
		SELECT DISTINCT ON (f.mailbox_id) f.mailbox_id, f.id, f.name
		  FROM folders f
		  JOIN mailboxes mb ON mb.id = f.mailbox_id
		 WHERE f.special_use = '\Archive'
		   AND mb.maintenance_at IS NULL
		   AND EXISTS (SELECT 1 FROM archive_categories c
		                WHERE c.mailbox_id = f.mailbox_id AND c.retired_at IS NULL)
		 ORDER BY f.mailbox_id, f.id`)
	if err != nil {
		return SortStats{}, fmt.Errorf("find archive folders: %w", err)
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.mailboxID, &t.root.ID, &t.root.Name); err != nil {
			rows.Close()
			return SortStats{}, fmt.Errorf("scan archive folder: %w", err)
		}
		targets = append(targets, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return SortStats{}, fmt.Errorf("find archive folders: %w", err)
	}

	var stats SortStats
	var firstErr error
	for _, t := range targets {
		stats.Mailboxes++
		n, err := sortMailbox(ctx, db, t.mailboxID, t.root, opts, batch, logger)
		stats.Filed += n
		if err != nil {
			if ctx.Err() != nil {
				return stats, ctx.Err()
			}
			// One mailbox's failure must not starve the others.
			logger.Error("archive sort", "mailbox_id", t.mailboxID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return stats, firstErr
}

// validCategoryKeys returns the mailbox's active category keys whose folder
// the sorter may file into, logging each one it refuses.
func validCategoryKeys(ctx context.Context, db *storage.DB, mailboxID int64, root string, logger *slog.Logger) ([]string, error) {
	rows, err := db.Pool().Query(ctx, `
		SELECT c.key, c.folder, COALESCE(f.special_use, '')
		  FROM archive_categories c
		  LEFT JOIN folders f ON f.mailbox_id = c.mailbox_id AND f.name = c.folder
		 WHERE c.mailbox_id = $1 AND c.retired_at IS NULL`, mailboxID)
	if err != nil {
		return nil, fmt.Errorf("read categories: %w", err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key, folder, special string
		if err := rows.Scan(&key, &folder, &special); err != nil {
			return nil, fmt.Errorf("scan category: %w", err)
		}
		switch {
		case !storage.IsUnderFolder(folder, root):
			logger.Warn("archive category folder is outside the \\Archive folder; not filing into it",
				"mailbox_id", mailboxID, "key", key, "folder", folder, "archive_folder", root)
		case special != "":
			logger.Warn("archive category folder is a special-use folder; not filing into it",
				"mailbox_id", mailboxID, "key", key, "folder", folder, "special_use", special)
		case storage.ValidateFolderName(folder) != nil:
			logger.Warn("archive category folder name is invalid; not filing into it",
				"mailbox_id", mailboxID, "key", key)
		default:
			keys = append(keys, key)
		}
	}
	return keys, rows.Err()
}

func sortMailbox(ctx context.Context, db *storage.DB, mailboxID int64, root storage.SpecialUseFolder, opts SortOptions, batch string, logger *slog.Logger) (int, error) {
	keys, err := validCategoryKeys(ctx, db, mailboxID, root.Name, logger)
	if err != nil || len(keys) == 0 {
		return 0, err
	}
	filed := 0
	for i := 0; i < maxBatchesPerMailbox; i++ {
		type candidate struct {
			id         int64
			category   string
			confidence float32
			folder     string
			annual     bool
			sentLocal  *time.Time
			internal   time.Time
		}
		now := time.Now()
		rows, err := db.Pool().Query(ctx, `
			SELECT m.id, c.category, c.confidence, ac.folder, ac.annual, m.sent_date_local, m.internal_date
			  FROM messages m
			  JOIN message_classifications c ON c.message_id = m.id
			  JOIN archive_categories ac
			    ON ac.mailbox_id = $1 AND ac.key = c.category AND ac.retired_at IS NULL
			 WHERE m.folder_id = $2
			   AND m.created_at <= now() - make_interval(secs => $3)
			   AND c.confidence >= $4
			   AND c.category = ANY($5)
			   AND NOT ('\Deleted' = ANY(m.flags))
			 ORDER BY m.id
			 LIMIT $6`,
			mailboxID, root.ID, opts.SettleDelay.Seconds(), opts.MinConfidence, keys, opts.BatchSize)
		if err != nil {
			return filed, fmt.Errorf("find archived messages: %w", err)
		}
		byFolder := map[string][]Move{}
		found := 0
		for rows.Next() {
			var c candidate
			if err := rows.Scan(&c.id, &c.category, &c.confidence, &c.folder, &c.annual, &c.sentLocal, &c.internal); err != nil {
				rows.Close()
				return filed, fmt.Errorf("scan archived message: %w", err)
			}
			found++
			conf := c.confidence
			dest := DestinationFolder(c.folder, c.annual, c.sentLocal, c.internal, now)
			byFolder[dest] = append(byFolder[dest], Move{
				MessageID: c.id, FromFolderID: root.ID, FromFolder: root.Name,
				ToFolder: dest, Reason: ReasonClassified, Category: c.category, Confidence: &conf,
			})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return filed, fmt.Errorf("find archived messages: %w", err)
		}
		folders := make([]string, 0, len(byFolder))
		for f := range byFolder {
			folders = append(folders, f)
		}
		sort.Strings(folders)
		for _, folder := range folders {
			moved, err := moveAndJournal(ctx, db, mailboxID, byFolder[folder], batch, "sort", filingDestination)
			if err != nil {
				return filed, fmt.Errorf("file into %q: %w", folder, err)
			}
			filed += moved
		}
		if found < opts.BatchSize {
			break
		}
	}
	if filed > 0 {
		logger.Info("archive sorter filed messages", "mailbox_id", mailboxID, "filed", filed, "batch", batch)
	}
	return filed, nil
}

// PurgeOptions configures the Trash purge.
type PurgeOptions struct {
	// Retention is how long a message stays in the \Trash folder before it is
	// destroyed. It is what lets a mistaken delete be recovered.
	Retention time.Duration
	// AllCopies also destroys every other copy of the same content in the
	// mailbox (see storage.PurgeMessages).
	AllCopies bool
	BatchSize int
	Logger    *slog.Logger
}

// PurgeStats reports one PurgeOnce pass.
type PurgeStats struct {
	Mailboxes int
	Messages  int64
	Copies    int64
	Bytes     int64
}

// PurgeOnce destroys what users have deleted: every message that has been in
// its mailbox's \Trash folder longer than Retention. Only the \Trash folder is
// ever read for candidates; with AllCopies, a candidate's identical copies in
// other folders go with it.
func PurgeOnce(ctx context.Context, db *storage.DB, opts PurgeOptions) (PurgeStats, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 500
	}
	if opts.Retention < 0 {
		return PurgeStats{}, errors.New("archive: purge retention must not be negative")
	}
	rows, err := db.Pool().Query(ctx, `
		SELECT DISTINCT ON (f.mailbox_id) f.mailbox_id, f.id, f.name
		  FROM folders f
		  JOIN mailboxes mb ON mb.id = f.mailbox_id
		 WHERE f.special_use = '\Trash' AND mb.maintenance_at IS NULL
		 ORDER BY f.mailbox_id, f.id`)
	if err != nil {
		return PurgeStats{}, fmt.Errorf("find trash folders: %w", err)
	}
	type target struct {
		mailboxID int64
		trash     storage.SpecialUseFolder
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.mailboxID, &t.trash.ID, &t.trash.Name); err != nil {
			rows.Close()
			return PurgeStats{}, fmt.Errorf("scan trash folder: %w", err)
		}
		targets = append(targets, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return PurgeStats{}, fmt.Errorf("find trash folders: %w", err)
	}

	var stats PurgeStats
	var firstErr error
	for _, t := range targets {
		stats.Mailboxes++
		res, err := purgeMailbox(ctx, db, t.mailboxID, t.trash, opts)
		stats.Messages += res.Messages
		stats.Copies += res.Copies
		stats.Bytes += res.Bytes
		if res.Messages > 0 {
			logger.Info("trash purge destroyed messages", "mailbox_id", t.mailboxID,
				"folder", t.trash.Name, "messages", res.Messages, "other_copies", res.Copies, "bytes", res.Bytes)
		}
		if err != nil {
			if ctx.Err() != nil {
				return stats, ctx.Err()
			}
			logger.Error("trash purge", "mailbox_id", t.mailboxID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return stats, firstErr
}

func purgeMailbox(ctx context.Context, db *storage.DB, mailboxID int64, trash storage.SpecialUseFolder, opts PurgeOptions) (storage.PurgeResult, error) {
	var total storage.PurgeResult
	for i := 0; i < maxBatchesPerMailbox; i++ {
		rows, err := db.Pool().Query(ctx, `
			SELECT id FROM messages
			 WHERE folder_id = $1 AND created_at <= now() - make_interval(secs => $2)
			 ORDER BY id LIMIT $3`,
			trash.ID, opts.Retention.Seconds(), opts.BatchSize)
		if err != nil {
			return total, fmt.Errorf("find expired trash: %w", err)
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return total, fmt.Errorf("scan expired trash: %w", err)
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return total, fmt.Errorf("find expired trash: %w", err)
		}
		if len(ids) == 0 {
			break
		}
		var res storage.PurgeResult
		if err := db.RunTx(ctx, func(tx pgx.Tx) error {
			var err error
			res, err = storage.PurgeMessages(ctx, tx, mailboxID, ids, opts.AllCopies)
			return err
		}); err != nil {
			return total, err
		}
		total.Messages += res.Messages
		total.Copies += res.Copies
		total.Bytes += res.Bytes
		if len(ids) < opts.BatchSize {
			break
		}
	}
	return total, nil
}

// DeletedTracker archives messages a client has marked \Deleted in INBOX but
// not expunged: Gmail's auto-expunge, for clients that mark on Delete and
// expunge only much later (Mac Mail with "Move deleted messages to the Trash
// mailbox" off). Delete means archive, and INBOX should empty when you press
// it, not days later.
//
// \Deleted carries no timestamp, so the tracker records when a pass first saw
// each mark and archives it once it has stood for SettleDelay. That leaves
// time for a client's Undo, which clears the mark. The record is in memory:
// after a restart the delay starts again. Only the last copy of a message is
// archived; a marked message with another copy elsewhere is a copy-then-delete
// move whose expunge is on its way.
type DeletedTracker struct {
	firstSeen map[int64]time.Time
}

// NewDeletedTracker returns an empty tracker. Keep one for the life of the
// process.
func NewDeletedTracker() *DeletedTracker {
	return &DeletedTracker{firstSeen: map[int64]time.Time{}}
}

// DeletedOptions configures DeletedTracker.ArchiveOnce.
type DeletedOptions struct {
	SettleDelay time.Duration
	BatchSize   int
	Logger      *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// ArchiveOnce runs one pass over every mailbox with an \Archive folder and
// returns how many messages it archived.
func (t *DeletedTracker) ArchiveOnce(ctx context.Context, db *storage.DB, opts DeletedOptions) (int, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 500
	}
	rows, err := db.Pool().Query(ctx, `
		SELECT r.mailbox_id, r.name, i.id, m.id
		  FROM (SELECT DISTINCT ON (f.mailbox_id) f.mailbox_id, f.name
		          FROM folders f JOIN mailboxes mb ON mb.id = f.mailbox_id
		         WHERE f.special_use = '\Archive' AND mb.maintenance_at IS NULL
		         ORDER BY f.mailbox_id, f.id) r
		  JOIN folders i ON i.mailbox_id = r.mailbox_id AND i.name = 'INBOX'
		  JOIN messages m ON m.folder_id = i.id
		 WHERE '\Deleted' = ANY(m.flags)
		   AND NOT EXISTS (
		       SELECT 1 FROM messages o JOIN folders of ON of.id = o.folder_id
		        WHERE of.mailbox_id = r.mailbox_id AND o.raw_sha256 = m.raw_sha256 AND o.id <> m.id)
		 ORDER BY r.mailbox_id, m.id`)
	if err != nil {
		return 0, fmt.Errorf("find deleted inbox mail: %w", err)
	}
	type group struct {
		mailboxID int64
		archive   string
		inboxID   int64
		ids       []int64
	}
	var groups []*group
	seen := map[int64]bool{}
	at := now()
	for rows.Next() {
		var mailboxID, inboxID, id int64
		var archive string
		if err := rows.Scan(&mailboxID, &archive, &inboxID, &id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan deleted inbox mail: %w", err)
		}
		seen[id] = true
		first, ok := t.firstSeen[id]
		if !ok {
			t.firstSeen[id] = at
			continue
		}
		if at.Sub(first) < opts.SettleDelay {
			continue
		}
		if len(groups) == 0 || groups[len(groups)-1].mailboxID != mailboxID {
			groups = append(groups, &group{mailboxID: mailboxID, archive: archive, inboxID: inboxID})
		}
		groups[len(groups)-1].ids = append(groups[len(groups)-1].ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("find deleted inbox mail: %w", err)
	}
	// Forget marks that are gone: undone, expunged, or archived below.
	for id := range t.firstSeen {
		if !seen[id] {
			delete(t.firstSeen, id)
		}
	}

	archived := 0
	for _, g := range groups {
		for start := 0; start < len(g.ids); start += opts.BatchSize {
			chunk := g.ids[start:min(start+opts.BatchSize, len(g.ids))]
			items := make([]storage.MoveItem, len(chunk))
			for i, id := range chunk {
				items[i] = storage.MoveItem{ID: id, FromFolderID: g.inboxID}
			}
			var moved []storage.MovedMessage
			err := db.RunTx(ctx, func(tx pgx.Tx) error {
				// Re-read the mark inside the transaction, so an Undo that
				// cleared it since the scan above is honoured. One that
				// lands after this read finds the message archived, which
				// loses nothing.
				still, err := stillDeleted(ctx, tx, chunk)
				if err != nil {
					return err
				}
				keep := items[:0:0]
				for _, it := range items {
					if still[it.ID] {
						keep = append(keep, it)
					}
				}
				moved, err = storage.ArchiveMessages(ctx, tx, g.mailboxID, keep, g.archive)
				return err
			})
			if err != nil {
				return archived, fmt.Errorf("archive deleted inbox mail of mailbox %d: %w", g.mailboxID, err)
			}
			archived += len(moved)
			for _, m := range moved {
				delete(t.firstSeen, m.ID)
			}
		}
	}
	if archived > 0 {
		logger.Info("archived messages left marked deleted in INBOX", "archived", archived)
	}
	return archived, nil
}

func stillDeleted(ctx context.Context, tx pgx.Tx, ids []int64) (map[int64]bool, error) {
	rows, err := tx.Query(ctx,
		`SELECT id FROM messages WHERE id = ANY($1) AND '\Deleted' = ANY(flags)`, ids)
	if err != nil {
		return nil, fmt.Errorf("re-read deleted marks: %w", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
