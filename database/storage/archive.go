package storage

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Server-side message filing and destruction for archive sorting
// (ARCHIVE_SORTING.md at the repository root; schema in migration 020).
//
// Every other path that places a message in a folder — delivery, import,
// APPEND, IMAP COPY and MOVE — inserts a new row. MoveMessages instead rewrites
// the row in place: the message keeps its id, so its attachments, annotations,
// classification and journal entries stay attached without being copied, and
// PostgreSQL keeps the out-of-line (TOAST) values of text_body, html_body and
// headers instead of writing them again. That is what makes it cheap enough to
// refile a whole archive on a nearly full pool.

// ErrNoArchiveFolder is returned when a mailbox has no folder carrying the
// RFC 6154 \Archive attribute, which archive sorting is anchored to.
var ErrNoArchiveFolder = errors.New(`storage: mailbox has no \Archive folder`)

// ErrInvalidArchiveCategory is returned for a category list that fails
// validation; the wrapped text names the offending entry.
var ErrInvalidArchiveCategory = errors.New("storage: invalid archive category")

// ErrInvalidFolderName is returned for a folder name the store must never hold.
var ErrInvalidFolderName = errors.New("storage: invalid folder name")

// MaxFolderNameBytes is the widely assumed IMAP mailbox-name ceiling, the same
// limit epistula-imap's CREATE enforces.
const MaxFolderNameBytes = 255

// MaxArchiveCategoryDescription bounds a category description, which is sent
// to the classifier with every message.
const MaxArchiveCategoryDescription = 500

var archiveKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}(/[a-z0-9][a-z0-9-]{0,39}){0,2}$`)

// ValidArchiveKey reports whether k is a well-formed category key: one to
// three lower-case slug segments separated by "/" ("finance/banking/acme-bank"),
// matching the archive_categories_key_chk constraint.
func ValidArchiveKey(k string) bool { return archiveKeyRe.MatchString(k) }

var yearLeafRe = regexp.MustCompile(`^[0-9]{4}$`)

// IsYearFolderOf reports whether name is a year folder, <parent>/<YYYY>,
// directly below parent: where an annual category files its mail.
func IsYearFolderOf(name, parent string) bool {
	leaf, ok := strings.CutPrefix(name, parent+"/")
	return ok && yearLeafRe.MatchString(leaf)
}

// ValidateFolderName applies the rule epistula-imap's CREATE gate enforces
// (RA6X-009) plus the hierarchy's own shape: non-empty, at most
// MaxFolderNameBytes, no control characters, and no empty hierarchy segment
// (a leading, trailing or doubled "/"). Admin paths that name a folder on the
// operator's behalf use it so they cannot create a folder a client could not.
func ValidateFolderName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty", ErrInvalidFolderName)
	}
	if len(name) > MaxFolderNameBytes {
		return fmt.Errorf("%w: %q exceeds %d bytes", ErrInvalidFolderName, name, MaxFolderNameBytes)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: %q contains a control character", ErrInvalidFolderName, name)
		}
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" {
			return fmt.Errorf("%w: %q has an empty hierarchy segment", ErrInvalidFolderName, name)
		}
	}
	return nil
}

// SpecialUseFolder is a folder located by its special-use attribute.
type SpecialUseFolder struct {
	ID   int64
	Name string
}

// FindSpecialUseFolder returns the folder of mailboxID carrying attr (canonical
// spelling, e.g. `\Archive`), or ErrNotFound. When several folders carry it —
// IMAP CREATE does not prevent that — the lowest id wins, so every caller
// agrees on which one.
func FindSpecialUseFolder(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, mailboxID int64, attr string) (SpecialUseFolder, error) {
	var f SpecialUseFolder
	err := q.QueryRow(ctx,
		`SELECT id, name FROM folders
		  WHERE mailbox_id = $1 AND special_use = $2
		  ORDER BY id LIMIT 1`, mailboxID, attr,
	).Scan(&f.ID, &f.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return SpecialUseFolder{}, ErrNotFound
	}
	if err != nil {
		return SpecialUseFolder{}, fmt.Errorf("find %s folder: %w", attr, err)
	}
	return f, nil
}

// IsUnderFolder reports whether name is a strict descendant of root in the
// "/" hierarchy.
func IsUnderFolder(name, root string) bool {
	return strings.HasPrefix(name, root+"/") && len(name) > len(root)+1
}

// ---- category list ----

// ArchiveCategory is one entry of a mailbox's approved archive taxonomy.
type ArchiveCategory struct {
	Key         string
	Folder      string
	Description string
	// Annual categories file into a year folder below Folder,
	// Folder/<YYYY>, the year taken from the message's own date.
	Annual  bool
	Retired bool
}

// CategoryChanges reports what ReplaceArchiveCategories did (or, dry, would
// do), by key.
type CategoryChanges struct {
	Added     []string
	Updated   []string
	Retired   []string
	Unchanged []string
	// ArchiveRoot is the \Archive folder the categories were validated
	// against.
	ArchiveRoot string
}

// ListArchiveCategories returns the mailbox's categories ordered by key,
// retired ones only when includeRetired is set.
func (db *DB) ListArchiveCategories(ctx context.Context, mailboxID int64, includeRetired bool) ([]ArchiveCategory, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT key, folder, description, annual, retired_at IS NOT NULL
		   FROM archive_categories
		  WHERE mailbox_id = $1 AND ($2 OR retired_at IS NULL)
		  ORDER BY key`, mailboxID, includeRetired)
	if err != nil {
		return nil, fmt.Errorf("list archive categories: %w", err)
	}
	defer rows.Close()
	var out []ArchiveCategory
	for rows.Next() {
		var c ArchiveCategory
		if err := rows.Scan(&c.Key, &c.Folder, &c.Description, &c.Annual, &c.Retired); err != nil {
			return nil, fmt.Errorf("scan archive category: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ReplaceArchiveCategories makes cats the mailbox's active category list:
// listed keys are inserted, updated or reactivated, and every active key not
// listed is retired. Retiring rather than deleting keeps older
// classifications explicable; a retired key files nothing.
//
// Validation happens under the mailbox row lock, against the folder that
// carries \Archive at that moment: every category folder must be a strict
// descendant of it, a legal folder name, and not a folder that already plays
// a special-use role — so no list, however written, can make the sorter file
// mail into INBOX, Sent or Trash. With dryRun the changes are computed and
// nothing is written.
func (db *DB) ReplaceArchiveCategories(ctx context.Context, mailboxID int64, cats []ArchiveCategory, dryRun bool) (CategoryChanges, error) {
	if len(cats) == 0 {
		return CategoryChanges{}, fmt.Errorf("%w: the list is empty", ErrInvalidArchiveCategory)
	}
	var changes CategoryChanges
	err := db.runTx(ctx, func(tx pgx.Tx) error {
		changes = CategoryChanges{}
		if !dryRun {
			if err := lockMailboxRow(ctx, tx, mailboxID); err != nil {
				return err
			}
		}
		root, err := FindSpecialUseFolder(ctx, tx, mailboxID, `\Archive`)
		if errors.Is(err, ErrNotFound) {
			return ErrNoArchiveFolder
		}
		if err != nil {
			return err
		}
		changes.ArchiveRoot = root.Name
		if err := validateCategories(ctx, tx, mailboxID, root.Name, cats); err != nil {
			return err
		}

		existing := map[string]ArchiveCategory{}
		rows, err := tx.Query(ctx,
			`SELECT key, folder, description, annual, retired_at IS NOT NULL
			   FROM archive_categories WHERE mailbox_id = $1`, mailboxID)
		if err != nil {
			return fmt.Errorf("read archive categories: %w", err)
		}
		for rows.Next() {
			var c ArchiveCategory
			if err := rows.Scan(&c.Key, &c.Folder, &c.Description, &c.Annual, &c.Retired); err != nil {
				rows.Close()
				return fmt.Errorf("scan archive category: %w", err)
			}
			existing[c.Key] = c
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read archive categories: %w", err)
		}

		listed := map[string]bool{}
		for _, c := range cats {
			listed[c.Key] = true
			old, ok := existing[c.Key]
			switch {
			case !ok:
				changes.Added = append(changes.Added, c.Key)
			case old.Retired || old.Folder != c.Folder || old.Description != c.Description || old.Annual != c.Annual:
				changes.Updated = append(changes.Updated, c.Key)
			default:
				changes.Unchanged = append(changes.Unchanged, c.Key)
				continue
			}
			if dryRun {
				continue
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO archive_categories (mailbox_id, key, folder, description, annual)
				 VALUES ($1, $2, $3, $4, $5)
				 ON CONFLICT (mailbox_id, key) DO UPDATE
				   SET folder = EXCLUDED.folder, description = EXCLUDED.description,
				       annual = EXCLUDED.annual, retired_at = NULL, updated_at = now()`,
				mailboxID, c.Key, c.Folder, c.Description, c.Annual,
			); err != nil {
				return fmt.Errorf("store category %q: %w", c.Key, err)
			}
		}
		for key, old := range existing {
			if listed[key] || old.Retired {
				continue
			}
			changes.Retired = append(changes.Retired, key)
			if dryRun {
				continue
			}
			if _, err := tx.Exec(ctx,
				`UPDATE archive_categories SET retired_at = now(), updated_at = now()
				  WHERE mailbox_id = $1 AND key = $2`, mailboxID, key,
			); err != nil {
				return fmt.Errorf("retire category %q: %w", key, err)
			}
		}
		sort.Strings(changes.Added)
		sort.Strings(changes.Updated)
		sort.Strings(changes.Retired)
		sort.Strings(changes.Unchanged)
		return nil
	})
	if err != nil {
		return CategoryChanges{}, err
	}
	return changes, nil
}

func validateCategories(ctx context.Context, tx pgx.Tx, mailboxID int64, root string, cats []ArchiveCategory) error {
	seen := map[string]bool{}
	for i, c := range cats {
		where := fmt.Sprintf("category %d (%q)", i+1, c.Key)
		if !ValidArchiveKey(c.Key) {
			return fmt.Errorf("%w: %s: key must be one to three lower-case segments of [a-z0-9-] (at most 40 characters each) separated by \"/\"", ErrInvalidArchiveCategory, where)
		}
		if seen[c.Key] {
			return fmt.Errorf("%w: %s: key is listed twice", ErrInvalidArchiveCategory, where)
		}
		seen[c.Key] = true
		if err := ValidateFolderName(c.Folder); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrInvalidArchiveCategory, where, err)
		}
		if !IsUnderFolder(c.Folder, root) {
			return fmt.Errorf("%w: %s: folder %q is not inside the \\Archive folder %q", ErrInvalidArchiveCategory, where, c.Folder, root)
		}
		if c.Annual && len(c.Folder)+len("/2025") > MaxFolderNameBytes {
			return fmt.Errorf("%w: %s: folder %q leaves no room for its year folder within %d bytes", ErrInvalidArchiveCategory, where, c.Folder, MaxFolderNameBytes)
		}
		if len(c.Description) > MaxArchiveCategoryDescription {
			return fmt.Errorf("%w: %s: description exceeds %d bytes", ErrInvalidArchiveCategory, where, MaxArchiveCategoryDescription)
		}
		if strings.ContainsRune(c.Description, 0) {
			return fmt.Errorf("%w: %s: description contains a NUL character", ErrInvalidArchiveCategory, where)
		}
		var special *string
		err := tx.QueryRow(ctx,
			`SELECT special_use FROM folders WHERE mailbox_id = $1 AND name = $2`,
			mailboxID, c.Folder,
		).Scan(&special)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return fmt.Errorf("check folder %q: %w", c.Folder, err)
		case special != nil:
			return fmt.Errorf("%w: %s: folder %q carries %s; a category may not file into a special-use folder", ErrInvalidArchiveCategory, where, c.Folder, *special)
		}
	}
	// An annual category owns the year folders below its own; another
	// category filing into one of them would mix the two.
	for _, a := range cats {
		if !a.Annual {
			continue
		}
		for _, c := range cats {
			if IsYearFolderOf(c.Folder, a.Folder) {
				return fmt.Errorf("%w: category %q files into %q, a year folder of annual category %q",
					ErrInvalidArchiveCategory, c.Key, c.Folder, a.Key)
			}
		}
	}
	return nil
}

// ---- moving ----

// MoveItem names one message to move and the folder it is expected to be in.
// A message found anywhere else is left alone: the caller decided to move it
// from that folder, and someone has moved it since.
type MoveItem struct {
	ID           int64
	FromFolderID int64
}

// MovedMessage reports one message MoveMessages relocated.
type MovedMessage struct {
	ID           int64
	FromFolderID int64
	FromFolder   string
	NewUID       int64
}

// MoveMessages moves messages of mailboxID into the folder named dest,
// creating it and its ancestors when missing, by rewriting each row's
// folder_id and uid in place. It returns the messages it moved; an item that
// no longer exists, is no longer in its expected folder, or is already in dest
// is skipped.
//
// It runs inside the caller's transaction and follows the canonical lock order
// (epistula-imap lockorder.go): the mailbox row, then the folder rows in
// ascending id, then the message rows in ascending id. No blob advisory lock
// is needed: no new reference to any blob is created, and the reference GC
// checks — (sha256, bucket) within the mailbox — is unchanged. Quota is
// unchanged for the same reason.
//
// What an IMAP client sees is exactly a MOVE: the messages leave their source
// folders (whose highest_modseq advances) and arrive in dest under fresh UIDs
// allocated from dest's uidnext, in (internal_date, id) order so a folder's UID
// order stays chronological. created_at is reset to now(), keeping it "the time
// this row entered its current folder" (schema.sql). Both sides are announced
// on mail_arrived so IDLE sessions notice.
func MoveMessages(ctx context.Context, tx pgx.Tx, mailboxID int64, items []MoveItem, dest string) ([]MovedMessage, error) {
	if err := ValidateFolderName(dest); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}
	expected := make(map[int64]int64, len(items))
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		if _, dup := expected[it.ID]; dup {
			continue
		}
		expected[it.ID] = it.FromFolderID
		ids = append(ids, it.ID)
	}

	// Level 1. Every writer that moves, inserts or deletes a message in this
	// mailbox — delivery, APPEND, COPY/MOVE, EXPUNGE, folder DELETE — takes it
	// first, so the folder membership read below cannot change before commit.
	if err := lockMailboxRow(ctx, tx, mailboxID); err != nil {
		return nil, err
	}
	// Only look the destination up for now: it is created below, once it is
	// known that something will move into it, so a move of nothing (an undo
	// whose messages have all been refiled since) does not resurrect a
	// pruned folder.
	var destID int64
	switch existing, err := lookupFolderTx(ctx, tx, mailboxID, dest); {
	case err == nil:
		destID = existing.ID
	case !errors.Is(err, ErrNotFound):
		return nil, err
	}

	rows, err := tx.Query(ctx,
		`SELECT m.id, m.folder_id, f.name
		   FROM messages m JOIN folders f ON f.id = m.folder_id
		  WHERE m.id = ANY($1) AND f.mailbox_id = $2`, ids, mailboxID)
	if err != nil {
		return nil, fmt.Errorf("read move sources: %w", err)
	}
	var moving []MovedMessage
	folderSet := map[int64]bool{}
	for rows.Next() {
		var m MovedMessage
		if err := rows.Scan(&m.ID, &m.FromFolderID, &m.FromFolder); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan move source: %w", err)
		}
		if m.FromFolderID == destID || m.FromFolderID != expected[m.ID] {
			continue
		}
		moving = append(moving, m)
		folderSet[m.FromFolderID] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read move sources: %w", err)
	}
	if len(moving) == 0 {
		return nil, nil
	}
	destFolder, _, err := EnsureFolder(ctx, tx, mailboxID, dest, nil)
	if err != nil {
		return nil, err
	}
	folderSet[destFolder.ID] = true

	// Level 3: every folder involved, ascending.
	folderIDs := make([]int64, 0, len(folderSet))
	for id := range folderSet {
		folderIDs = append(folderIDs, id)
	}
	sort.Slice(folderIDs, func(i, j int) bool { return folderIDs[i] < folderIDs[j] })
	if _, err := tx.Exec(ctx,
		`SELECT id FROM folders WHERE id = ANY($1) ORDER BY id FOR UPDATE`, folderIDs,
	); err != nil {
		return nil, fmt.Errorf("lock folders: %w", err)
	}

	// Level 4: the message rows, ascending.
	moveIDs := make([]int64, len(moving))
	for i, m := range moving {
		moveIDs[i] = m.ID
	}
	sort.Slice(moveIDs, func(i, j int) bool { return moveIDs[i] < moveIDs[j] })
	if _, err := tx.Exec(ctx,
		`SELECT id FROM messages WHERE id = ANY($1) ORDER BY id FOR UPDATE`, moveIDs,
	); err != nil {
		return nil, fmt.Errorf("lock messages: %w", err)
	}

	// Reserve the UID block and modseq range in one statement, as IMAP COPY
	// does, so the destination folder row is updated once.
	n := int64(len(moveIDs))
	var firstUID, firstModSeq int64
	if err := tx.QueryRow(ctx,
		`UPDATE folders
		    SET uidnext        = uidnext + $2,
		        highest_modseq = highest_modseq + $2
		  WHERE id = $1
		 RETURNING uidnext - $2, highest_modseq - $2 + 1`,
		destFolder.ID, n,
	).Scan(&firstUID, &firstModSeq); err != nil {
		return nil, fmt.Errorf("allocate uids: %w", err)
	}
	if err := CheckProtocolID("uid", firstUID); err != nil {
		return nil, err
	}
	if err := CheckProtocolID("uid", firstUID+n-1); err != nil {
		return nil, err
	}

	upd, err := tx.Query(ctx, `
		UPDATE messages m
		   SET folder_id  = $1,
		       uid        = $2 + o.ord,
		       mod_seq    = $3 + o.ord,
		       created_at = now()
		  FROM (SELECT id, row_number() OVER (ORDER BY internal_date, id) - 1 AS ord
		          FROM messages WHERE id = ANY($4)) o
		 WHERE m.id = o.id
		RETURNING m.id, m.uid`,
		destFolder.ID, firstUID, firstModSeq, moveIDs)
	if err != nil {
		return nil, fmt.Errorf("move messages: %w", err)
	}
	newUID := make(map[int64]int64, len(moveIDs))
	for upd.Next() {
		var id, uid int64
		if err := upd.Scan(&id, &uid); err != nil {
			upd.Close()
			return nil, fmt.Errorf("scan moved message: %w", err)
		}
		newUID[id] = uid
	}
	upd.Close()
	if err := upd.Err(); err != nil {
		return nil, fmt.Errorf("move messages: %w", err)
	}
	// The rows were locked above, so every one of them must have moved. A
	// mismatch is a bug in the set arithmetic, and committing it would leave
	// the destination's uidnext advanced past UIDs nobody holds.
	if len(newUID) != len(moveIDs) {
		return nil, fmt.Errorf("move messages: updated %d rows, expected %d", len(newUID), len(moveIDs))
	}

	sources := make([]int64, 0, len(folderIDs))
	for _, id := range folderIDs {
		if id != destFolder.ID {
			sources = append(sources, id)
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE folders SET highest_modseq = highest_modseq + 1 WHERE id = ANY($1)`, sources,
	); err != nil {
		return nil, fmt.Errorf("bump source modseq: %w", err)
	}
	if err := notifyFolders(ctx, tx, folderIDs); err != nil {
		return nil, err
	}

	for i := range moving {
		moving[i].NewUID = newUID[moving[i].ID]
	}
	sort.Slice(moving, func(i, j int) bool { return moving[i].NewUID < moving[j].NewUID })
	return moving, nil
}

// ArchiveMessages is MoveMessages into the \Archive folder for a message a
// user deleted: it also clears the \Deleted mark, so the archived message is
// not destroyed by the next EXPUNGE of the archive and the sorter files it.
// Delete means archive (ARCHIVE_SORTING.md).
func ArchiveMessages(ctx context.Context, tx pgx.Tx, mailboxID int64, items []MoveItem, archiveFolder string) ([]MovedMessage, error) {
	moved, err := MoveMessages(ctx, tx, mailboxID, items, archiveFolder)
	if err != nil || len(moved) == 0 {
		return moved, err
	}
	ids := make([]int64, len(moved))
	for i, m := range moved {
		ids[i] = m.ID
	}
	// MoveMessages stamped each row with a fresh mod_seq in this transaction,
	// so the flag change is covered by it.
	if _, err := tx.Exec(ctx,
		`UPDATE messages SET flags = array_remove(flags, '\Deleted') WHERE id = ANY($1)`, ids,
	); err != nil {
		return nil, fmt.Errorf("clear deleted flag: %w", err)
	}
	return moved, nil
}

// ---- destroying ----

// PurgeResult reports what PurgeMessages destroyed.
type PurgeResult struct {
	Messages int64
	Bytes    int64
	// Copies counts the destroyed rows that were not among the requested ids:
	// other copies of the same content elsewhere in the mailbox.
	Copies int64
}

// PurgeMessages destroys messages of mailboxID: the rows and, by cascade, their
// attachments, annotations, classification and journal entries. With
// allCopies, every other message in the mailbox with the same raw content
// (raw_sha256) goes too, so a message deleted "everywhere" does not survive as
// the copy an import or a COPY left in another folder. The blob files are left
// to `epistula-database gc`, whose mark and sweep reap them once nothing
// references them.
//
// It runs inside the caller's transaction in the canonical lock order —
// mailbox, folders ascending, then the DELETE's own row locks — debits
// used_bytes by the bytes actually deleted, and bumps and announces every
// folder it changed. Ids that no longer exist or belong to another mailbox are
// ignored.
func PurgeMessages(ctx context.Context, tx pgx.Tx, mailboxID int64, ids []int64, allCopies bool) (PurgeResult, error) {
	if len(ids) == 0 {
		return PurgeResult{}, nil
	}
	if err := lockMailboxRow(ctx, tx, mailboxID); err != nil {
		return PurgeResult{}, err
	}

	query := `SELECT m.id, m.folder_id
	            FROM messages m JOIN folders f ON f.id = m.folder_id
	           WHERE f.mailbox_id = $1 AND m.id = ANY($2)`
	if allCopies {
		query = `SELECT m.id, m.folder_id
		           FROM messages m JOIN folders f ON f.id = m.folder_id
		          WHERE f.mailbox_id = $1
		            AND (m.id = ANY($2) OR m.raw_sha256 IN (
		                  SELECT s.raw_sha256
		                    FROM messages s JOIN folders sf ON sf.id = s.folder_id
		                   WHERE sf.mailbox_id = $1 AND s.id = ANY($2)))`
	}
	rows, err := tx.Query(ctx, query, mailboxID, ids)
	if err != nil {
		return PurgeResult{}, fmt.Errorf("read purge targets: %w", err)
	}
	requested := make(map[int64]bool, len(ids))
	for _, id := range ids {
		requested[id] = true
	}
	var targets []int64
	folderSet := map[int64]bool{}
	for rows.Next() {
		var id, folderID int64
		if err := rows.Scan(&id, &folderID); err != nil {
			rows.Close()
			return PurgeResult{}, fmt.Errorf("scan purge target: %w", err)
		}
		targets = append(targets, id)
		folderSet[folderID] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return PurgeResult{}, fmt.Errorf("read purge targets: %w", err)
	}
	if len(targets) == 0 {
		return PurgeResult{}, nil
	}

	folderIDs := make([]int64, 0, len(folderSet))
	for id := range folderSet {
		folderIDs = append(folderIDs, id)
	}
	sort.Slice(folderIDs, func(i, j int) bool { return folderIDs[i] < folderIDs[j] })
	if _, err := tx.Exec(ctx,
		`SELECT id FROM folders WHERE id = ANY($1) ORDER BY id FOR UPDATE`, folderIDs,
	); err != nil {
		return PurgeResult{}, fmt.Errorf("lock folders: %w", err)
	}

	del, err := tx.Query(ctx,
		`DELETE FROM messages WHERE id = ANY($1) RETURNING id, raw_size`, targets)
	if err != nil {
		return PurgeResult{}, fmt.Errorf("delete messages: %w", err)
	}
	var res PurgeResult
	for del.Next() {
		var id, size int64
		if err := del.Scan(&id, &size); err != nil {
			del.Close()
			return PurgeResult{}, fmt.Errorf("scan deleted message: %w", err)
		}
		res.Messages++
		res.Bytes += size
		if !requested[id] {
			res.Copies++
		}
	}
	del.Close()
	if err := del.Err(); err != nil {
		return PurgeResult{}, fmt.Errorf("delete messages: %w", err)
	}
	if res.Messages == 0 {
		return PurgeResult{}, nil
	}
	if res.Bytes > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE mailboxes SET used_bytes = GREATEST(0, used_bytes - $1), updated_at = now()
			  WHERE id = $2`, res.Bytes, mailboxID,
		); err != nil {
			return PurgeResult{}, fmt.Errorf("debit used_bytes: %w", err)
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE folders SET highest_modseq = highest_modseq + 1 WHERE id = ANY($1)`, folderIDs,
	); err != nil {
		return PurgeResult{}, fmt.Errorf("bump modseq: %w", err)
	}
	if err := notifyFolders(ctx, tx, folderIDs); err != nil {
		return PurgeResult{}, err
	}
	return res, nil
}

// ---- pruning ----

// PruneEmptyFolders deletes the mailbox's empty folders that nothing needs:
// no messages, no special-use attribute, not INBOX, no remaining descendant,
// and not an active archive category's folder or one of its ancestors (an
// empty category folder is still somewhere to drag mail to). Deleting a leaf
// can leave its parent with no descendants, so it works bottom-up and returns
// every name it removed, deepest first. With dryRun nothing is written.
//
// It runs in one transaction under the mailbox row lock, which every writer
// that could put a message into a folder also takes, so a folder found empty
// stays empty until it is deleted.
func (db *DB) PruneEmptyFolders(ctx context.Context, mailboxID int64, dryRun bool) ([]string, error) {
	var removed []string
	err := db.runTx(ctx, func(tx pgx.Tx) error {
		removed = nil
		if !dryRun {
			if err := lockMailboxRow(ctx, tx, mailboxID); err != nil {
				return err
			}
		}
		type folder struct {
			id          int64
			name        string
			count       int64
			special     bool
			descendants int
		}
		rows, err := tx.Query(ctx, `
			SELECT f.id, f.name, f.special_use IS NOT NULL,
			       (SELECT count(*) FROM messages m WHERE m.folder_id = f.id)
			  FROM folders f WHERE f.mailbox_id = $1`, mailboxID)
		if err != nil {
			return fmt.Errorf("list folders: %w", err)
		}
		byName := map[string]*folder{}
		for rows.Next() {
			f := &folder{}
			if err := rows.Scan(&f.id, &f.name, &f.special, &f.count); err != nil {
				rows.Close()
				return fmt.Errorf("scan folder: %w", err)
			}
			byName[f.name] = f
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("list folders: %w", err)
		}

		protected := map[string]bool{"INBOX": true}
		catRows, err := tx.Query(ctx,
			`SELECT folder FROM archive_categories WHERE mailbox_id = $1 AND retired_at IS NULL`, mailboxID)
		if err != nil {
			return fmt.Errorf("list category folders: %w", err)
		}
		for catRows.Next() {
			var name string
			if err := catRows.Scan(&name); err != nil {
				catRows.Close()
				return fmt.Errorf("scan category folder: %w", err)
			}
			protected[name] = true
			for _, a := range FolderAncestors(name) {
				protected[a] = true
			}
		}
		catRows.Close()
		if err := catRows.Err(); err != nil {
			return fmt.Errorf("list category folders: %w", err)
		}

		// Count descendants rather than direct children, so a folder whose
		// intermediate parent row is missing (an import that predates
		// OPS-001) still protects every ancestor above it.
		for name := range byName {
			for _, a := range FolderAncestors(name) {
				if anc, ok := byName[a]; ok {
					anc.descendants++
				}
			}
		}
		names := make([]string, 0, len(byName))
		for name := range byName {
			names = append(names, name)
		}
		// Deepest first, then by name, so a parent is only considered after
		// every child that could still be removed.
		sort.Slice(names, func(i, j int) bool {
			di, dj := strings.Count(names[i], "/"), strings.Count(names[j], "/")
			if di != dj {
				return di > dj
			}
			return names[i] < names[j]
		})
		var ids []int64
		for _, name := range names {
			f := byName[name]
			if f.count > 0 || f.special || f.descendants > 0 || protected[name] {
				continue
			}
			removed = append(removed, name)
			ids = append(ids, f.id)
			for _, a := range FolderAncestors(name) {
				if anc, ok := byName[a]; ok {
					anc.descendants--
				}
			}
		}
		if dryRun || len(ids) == 0 {
			return nil
		}
		// The folder rows are locked in ascending id order before the delete,
		// and the delete re-checks emptiness itself, so a message that arrived
		// by a writer that skipped the mailbox lock is never cascaded away.
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		if _, err := tx.Exec(ctx,
			`SELECT id FROM folders WHERE id = ANY($1) ORDER BY id FOR UPDATE`, ids,
		); err != nil {
			return fmt.Errorf("lock folders: %w", err)
		}
		tag, err := tx.Exec(ctx,
			`DELETE FROM folders f
			  WHERE f.id = ANY($1)
			    AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.folder_id = f.id)`, ids)
		if err != nil {
			return fmt.Errorf("delete folders: %w", err)
		}
		if tag.RowsAffected() != int64(len(ids)) {
			return fmt.Errorf("delete folders: removed %d of %d; a folder gained a message, nothing was changed", tag.RowsAffected(), len(ids))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return removed, nil
}

// notifyFolders announces a change to each folder on mail_arrived, the channel
// epistula-imap's IDLE sessions listen on. The notification is delivered at
// commit, and a failure aborts the transaction, so it is returned rather than
// logged: a move that commits without waking IDLE clients is still correct,
// but one that fails here would not commit at all.
func notifyFolders(ctx context.Context, tx pgx.Tx, folderIDs []int64) error {
	for _, id := range folderIDs {
		if _, err := tx.Exec(ctx,
			`SELECT pg_notify('mail_arrived', $1)`, strconv.FormatInt(id, 10),
		); err != nil {
			return fmt.Errorf("notify folder %d: %w", id, err)
		}
	}
	return nil
}
