package storage

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Folder creation and folder metadata, shared by every writer of the schema.
//
// Folder rows are created in four places: delivery and import (Ingest), IMAP
// CREATE, IMAP COPY/MOVE auto-create, and IMAP RENAME's destination. They
// disagreed about the hierarchy. IMAP CREATE and COPY/MOVE inserted missing
// ancestors (RO5X-008) while Ingest and LookupOrCreateFolder inserted only the
// leaf, so `epistula-database import -folder Sent/2004/11-Nov` produced a child
// whose parent LIST never returns, and RENAME onto a nested name did the same
// (OPS-001). Every creation now goes through EnsureFolder or
// EnsureFolderAncestors, so there is one definition of what creating a folder
// means, and it lives with the schema.

// ErrUnknownSpecialUse is returned for a special-use attribute the schema does
// not model (see SpecialUseAttributes).
var ErrUnknownSpecialUse = errors.New("storage: unsupported special-use attribute")

// ErrSpecialUseTaken is returned by SetFolderSpecialUse when another folder in
// the same mailbox already carries the requested attribute.
var ErrSpecialUseTaken = errors.New("storage: special-use attribute is already assigned to another folder")

// Folder identifies a folder row: its durable id and the UIDVALIDITY
// generation it was read or created with.
type Folder struct {
	ID          int64
	UIDValidity int64
}

// FolderAncestors returns every proper hierarchical ancestor of name,
// outermost first: "a/b/c" yields ["a", "a/b"]. A name with no delimiter has
// none. The store's hierarchy delimiter is "/", the one epistula-imap
// advertises in LIST and NAMESPACE.
//
// Empty segments (a leading, trailing, or doubled "/") produce no ancestor —
// "" is not a creatable mailbox name, and emitting it would attempt an
// unnamed folder row.
func FolderAncestors(name string) []string {
	var out []string
	for i := 0; i < len(name); i++ {
		if name[i] != '/' {
			continue
		}
		// Skip an empty prefix (leading "/") and one that itself ends in the
		// delimiter (a doubled "//"), so neither produces an unnamed or
		// trailing-slash folder row.
		prefix := name[:i]
		if prefix == "" || prefix[len(prefix)-1] == '/' {
			continue
		}
		out = append(out, prefix)
	}
	return out
}

// EnsureFolder returns the folder called name in mailboxID, creating it —
// after every missing hierarchical ancestor — when it does not exist. created
// reports whether this call inserted the leaf; IMAP CREATE maps false to
// ALREADYEXISTS.
//
// specialUse, when non-nil, is stored on the leaf only, and only when this call
// creates it: an existing folder keeps its attribute, and ancestors are created
// plain, exactly as a CREATE of each would make them. It must be one of
// SpecialUseAttributes and is stored in canonical spelling.
//
// The caller's transaction must already hold the mailbox row FOR UPDATE —
// level 1 of the canonical lock order (epistula-imap lockorder.go). That lock
// is what makes "every folder's ancestors exist" hold under concurrency: IMAP
// DELETE takes it before refusing a folder with inferiors, so without it a
// DELETE of `a` could commit between this call finding `a` present and
// inserting `a/b`, leaving `a/b` parentless.
//
// UIDVALIDITY for every row comes from mail_next_uidvalidity(), not the clock,
// so a same-second delete and recreate cannot reuse a value (R-062). Each
// INSERT is guarded by WHERE NOT EXISTS so that function runs only when a row
// is genuinely created: an unconditional `INSERT ... ON CONFLICT DO NOTHING`
// still evaluates it, which tied sequence consumption to message traffic
// (RA6X-052). ON CONFLICT stays as a backstop for a writer that does not hold
// the mailbox lock.
func EnsureFolder(ctx context.Context, tx pgx.Tx, mailboxID int64, name string, specialUse *string) (Folder, bool, error) {
	if name == "" {
		return Folder{}, false, errors.New("storage.EnsureFolder: folder name is required")
	}
	if specialUse != nil {
		canonical, ok := CanonicalSpecialUse(*specialUse)
		if !ok {
			return Folder{}, false, fmt.Errorf("%w: %q", ErrUnknownSpecialUse, *specialUse)
		}
		specialUse = &canonical
	}

	// The common case — delivery into a folder that exists — costs one lookup
	// and never touches the ancestors.
	f, err := lookupFolderTx(ctx, tx, mailboxID, name)
	if err == nil {
		return f, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Folder{}, false, err
	}

	if _, err := EnsureFolderAncestors(ctx, tx, mailboxID, name); err != nil {
		return Folder{}, false, err
	}
	err = tx.QueryRow(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext, special_use)
		 SELECT $1, $2, mail_next_uidvalidity(), 1, $3
		  WHERE NOT EXISTS (SELECT 1 FROM folders WHERE mailbox_id = $1 AND name = $2)
		 ON CONFLICT (mailbox_id, name) DO NOTHING
		 RETURNING id, uidvalidity`,
		mailboxID, name, specialUse,
	).Scan(&f.ID, &f.UIDValidity)
	if err == nil {
		return f, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Folder{}, false, fmt.Errorf("create folder %q: %w", name, err)
	}
	// A writer that does not hold the mailbox lock created it between the
	// lookup and the insert. It exists, but this call did not create it.
	f, err = lookupFolderTx(ctx, tx, mailboxID, name)
	if err != nil {
		return Folder{}, false, fmt.Errorf("re-read folder %q: %w", name, err)
	}
	return f, false, nil
}

// EnsureFolderAncestors creates every missing proper ancestor of name,
// outermost first, and returns the names it created. name itself is not
// touched: IMAP RENAME uses this for its destination, whose leaf row is the
// folder being renamed. The lock precondition is EnsureFolder's.
func EnsureFolderAncestors(ctx context.Context, tx pgx.Tx, mailboxID int64, name string) ([]string, error) {
	return createFoldersIfAbsent(ctx, tx, mailboxID, FolderAncestors(name))
}

// RepairFolderAncestors creates the missing hierarchical ancestors of every
// folder in mailboxID and returns their names in creation order. The order is
// byte order, which puts every ancestor before its descendants because a
// proper prefix sorts first.
//
// It exists for folders written before OPS-001, when Ingest created only the
// leaf, and it is idempotent: once every ancestor exists there is nothing to
// do and no UIDVALIDITY is consumed. With dryRun it reports what it would
// create from a single read, taking no lock.
func (db *DB) RepairFolderAncestors(ctx context.Context, mailboxID int64, dryRun bool) ([]string, error) {
	if dryRun {
		names, err := folderNames(ctx, db.pool, mailboxID)
		if err != nil {
			return nil, err
		}
		return missingAncestors(names), nil
	}
	var created []string
	err := db.runTx(ctx, func(tx pgx.Tx) error {
		if err := lockMailboxRow(ctx, tx, mailboxID); err != nil {
			return err
		}
		names, err := folderNames(ctx, tx, mailboxID)
		if err != nil {
			return err
		}
		created, err = createFoldersIfAbsent(ctx, tx, mailboxID, missingAncestors(names))
		return err
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// createFoldersIfAbsent inserts each name that does not already exist, in the
// order given, and returns the ones it inserted. Callers pass ancestors before
// descendants.
func createFoldersIfAbsent(ctx context.Context, tx pgx.Tx, mailboxID int64, names []string) ([]string, error) {
	var created []string
	for _, name := range names {
		tag, err := tx.Exec(ctx,
			`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
			 SELECT $1, $2, mail_next_uidvalidity(), 1
			  WHERE NOT EXISTS (SELECT 1 FROM folders WHERE mailbox_id = $1 AND name = $2)
			 ON CONFLICT (mailbox_id, name) DO NOTHING`,
			mailboxID, name,
		)
		if err != nil {
			return nil, fmt.Errorf("create folder %q: %w", name, err)
		}
		if tag.RowsAffected() == 1 {
			created = append(created, name)
		}
	}
	return created, nil
}

// missingAncestors returns, sorted, every ancestor of the given folder names
// that is not itself one of them.
func missingAncestors(names []string) []string {
	have := make(map[string]bool, len(names))
	for _, n := range names {
		have[n] = true
	}
	want := map[string]bool{}
	for _, n := range names {
		for _, a := range FolderAncestors(n) {
			if !have[a] {
				want[a] = true
			}
		}
	}
	out := make([]string, 0, len(want))
	for a := range want {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// folderQuerier is satisfied by both the pool and a transaction, so the
// dry-run read and the locked read share one query.
type folderQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// folderNames returns every folder name in the mailbox.
func folderNames(ctx context.Context, q folderQuerier, mailboxID int64) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT name FROM folders WHERE mailbox_id = $1`, mailboxID)
	if err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan folder: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	return names, nil
}

// lookupFolderTx returns the folder called name, or ErrNotFound.
func lookupFolderTx(ctx context.Context, tx pgx.Tx, mailboxID int64, name string) (Folder, error) {
	var f Folder
	err := tx.QueryRow(ctx,
		`SELECT id, uidvalidity FROM folders WHERE mailbox_id = $1 AND name = $2`,
		mailboxID, name,
	).Scan(&f.ID, &f.UIDValidity)
	if errors.Is(err, pgx.ErrNoRows) {
		return Folder{}, ErrNotFound
	}
	if err != nil {
		return Folder{}, fmt.Errorf("folder lookup: %w", err)
	}
	return f, nil
}

// lockMailboxRow takes level 1 of the canonical lock order, the mailbox row
// FOR UPDATE, and reports ErrNotFound when the mailbox no longer exists.
func lockMailboxRow(ctx context.Context, tx pgx.Tx, mailboxID int64) error {
	var id int64
	err := tx.QueryRow(ctx,
		`SELECT id FROM mailboxes WHERE id = $1 FOR UPDATE`, mailboxID,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: mailbox %d", ErrNotFound, mailboxID)
	}
	if err != nil {
		return fmt.Errorf("lock mailbox: %w", err)
	}
	return nil
}

// specialUseAttributes is everything folders.special_use may hold: the RFC
// 6154 attributes plus RFC 8457's \Important, as the schema comment on the
// column enumerates. LIST emits the column verbatim, so nothing outside this
// set may be stored — IMAP CREATE ... (USE (...)) and the admin CLI both check
// against it.
var specialUseAttributes = []string{
	`\All`, `\Archive`, `\Drafts`, `\Flagged`, `\Important`, `\Junk`, `\Sent`, `\Trash`,
}

// SpecialUseAttributes returns the modelled special-use attributes in their
// canonical spelling.
func SpecialUseAttributes() []string {
	return append([]string(nil), specialUseAttributes...)
}

// CanonicalSpecialUse returns the canonical spelling of attr and whether the
// schema models it. Matching is case-insensitive: RFC 6154 §6 defines the
// attributes as ABNF literals, and ABNF literals are case-insensitive (RFC 5234
// §2.3), so `\sent` is `\Sent`. Storing one spelling keeps LIST output
// canonical.
func CanonicalSpecialUse(attr string) (string, bool) {
	for _, a := range specialUseAttributes {
		if strings.EqualFold(a, attr) {
			return a, true
		}
	}
	return "", false
}

// SpecialUseChange reports what SetFolderSpecialUse found and did. An empty
// attribute means none.
type SpecialUseChange struct {
	Before string
	After  string
}

// Changed reports whether the folder's attribute differs after the call.
func (c SpecialUseChange) Changed() bool { return c.Before != c.After }

// SetFolderSpecialUse sets the RFC 6154 special-use attribute of one folder,
// or clears it when attr is "" (OPS-002).
//
// IMAP CREATE ... (USE (...)) was the only way to set folders.special_use, so a
// folder that arrived any other way — delivered or imported — could never
// carry one, and clients showed an imported Sent or Drafts as an ordinary
// folder. Which folder should carry an attribute is an operator decision, not
// a naming heuristic: an imported tree can hold "Sent", "Sent Messages" and
// "sent-mail" at once.
//
// attr is canonicalized; one outside SpecialUseAttributes is refused with
// ErrUnknownSpecialUse. A folder that does not exist is ErrNotFound. Giving an
// attribute to a second folder of the same mailbox is refused with
// ErrSpecialUseTaken: RFC 6154 does not forbid it and IMAP CREATE does not
// prevent it, but a client shown two \Sent folders picks one unpredictably, so
// moving an attribute is made explicit — clear it where it is, then set it
// where it belongs. The mailbox row lock serializes two such commands.
//
// With dryRun it reports the change it would make, including a refusal,
// without locking or writing anything.
func (db *DB) SetFolderSpecialUse(ctx context.Context, mailboxID int64, folder, attr string, dryRun bool) (SpecialUseChange, error) {
	if attr != "" {
		canonical, ok := CanonicalSpecialUse(attr)
		if !ok {
			return SpecialUseChange{}, fmt.Errorf("%w: %q", ErrUnknownSpecialUse, attr)
		}
		attr = canonical
	}
	var change SpecialUseChange
	err := db.runTx(ctx, func(tx pgx.Tx) error {
		if !dryRun {
			if err := lockMailboxRow(ctx, tx, mailboxID); err != nil {
				return err
			}
		}
		var (
			folderID int64
			current  *string
		)
		if err := tx.QueryRow(ctx,
			`SELECT id, special_use FROM folders WHERE mailbox_id = $1 AND name = $2`,
			mailboxID, folder,
		).Scan(&folderID, &current); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: folder %q", ErrNotFound, folder)
			}
			return fmt.Errorf("folder lookup: %w", err)
		}
		change = SpecialUseChange{After: attr}
		if current != nil {
			change.Before = *current
		}
		if !change.Changed() {
			return nil
		}
		if attr != "" {
			var holder string
			err := tx.QueryRow(ctx,
				`SELECT name FROM folders
				  WHERE mailbox_id = $1 AND id <> $2 AND lower(special_use) = lower($3)
				  ORDER BY name LIMIT 1`,
				mailboxID, folderID, attr,
			).Scan(&holder)
			switch {
			case err == nil:
				return fmt.Errorf("%w: %s is on folder %q", ErrSpecialUseTaken, attr, holder)
			case !errors.Is(err, pgx.ErrNoRows):
				return fmt.Errorf("special-use check: %w", err)
			}
		}
		if dryRun {
			return nil
		}
		var value *string
		if attr != "" {
			value = &attr
		}
		if _, err := tx.Exec(ctx,
			`UPDATE folders SET special_use = $1 WHERE id = $2`, value, folderID,
		); err != nil {
			return fmt.Errorf("update special_use: %w", err)
		}
		return nil
	})
	if err != nil {
		return SpecialUseChange{}, err
	}
	return change, nil
}

// FolderCreation reports what CreateFolder did (or, dry, would do).
type FolderCreation struct {
	// Created lists the folders created, ancestors first.
	Created []string
	// Existed is true when the folder was already there; it is left as it
	// is, special-use included (folder-set-special-use changes that).
	Existed bool
}

// CreateFolder creates the folder called name in mailboxID, with its missing
// ancestors, giving it the special-use attribute attr ("" for none). It is the
// operator's IMAP CREATE: it needs no mailbox password and goes through the
// same EnsureFolder every other creation path uses.
//
// The name must pass ValidateFolderName and may not be INBOX, which every
// mailbox already has. An attribute another folder already carries is
// refused with ErrSpecialUseTaken, as SetFolderSpecialUse does. With dryRun it
// reports what it would create without locking or writing.
func (db *DB) CreateFolder(ctx context.Context, mailboxID int64, name, attr string, dryRun bool) (FolderCreation, error) {
	if err := ValidateFolderName(name); err != nil {
		return FolderCreation{}, err
	}
	if strings.EqualFold(name, "INBOX") || strings.HasPrefix(strings.ToUpper(name), "INBOX/") {
		return FolderCreation{}, fmt.Errorf("%w: %q is INBOX or inside it", ErrInvalidFolderName, name)
	}
	var specialUse *string
	if attr != "" {
		canonical, ok := CanonicalSpecialUse(attr)
		if !ok {
			return FolderCreation{}, fmt.Errorf("%w: %q", ErrUnknownSpecialUse, attr)
		}
		specialUse = &canonical
	}
	var out FolderCreation
	err := db.runTx(ctx, func(tx pgx.Tx) error {
		out = FolderCreation{}
		if !dryRun {
			if err := lockMailboxRow(ctx, tx, mailboxID); err != nil {
				return err
			}
		}
		switch _, err := lookupFolderTx(ctx, tx, mailboxID, name); {
		case err == nil:
			out.Existed = true
			return nil
		case !errors.Is(err, ErrNotFound):
			return err
		}
		if specialUse != nil {
			var holder string
			err := tx.QueryRow(ctx,
				`SELECT name FROM folders WHERE mailbox_id = $1 AND lower(special_use) = lower($2)
				  ORDER BY name LIMIT 1`, mailboxID, *specialUse,
			).Scan(&holder)
			switch {
			case err == nil:
				return fmt.Errorf("%w: %s is on folder %q", ErrSpecialUseTaken, *specialUse, holder)
			case !errors.Is(err, pgx.ErrNoRows):
				return fmt.Errorf("special-use check: %w", err)
			}
		}
		if dryRun {
			names, err := folderNames(ctx, tx, mailboxID)
			if err != nil {
				return err
			}
			have := map[string]bool{}
			for _, n := range names {
				have[n] = true
			}
			for _, a := range FolderAncestors(name) {
				if !have[a] {
					out.Created = append(out.Created, a)
				}
			}
			out.Created = append(out.Created, name)
			return nil
		}
		created, err := EnsureFolderAncestors(ctx, tx, mailboxID, name)
		if err != nil {
			return err
		}
		if _, made, err := EnsureFolder(ctx, tx, mailboxID, name, specialUse); err != nil {
			return err
		} else if made {
			created = append(created, name)
		}
		out.Created = created
		return nil
	})
	if err != nil {
		return FolderCreation{}, err
	}
	return out, nil
}
