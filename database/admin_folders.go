package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ptudor/epistula-mail/database/storage"
)

// adminFolderRepairAncestors creates the missing hierarchical ancestors of
// existing folders (OPS-001).
//
// Until OPS-001 the writer's ingest path created only the leaf, so a Maildir++
// import into `Sent/2004/11-Nov` left no `Sent/2004` row and clients listed a
// child whose parent did not exist. Folders created from now on get their
// ancestors with them; this repairs the rows written before that. It is safe to
// re-run — when nothing is missing it changes nothing — and -dry-run lists what
// it would create without locking or writing.
func adminFolderRepairAncestors(args []string) int {
	fs := flag.NewFlagSet("admin folder-repair-ancestors", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	mailbox := fs.String("mailbox", "", "Mailbox to repair")
	all := fs.Bool("all", false, "Repair every mailbox")
	dryRun := fs.Bool("dry-run", false, "List the folders that would be created, without creating them")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	// Match the canonical stored name (lower + trimmed), as every other
	// mailbox-taking subcommand does (R-048).
	*mailbox = strings.ToLower(strings.TrimSpace(*mailbox))
	if (*mailbox == "") == !*all {
		fmt.Fprintln(os.Stderr, "folder-repair-ancestors: pass exactly one of -mailbox or -all")
		return EX_USAGE
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return EX_CONFIG
	}
	db, code := adminOpenDB(cfg)
	if code != EX_OK {
		return code
	}
	defer db.Close()

	type target struct {
		id   int64
		name string
	}
	var targets []target
	listCtx, cancelList := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelList()
	if *all {
		rows, err := db.Pool().Query(listCtx, `SELECT id, name FROM mailboxes ORDER BY name`)
		if err != nil {
			fmt.Fprintf(os.Stderr, "list mailboxes: %v\n", err)
			return EX_TEMPFAIL
		}
		for rows.Next() {
			var t target
			if err := rows.Scan(&t.id, &t.name); err != nil {
				rows.Close()
				fmt.Fprintf(os.Stderr, "scan mailbox: %v\n", err)
				return EX_TEMPFAIL
			}
			targets = append(targets, t)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "list mailboxes: %v\n", err)
			return EX_TEMPFAIL
		}
	} else {
		id, err := db.LookupMailboxByName(listCtx, *mailbox)
		if errors.Is(err, storage.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "mailbox %q not found\n", *mailbox)
			return EX_USAGE
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "lookup mailbox: %v\n", err)
			return EX_TEMPFAIL
		}
		targets = append(targets, target{id: id, name: *mailbox})
	}

	verb := "created"
	if *dryRun {
		verb = "would create"
	}
	var total int
	for _, t := range targets {
		// Each mailbox is its own transaction under its own row lock, held
		// only for as long as that mailbox's repair takes.
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		names, err := db.RepairFolderAncestors(ctx, t.id, *dryRun)
		cancel()
		switch {
		case errors.Is(err, storage.ErrNotFound):
			fmt.Fprintf(os.Stderr, "mailbox %q no longer exists; skipped\n", t.name)
			continue
		case err != nil:
			fmt.Fprintf(os.Stderr, "mailbox %q: %v\n", t.name, err)
			return EX_TEMPFAIL
		}
		if len(names) == 0 {
			fmt.Printf("mailbox %s: no missing ancestor folders\n", t.name)
			continue
		}
		fmt.Printf("mailbox %s: %s %d missing ancestor folder(s):\n", t.name, verb, len(names))
		for _, n := range names {
			fmt.Printf("  %s\n", n)
		}
		total += len(names)
	}
	if *all {
		fmt.Printf("%d mailbox(es) checked; %s %d folder(s)\n", len(targets), verb, total)
	}
	if *dryRun {
		fmt.Println("dry run: nothing was written")
	}
	return EX_OK
}

// adminFolderSetSpecialUse assigns or clears one folder's RFC 6154 special-use
// attribute (OPS-002).
//
// folders.special_use — the column LIST reports — was only ever set by an IMAP
// `CREATE ... (USE (...))`, so a folder that arrived by delivery or import could
// never carry one, and clients showed an imported Sent or Drafts as an ordinary
// folder. This is the operator's way to say which folder plays which role. It
// accepts exactly the attributes CREATE does, stores the canonical spelling,
// and gives each attribute to at most one folder per mailbox.
func adminFolderSetSpecialUse(args []string) int {
	fs := flag.NewFlagSet("admin folder-set-special-use", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	mailbox := fs.String("mailbox", "", "Mailbox name (required)")
	folder := fs.String("folder", "", "Folder name, exactly as folder-list prints it (required)")
	use := fs.String("use", "", "Attribute to assign, one of: "+
		strings.Join(storage.SpecialUseAttributes(), " ")+" (the leading backslash is optional)")
	clearUse := fs.Bool("clear", false, "Remove the folder's special-use attribute")
	dryRun := fs.Bool("dry-run", false, "Report the change without writing it")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	*mailbox = strings.ToLower(strings.TrimSpace(*mailbox))
	if *mailbox == "" || *folder == "" {
		fmt.Fprintln(os.Stderr, "folder-set-special-use: -mailbox and -folder are required")
		return EX_USAGE
	}
	if (*use != "") == *clearUse {
		fmt.Fprintln(os.Stderr, "folder-set-special-use: pass exactly one of -use or -clear")
		return EX_USAGE
	}
	attr := ""
	if *use != "" {
		// A bare `-use Sent` saves the operator quoting a backslash through
		// the shell; either way the stored value is the canonical `\Sent`.
		a := strings.TrimSpace(*use)
		if !strings.HasPrefix(a, `\`) {
			a = `\` + a
		}
		canonical, ok := storage.CanonicalSpecialUse(a)
		if !ok {
			// %s, not %q: quoting would print the backslash doubled.
			fmt.Fprintf(os.Stderr, "folder-set-special-use: unsupported attribute %s; use one of: %s\n",
				a, strings.Join(storage.SpecialUseAttributes(), " "))
			return EX_USAGE
		}
		attr = canonical
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return EX_CONFIG
	}
	db, code := adminOpenDB(cfg)
	if code != EX_OK {
		return code
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mailboxID, err := db.LookupMailboxByName(ctx, *mailbox)
	if errors.Is(err, storage.ErrNotFound) {
		fmt.Fprintf(os.Stderr, "mailbox %q not found\n", *mailbox)
		return EX_USAGE
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "lookup mailbox: %v\n", err)
		return EX_TEMPFAIL
	}

	change, err := db.SetFolderSpecialUse(ctx, mailboxID, *folder, attr, *dryRun)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		fmt.Fprintf(os.Stderr, "%v (folder names are exact and case-sensitive; see folder-list -mailbox %s)\n",
			err, *mailbox)
		return EX_USAGE
	case errors.Is(err, storage.ErrSpecialUseTaken):
		fmt.Fprintf(os.Stderr, "%v\nEach attribute belongs to one folder per mailbox: "+
			"clear it there first with -clear, then assign it here.\n", err)
		return EX_USAGE
	case err != nil:
		fmt.Fprintf(os.Stderr, "set special-use: %v\n", err)
		return EX_TEMPFAIL
	}

	show := func(a string) string {
		if a == "" {
			return "none"
		}
		return a
	}
	switch {
	case !change.Changed():
		fmt.Printf("mailbox %s folder %q: special-use is already %s; no change\n",
			*mailbox, *folder, show(change.After))
	case *dryRun:
		fmt.Printf("mailbox %s folder %q: would change special-use from %s to %s\n",
			*mailbox, *folder, show(change.Before), show(change.After))
		fmt.Println("dry run: nothing was written")
	default:
		fmt.Printf("mailbox %s folder %q: special-use changed from %s to %s\n",
			*mailbox, *folder, show(change.Before), show(change.After))
	}
	if change.After == `\Trash` && change.Changed() {
		// The Trash purge (epistula-imap [archive] purge_enabled) destroys
		// whatever sits in this folder past the retention period, including
		// anything that was already there.
		var n int64
		if err := db.Pool().QueryRow(ctx,
			`SELECT count(*) FROM messages m JOIN folders f ON f.id = m.folder_id
			  WHERE f.mailbox_id = $1 AND f.name = $2`, mailboxID, *folder,
		).Scan(&n); err != nil {
			fmt.Fprintf(os.Stderr, "count messages in %q: %v\n", *folder, err)
			return EX_TEMPFAIL
		}
		fmt.Printf("note: with epistula-imap's Trash purge enabled, the %d message(s) in %q "+
			"(and every message moved there later) are destroyed after the retention period\n", n, *folder)
	}
	return EX_OK
}

// adminFolderCreate creates a folder, with its missing ancestors and an
// optional special-use attribute, without the mailbox password IMAP CREATE
// would need. Archive sorting needs a mailbox to have \Archive and \Trash
// folders; a mailbox that only ever received mail has neither.
func adminFolderCreate(args []string) int {
	fs := flag.NewFlagSet("admin folder-create", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	mailbox := fs.String("mailbox", "", "Mailbox name (required)")
	folder := fs.String("folder", "", "Folder name, with / between levels (required)")
	use := fs.String("use", "", "Special-use attribute for the new folder, one of: "+
		strings.Join(storage.SpecialUseAttributes(), " ")+" (the leading backslash is optional)")
	dryRun := fs.Bool("dry-run", false, "List what would be created without creating it")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	*mailbox = strings.ToLower(strings.TrimSpace(*mailbox))
	if *mailbox == "" || *folder == "" {
		fmt.Fprintln(os.Stderr, "folder-create: -mailbox and -folder are required")
		return EX_USAGE
	}
	attr := ""
	if *use != "" {
		a := strings.TrimSpace(*use)
		if !strings.HasPrefix(a, `\`) {
			a = `\` + a
		}
		canonical, ok := storage.CanonicalSpecialUse(a)
		if !ok {
			fmt.Fprintf(os.Stderr, "folder-create: unsupported attribute %s; use one of: %s\n",
				a, strings.Join(storage.SpecialUseAttributes(), " "))
			return EX_USAGE
		}
		attr = canonical
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return EX_CONFIG
	}
	db, code := adminOpenDB(cfg)
	if code != EX_OK {
		return code
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mailboxID, err := db.LookupMailboxByName(ctx, *mailbox)
	if errors.Is(err, storage.ErrNotFound) {
		fmt.Fprintf(os.Stderr, "mailbox %q not found\n", *mailbox)
		return EX_USAGE
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "lookup mailbox: %v\n", err)
		return EX_TEMPFAIL
	}

	res, err := db.CreateFolder(ctx, mailboxID, *folder, attr, *dryRun)
	switch {
	case errors.Is(err, storage.ErrInvalidFolderName):
		fmt.Fprintf(os.Stderr, "folder-create: %v\n", err)
		return EX_USAGE
	case errors.Is(err, storage.ErrSpecialUseTaken):
		fmt.Fprintf(os.Stderr, "folder-create: %v\nClear it there with folder-set-special-use -clear first.\n", err)
		return EX_USAGE
	case err != nil:
		fmt.Fprintf(os.Stderr, "folder-create: %v\n", err)
		return EX_TEMPFAIL
	}
	if res.Existed {
		fmt.Printf("mailbox %s: folder %q already exists; left unchanged (folder-set-special-use changes its role)\n", *mailbox, *folder)
		return EX_OK
	}
	verb := "created"
	if *dryRun {
		verb = "would create"
	}
	for _, n := range res.Created {
		label := ""
		if n == *folder && attr != "" {
			label = " " + attr
		}
		fmt.Printf("mailbox %s: %s folder %q%s\n", *mailbox, verb, n, label)
	}
	if *dryRun {
		fmt.Println("dry run: nothing was written")
	}
	return EX_OK
}
