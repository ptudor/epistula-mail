package archive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ptudor/epistula-mail/database/storage"
)

// Reasons a planned move is made.
const (
	ReasonClassified = "classified"  // the classifier's category, at or above min_confidence
	ReasonFolderRule = "folder-rule" // a [folders] rule filed the whole folder
	ReasonDrain      = "drain"       // unfiled mail leaving a drained folder for the \Archive folder
)

// Reasons a message stays where it is.
const (
	StayKeep          = "keep"             // a keep rule, or a \Sent, \Drafts or \Junk folder
	StayFiled         = "filed"            // already in a category folder
	StayDeletedFlag   = `flagged \Deleted` // a client marked it for expunge; moving it would carry the mark
	StayInboxRecent   = "inbox-recent"     // INBOX mail newer than inbox_keep_days
	StayUnclassified  = "unclassified"     // no current classification
	StayLowConfidence = "low-confidence"   // classified below min_confidence
)

// ErrSpecialUseDestination is returned when a move would file mail into a
// folder that plays a special-use role: \Trash above all, whose contents the
// purge destroys.
var ErrSpecialUseDestination = errors.New("archive: refusing to file into a special-use folder")

// ErrUnknownRuleCategory is returned when a [folders] rule names a key that is
// not an active category of the mailbox.
var ErrUnknownRuleCategory = errors.New("archive: rule names a category that is not active")

// Move is one planned server-side move.
type Move struct {
	MessageID    int64
	FromFolderID int64
	FromFolder   string
	ToFolder     string
	Reason       string
	// Category and Confidence are set when a category decided the move.
	Category   string
	Confidence *float32
}

// FolderSummary is what the plan does to one source folder.
type FolderSummary struct {
	Folder string
	Total  int
	Moves  map[string]int // destination folder → count
	Stays  map[string]int // stay reason → count
}

// Plan is a computed reorganization of one mailbox. It is a snapshot: Apply
// moves a message only if it is still in the folder the plan found it in.
type Plan struct {
	MailboxID     int64
	ArchiveRoot   string
	Categories    int
	TotalMessages int
	Moves         []Move
	Folders       []FolderSummary
}

// category is one active category as the plan uses it.
type category struct {
	folder string
	annual bool
}

// folderPolicy is what the rules say about one folder.
type folderPolicy struct {
	id        int64
	name      string
	keep      bool
	filed     bool      // an active category folder
	mapped    *category // the category a [folders] rule sends everything to
	mappedKey string    // and the key that rule names
	drain     bool
	inbox     bool
	isRoot    bool
	summary   *FolderSummary
}

// BuildPlan decides, for every message of the mailbox, whether the
// reorganization moves it and where. It reads a consistent snapshot in one
// REPEATABLE READ transaction and writes nothing.
func BuildPlan(ctx context.Context, db *storage.DB, mailboxID int64, rules Rules, now time.Time) (*Plan, error) {
	if err := rules.Validate(); err != nil {
		return nil, err
	}
	var plan *Plan
	err := pgx.BeginTxFunc(ctx, db.Pool(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		plan, err = buildPlanTx(ctx, tx, mailboxID, rules, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	return plan, nil
}

func buildPlanTx(ctx context.Context, tx pgx.Tx, mailboxID int64, rules Rules, now time.Time) (*Plan, error) {
	root, err := storage.FindSpecialUseFolder(ctx, tx, mailboxID, `\Archive`)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, storage.ErrNoArchiveFolder
	}
	if err != nil {
		return nil, err
	}

	categories := map[string]*category{}
	catFolders := map[string]bool{}
	annualFolders := map[string]bool{}
	catRows, err := tx.Query(ctx,
		`SELECT key, folder, annual FROM archive_categories WHERE mailbox_id = $1 AND retired_at IS NULL`, mailboxID)
	if err != nil {
		return nil, fmt.Errorf("read categories: %w", err)
	}
	for catRows.Next() {
		var key string
		c := &category{}
		if err := catRows.Scan(&key, &c.folder, &c.annual); err != nil {
			catRows.Close()
			return nil, fmt.Errorf("scan category: %w", err)
		}
		categories[key] = c
		catFolders[c.folder] = true
		if c.annual {
			annualFolders[c.folder] = true
		}
	}
	catRows.Close()
	if err := catRows.Err(); err != nil {
		return nil, fmt.Errorf("read categories: %w", err)
	}
	if len(categories) == 0 {
		return nil, fmt.Errorf("archive: mailbox has no active categories; import a category list first")
	}
	for name, key := range rules.Folders {
		if _, ok := categories[key]; !ok {
			return nil, fmt.Errorf("%w: folders rule %q → %q", ErrUnknownRuleCategory, name, key)
		}
	}

	// Folders that play a role the reorganization must not touch, and their
	// subtrees: an imported "Sent/2004/11-Nov" is sent mail as much as "Sent".
	var keepRoots, trashRoots []string
	policies := map[int64]*folderPolicy{}
	fRows, err := tx.Query(ctx,
		`SELECT id, name, COALESCE(special_use, '') FROM folders WHERE mailbox_id = $1`, mailboxID)
	if err != nil {
		return nil, fmt.Errorf("read folders: %w", err)
	}
	for fRows.Next() {
		p := &folderPolicy{}
		var special string
		if err := fRows.Scan(&p.id, &p.name, &special); err != nil {
			fRows.Close()
			return nil, fmt.Errorf("scan folder: %w", err)
		}
		switch special {
		case `\Sent`, `\Drafts`, `\Junk`:
			keepRoots = append(keepRoots, p.name)
		case `\Trash`:
			trashRoots = append(trashRoots, p.name)
		}
		policies[p.id] = p
	}
	fRows.Close()
	if err := fRows.Err(); err != nil {
		return nil, fmt.Errorf("read folders: %w", err)
	}
	keepRoots = append(keepRoots, rules.Keep...)
	drainRoots := append(append([]string{}, trashRoots...), rules.Drain...)
	for _, p := range policies {
		p.inbox = p.name == "INBOX"
		p.isRoot = p.name == root.Name
		p.filed = catFolders[p.name]
		if i := strings.LastIndexByte(p.name, '/'); i > 0 && annualFolders[p.name[:i]] &&
			storage.IsYearFolderOf(p.name, p.name[:i]) {
			p.filed = true
		}
		for _, k := range keepRoots {
			if matchesSubtree(p.name, k) {
				p.keep = true
			}
		}
		for _, d := range drainRoots {
			if matchesSubtree(p.name, d) {
				p.drain = true
			}
		}
		if key, ok := longestSubtreeMatch(p.name, rules.Folders); ok {
			p.mapped, p.mappedKey = categories[key], key
		}
		p.summary = &FolderSummary{Folder: p.name, Moves: map[string]int{}, Stays: map[string]int{}}
	}

	plan := &Plan{MailboxID: mailboxID, ArchiveRoot: root.Name, Categories: len(categories)}
	inboxCutoff := now.Add(-time.Duration(rules.InboxKeepDays) * 24 * time.Hour)

	rows, err := tx.Query(ctx, `
		SELECT m.id, m.folder_id, m.internal_date, m.sent_date_local, '\Deleted' = ANY(m.flags),
		       c.category, c.confidence
		  FROM messages m
		  JOIN folders f ON f.id = m.folder_id
		  LEFT JOIN message_classifications c ON c.message_id = m.id
		 WHERE f.mailbox_id = $1
		 ORDER BY m.id`, mailboxID)
	if err != nil {
		return nil, fmt.Errorf("read messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id, folderID int64
			internal     time.Time
			sentLocal    *time.Time
			deleted      bool
			catKey       *string
			confidence   *float32
		)
		if err := rows.Scan(&id, &folderID, &internal, &sentLocal, &deleted, &catKey, &confidence); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		p, ok := policies[folderID]
		if !ok {
			return nil, fmt.Errorf("message %d is in folder %d, which the snapshot does not contain", id, folderID)
		}
		plan.TotalMessages++
		p.summary.Total++

		stay := func(reason string) { p.summary.Stays[reason]++ }
		move := func(to, reason, cat string, conf *float32) {
			if to == p.name {
				stay(StayFiled)
				return
			}
			plan.Moves = append(plan.Moves, Move{
				MessageID: id, FromFolderID: folderID, FromFolder: p.name,
				ToFolder: to, Reason: reason, Category: cat, Confidence: conf,
			})
			p.summary.Moves[to]++
		}

		switch {
		case p.keep:
			stay(StayKeep)
			continue
		case p.filed:
			stay(StayFiled)
			continue
		case deleted:
			stay(StayDeletedFlag)
			continue
		case p.inbox && internal.After(inboxCutoff):
			stay(StayInboxRecent)
			continue
		case p.mapped != nil:
			move(DestinationFolder(p.mapped.folder, p.mapped.annual, sentLocal, internal, now),
				ReasonFolderRule, p.mappedKey, nil)
			continue
		}

		var cat *category
		if catKey != nil {
			cat = categories[*catKey]
		}
		active := cat != nil
		if active && confidence != nil && float64(*confidence) >= rules.MinConfidence {
			move(DestinationFolder(cat.folder, cat.annual, sentLocal, internal, now),
				ReasonClassified, *catKey, confidence)
			continue
		}
		if (p.drain || p.inbox) && !p.isRoot {
			move(root.Name, ReasonDrain, "", nil)
			continue
		}
		if active {
			stay(StayLowConfidence)
		} else {
			stay(StayUnclassified)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read messages: %w", err)
	}

	for _, p := range policies {
		if p.summary.Total > 0 {
			plan.Folders = append(plan.Folders, *p.summary)
		}
	}
	sort.Slice(plan.Folders, func(i, j int) bool { return plan.Folders[i].Folder < plan.Folders[j].Folder })
	sort.SliceStable(plan.Moves, func(i, j int) bool {
		if plan.Moves[i].ToFolder != plan.Moves[j].ToFolder {
			return plan.Moves[i].ToFolder < plan.Moves[j].ToFolder
		}
		return plan.Moves[i].MessageID < plan.Moves[j].MessageID
	})
	return plan, nil
}

// WriteReport prints the plan for a human: totals, then every source folder
// with where its messages go and why the rest stay.
func (p *Plan) WriteReport(w io.Writer) error {
	staying := p.TotalMessages - len(p.Moves)
	if _, err := fmt.Fprintf(w, "archive folder %q, %d active categories\n%d messages: %d move, %d stay\n",
		p.ArchiveRoot, p.Categories, p.TotalMessages, len(p.Moves), staying); err != nil {
		return err
	}

	byDest := map[string]int{}
	for _, m := range p.Moves {
		byDest[m.ToFolder]++
	}
	if len(byDest) > 0 {
		if _, err := fmt.Fprintln(w, "\ndestinations:"); err != nil {
			return err
		}
		for _, kv := range sortedCounts(byDest) {
			if _, err := fmt.Fprintf(w, "  %8d  %s\n", kv.n, kv.k); err != nil {
				return err
			}
		}
	}

	if _, err := fmt.Fprintln(w, "\nby source folder:"); err != nil {
		return err
	}
	for _, f := range p.Folders {
		if _, err := fmt.Fprintf(w, "\n%s (%d)\n", f.Folder, f.Total); err != nil {
			return err
		}
		for _, kv := range sortedCounts(f.Moves) {
			if _, err := fmt.Fprintf(w, "  %8d  -> %s\n", kv.n, kv.k); err != nil {
				return err
			}
		}
		if len(f.Stays) > 0 {
			parts := make([]string, 0, len(f.Stays))
			for _, kv := range sortedCounts(f.Stays) {
				parts = append(parts, fmt.Sprintf("%d %s", kv.n, kv.k))
			}
			if _, err := fmt.Fprintf(w, "            stays: %s\n", strings.Join(parts, ", ")); err != nil {
				return err
			}
		}
	}
	return nil
}

// WriteMoves writes every planned move as tab-separated values, for review
// or for grepping one message's fate.
func (p *Plan) WriteMoves(w io.Writer) error {
	if _, err := fmt.Fprintln(w, "message_id\tfrom\tto\treason\tcategory\tconfidence"); err != nil {
		return err
	}
	for _, m := range p.Moves {
		conf := ""
		if m.Confidence != nil {
			conf = fmt.Sprintf("%.2f", *m.Confidence)
		}
		if _, err := fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n",
			m.MessageID, tsvField(m.FromFolder), tsvField(m.ToFolder), m.Reason, m.Category, conf); err != nil {
			return err
		}
	}
	return nil
}

// tsvField keeps a folder name on one TSV cell. Folder names cannot hold
// control characters (storage.ValidateFolderName), so only a tab written by an
// older import could need this.
func tsvField(s string) string { return strings.ReplaceAll(s, "\t", " ") }

type countEntry struct {
	k string
	n int
}

func sortedCounts(m map[string]int) []countEntry {
	out := make([]countEntry, 0, len(m))
	for k, n := range m {
		out = append(out, countEntry{k, n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].k < out[j].k
	})
	return out
}

// ApplyOptions tunes Apply.
type ApplyOptions struct {
	// Batch names this run in the archive_moves journal; archive-undo takes it.
	Batch string
	// BatchSize is how many messages move per transaction. Each transaction
	// holds the mailbox row lock, which delivery also takes, so a batch is
	// kept short enough that no delivery waits long.
	BatchSize int
	// Limit stops after this many moves; 0 is no limit.
	Limit int
	// Pause sleeps between transactions to leave room for live traffic.
	Pause time.Duration
	// Progress, when set, is called after every transaction.
	Progress func(done, total int)
}

// ApplyStats reports what Apply did.
type ApplyStats struct {
	Moved int
	// Skipped counts planned moves whose message had left the folder the plan
	// found it in (or no longer exists) by the time its batch ran.
	Skipped      int
	Transactions int
}

// Apply carries out a plan's moves, one transaction per batch, journaling
// each move under opts.Batch. It is safe to interrupt: every committed batch
// is complete and journaled, and running a fresh plan again picks up where it
// stopped, since a message already in its destination is not planned again.
func Apply(ctx context.Context, db *storage.DB, plan *Plan, opts ApplyOptions) (ApplyStats, error) {
	if opts.Batch == "" {
		return ApplyStats{}, errors.New("archive: apply needs a batch name")
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 500
	}
	moves := plan.Moves
	if opts.Limit > 0 && len(moves) > opts.Limit {
		moves = moves[:opts.Limit]
	}
	gin, err := newGINFlusher(ctx, db)
	if err != nil {
		return ApplyStats{}, err
	}
	var stats ApplyStats
	for start := 0; start < len(moves); {
		end := start + 1
		for end < len(moves) && end-start < opts.BatchSize && moves[end].ToFolder == moves[start].ToFolder {
			end++
		}
		chunk := moves[start:end]
		moved, err := moveAndJournal(ctx, db, plan.MailboxID, chunk, opts.Batch, "reorg", filingDestination)
		if err != nil {
			return stats, fmt.Errorf("move %d message(s) to %q: %w", len(chunk), chunk[0].ToFolder, err)
		}
		stats.Moved += moved
		stats.Skipped += len(chunk) - moved
		stats.Transactions++
		start = end
		if err := gin.flush(ctx); err != nil {
			return stats, err
		}
		if opts.Progress != nil {
			opts.Progress(start, len(moves))
		}
		if opts.Pause > 0 && start < len(moves) {
			select {
			case <-ctx.Done():
				return stats, ctx.Err()
			case <-time.After(opts.Pause):
			}
		}
	}
	return stats, nil
}

// destinationRule says which special-use roles a move may file into.
type destinationRule func(specialUse string) bool

// filingDestination is the rule for filing (the reorganization and the live
// sorter): no special-use folder but \Archive, the unsorted pile a drain
// sends mail to.
func filingDestination(specialUse string) bool { return specialUse == `\Archive` }

// mergeDestination is the rule for an operator's folder merge, which names its
// destination explicitly: anything but \Trash, where the purge would destroy
// the merged mail.
func mergeDestination(specialUse string) bool { return specialUse != `\Trash` }

// moveAndJournal moves one same-destination chunk and journals what moved, in
// one transaction.
func moveAndJournal(ctx context.Context, db *storage.DB, mailboxID int64, chunk []Move, batch, reason string, allowed destinationRule) (int, error) {
	items := make([]storage.MoveItem, len(chunk))
	byID := make(map[int64]Move, len(chunk))
	for i, m := range chunk {
		items[i] = storage.MoveItem{ID: m.MessageID, FromFolderID: m.FromFolderID}
		byID[m.MessageID] = m
	}
	var moved int
	err := db.RunTx(ctx, func(tx pgx.Tx) error {
		// Categories are validated when imported, but a destination can be a
		// year folder below one, or a folder given a role since: the
		// destination's special-use role is checked against the caller's
		// rule every time.
		var special *string
		switch err := tx.QueryRow(ctx,
			`SELECT special_use FROM folders WHERE mailbox_id = $1 AND name = $2`,
			mailboxID, chunk[0].ToFolder,
		).Scan(&special); {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return fmt.Errorf("check destination: %w", err)
		case special != nil && !allowed(*special):
			return fmt.Errorf("%w: %q carries %s", ErrSpecialUseDestination, chunk[0].ToFolder, *special)
		}
		done, err := storage.MoveMessages(ctx, tx, mailboxID, items, chunk[0].ToFolder)
		if err != nil {
			return err
		}
		moved = len(done)
		if len(done) == 0 {
			return nil
		}
		ids := make([]int64, len(done))
		froms := make([]string, len(done))
		cats := make([]*string, len(done))
		confs := make([]*float32, len(done))
		for i, d := range done {
			m := byID[d.ID]
			ids[i] = d.ID
			froms[i] = d.FromFolder
			if m.Category != "" {
				c := m.Category
				cats[i] = &c
			}
			confs[i] = m.Confidence
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO archive_moves (mailbox_id, message_id, batch, reason, from_folder, to_folder, category, confidence)
			SELECT $1, t.id, $2, $3, t.from_folder, $4, t.category, t.confidence
			  FROM unnest($5::bigint[], $6::text[], $7::text[], $8::real[])
			       AS t(id, from_folder, category, confidence)`,
			mailboxID, batch, reason, chunk[0].ToFolder, ids, froms, cats, confs)
		if err != nil {
			return fmt.Errorf("journal moves: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return moved, nil
}

// UndoStats reports what Undo did.
type UndoStats struct {
	Restored int
	// Moved counts journaled messages that have left the folder the batch put
	// them in since; they are left where they are now.
	Moved int
	// Redirects counts the folder redirects a merge batch recorded, removed
	// with it so later imports go to the restored folders again.
	Redirects int
}

// Undo returns every message a journaled batch moved to the folder it came
// from, when it is still where the batch put it, recreating source folders a
// prune removed. Each restored entry is marked undone, so running it again
// changes nothing. A merge batch's folder redirects are removed with it. With
// dryRun it reports without writing.
func Undo(ctx context.Context, db *storage.DB, mailboxID int64, batch string, dryRun bool, batchSize int) (UndoStats, error) {
	if batchSize <= 0 {
		batchSize = 500
	}
	type entry struct {
		journalID     int64
		messageID     int64
		fromFolder    string
		currentFolder int64
	}
	rows, err := db.Pool().Query(ctx, `
		SELECT DISTINCT ON (j.message_id)
		       j.id, j.message_id, j.from_folder, j.to_folder, m.folder_id, f.name
		  FROM archive_moves j
		  JOIN messages m ON m.id = j.message_id
		  JOIN folders f ON f.id = m.folder_id
		 WHERE j.mailbox_id = $1 AND j.batch = $2 AND j.undone_at IS NULL
		 ORDER BY j.message_id, j.id DESC`, mailboxID, batch)
	if err != nil {
		return UndoStats{}, fmt.Errorf("read journal: %w", err)
	}
	var stats UndoStats
	byFrom := map[string][]entry{}
	for rows.Next() {
		var e entry
		var toFolder, current string
		if err := rows.Scan(&e.journalID, &e.messageID, &e.fromFolder, &toFolder, &e.currentFolder, &current); err != nil {
			rows.Close()
			return UndoStats{}, fmt.Errorf("scan journal: %w", err)
		}
		if current != toFolder {
			stats.Moved++
			continue
		}
		byFrom[e.fromFolder] = append(byFrom[e.fromFolder], e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return UndoStats{}, fmt.Errorf("read journal: %w", err)
	}
	if dryRun {
		for _, list := range byFrom {
			stats.Restored += len(list)
		}
		stats.Redirects, err = db.CountFolderRedirects(ctx, mailboxID, batch)
		return stats, err
	}

	froms := make([]string, 0, len(byFrom))
	for f := range byFrom {
		froms = append(froms, f)
	}
	sort.Strings(froms)
	gin, err := newGINFlusher(ctx, db)
	if err != nil {
		return stats, err
	}
	for _, from := range froms {
		list := byFrom[from]
		for start := 0; start < len(list); start += batchSize {
			end := min(start+batchSize, len(list))
			chunk := list[start:end]
			items := make([]storage.MoveItem, len(chunk))
			journalOf := make(map[int64]int64, len(chunk))
			for i, e := range chunk {
				items[i] = storage.MoveItem{ID: e.messageID, FromFolderID: e.currentFolder}
				journalOf[e.messageID] = e.journalID
			}
			var restored int
			err := db.RunTx(ctx, func(tx pgx.Tx) error {
				done, err := storage.MoveMessages(ctx, tx, mailboxID, items, from)
				if err != nil {
					return err
				}
				undone := make([]int64, len(done))
				for i, d := range done {
					undone[i] = journalOf[d.ID]
				}
				if _, err := tx.Exec(ctx,
					`UPDATE archive_moves SET undone_at = now() WHERE id = ANY($1)`, undone,
				); err != nil {
					return fmt.Errorf("mark undone: %w", err)
				}
				restored = len(done)
				return nil
			})
			if err != nil {
				return stats, fmt.Errorf("restore %d message(s) to %q: %w", len(chunk), from, err)
			}
			stats.Restored += restored
			stats.Moved += len(chunk) - restored
			if err := gin.flush(ctx); err != nil {
				return stats, err
			}
		}
	}
	// Last, so an undo that stops early leaves the redirect in place with
	// the moves it still describes; re-running finishes both.
	stats.Redirects, err = db.DeleteFolderRedirects(ctx, mailboxID, batch)
	return stats, err
}
