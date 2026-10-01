package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/ptudor/epistula-mail/database/archive"
	"github.com/ptudor/epistula-mail/database/storage"
)

// Archive sorting operator commands (ARCHIVE_SORTING.md at the repository
// root): the category list the classifier chooses from, and the one-time
// reorganization of an imported archive — plan, apply, undo — plus pruning the
// folders it empties. The live sorter and the Trash purge run inside
// epistula-imap; nothing here runs on a timer.

// adminArchiveMailbox opens the database and resolves the -mailbox flag, the
// preamble every archive command shares.
func adminArchiveMailbox(configPath, mailbox string) (*storage.DB, int64, int) {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return nil, 0, EX_CONFIG
	}
	setupLogging(cfg)
	db, code := adminOpenDB(cfg)
	if code != EX_OK {
		return nil, 0, code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	id, err := db.LookupMailboxByName(ctx, mailbox)
	if errors.Is(err, storage.ErrNotFound) {
		db.Close()
		fmt.Fprintf(os.Stderr, "mailbox %q not found\n", mailbox)
		return nil, 0, EX_USAGE
	}
	if err != nil {
		db.Close()
		fmt.Fprintf(os.Stderr, "lookup mailbox: %v\n", err)
		return nil, 0, EX_TEMPFAIL
	}
	return db, id, EX_OK
}

func adminArchiveCategoryImport(args []string) int {
	fs := flag.NewFlagSet("admin archive-category-import", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	mailbox := fs.String("mailbox", "", "Mailbox name (required)")
	file := fs.String("file", "", "Category list TOML file (required)")
	dryRun := fs.Bool("dry-run", false, "Report the changes without writing them")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	*mailbox = strings.ToLower(strings.TrimSpace(*mailbox))
	if *mailbox == "" || *file == "" {
		fmt.Fprintln(os.Stderr, "archive-category-import: -mailbox and -file are required")
		return EX_USAGE
	}
	f, err := os.Open(*file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "archive-category-import: %v\n", err)
		return EX_USAGE
	}
	cats, err := archive.ParseTaxonomy(bufio.NewReader(f))
	f.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "archive-category-import: %v\n", err)
		return EX_DATAERR
	}

	db, mailboxID, code := adminArchiveMailbox(*configPath, *mailbox)
	if code != EX_OK {
		return code
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	changes, err := db.ReplaceArchiveCategories(ctx, mailboxID, cats, *dryRun)
	switch {
	case errors.Is(err, storage.ErrNoArchiveFolder):
		fmt.Fprintf(os.Stderr, "mailbox %s has no \\Archive folder; give one with folder-set-special-use -use Archive first\n", *mailbox)
		return EX_USAGE
	case errors.Is(err, storage.ErrInvalidArchiveCategory):
		fmt.Fprintf(os.Stderr, "archive-category-import: %v\n", err)
		return EX_DATAERR
	case err != nil:
		fmt.Fprintf(os.Stderr, "archive-category-import: %v\n", err)
		return EX_TEMPFAIL
	}
	verb := ""
	if *dryRun {
		verb = "would be "
	}
	fmt.Printf("mailbox %s, archive folder %q: %d categories listed\n", *mailbox, changes.ArchiveRoot, len(cats))
	for _, group := range []struct {
		label string
		keys  []string
	}{{"added", changes.Added}, {"updated", changes.Updated}, {"retired", changes.Retired}} {
		for _, k := range group.keys {
			fmt.Printf("  %s%s: %s\n", verb, group.label, k)
		}
	}
	fmt.Printf("%d unchanged\n", len(changes.Unchanged))
	if *dryRun {
		fmt.Println("dry run: nothing was written")
	}
	return EX_OK
}

func adminArchiveCategoryList(args []string) int {
	fs := flag.NewFlagSet("admin archive-category-list", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	mailbox := fs.String("mailbox", "", "Mailbox name (required)")
	all := fs.Bool("all", false, "Include retired categories")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	*mailbox = strings.ToLower(strings.TrimSpace(*mailbox))
	if *mailbox == "" {
		fmt.Fprintln(os.Stderr, "archive-category-list: -mailbox is required")
		return EX_USAGE
	}
	db, mailboxID, code := adminArchiveMailbox(*configPath, *mailbox)
	if code != EX_OK {
		return code
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cats, err := db.ListArchiveCategories(ctx, mailboxID, *all)
	if err != nil {
		fmt.Fprintf(os.Stderr, "archive-category-list: %v\n", err)
		return EX_TEMPFAIL
	}
	counts := map[string]int64{}
	rows, err := db.Pool().Query(ctx, `
		SELECT c.category, count(*)
		  FROM message_classifications c
		  JOIN messages m ON m.id = c.message_id
		  JOIN folders f ON f.id = m.folder_id
		 WHERE f.mailbox_id = $1
		 GROUP BY c.category`, mailboxID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "archive-category-list: count classifications: %v\n", err)
		return EX_TEMPFAIL
	}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			rows.Close()
			fmt.Fprintf(os.Stderr, "archive-category-list: %v\n", err)
			return EX_TEMPFAIL
		}
		counts[k] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "archive-category-list: %v\n", err)
		return EX_TEMPFAIL
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tFOLDER\tANNUAL\tCLASSIFIED\tSTATE\tDESCRIPTION")
	for _, c := range cats {
		state := "active"
		if c.Retired {
			state = "retired"
		}
		annual := "-"
		if c.Annual {
			annual = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", c.Key, c.Folder, annual, counts[c.Key], state, c.Description)
	}
	if err := tw.Flush(); err != nil {
		return EX_IOERR
	}
	return EX_OK
}

// loadRules reads a rules file, or the defaults when path is empty.
func loadRules(path string) (archive.Rules, error) {
	if path == "" {
		return archive.DefaultRules(), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return archive.Rules{}, err
	}
	defer f.Close()
	return archive.ParseRules(bufio.NewReader(f))
}

// printPlanError maps a BuildPlan failure to an operator message and exit code.
func printPlanError(cmd, mailbox string, err error) int {
	switch {
	case errors.Is(err, storage.ErrNoArchiveFolder):
		fmt.Fprintf(os.Stderr, "mailbox %s has no \\Archive folder; give one with folder-set-special-use -use Archive first\n", mailbox)
		return EX_USAGE
	case errors.Is(err, archive.ErrUnknownRuleCategory):
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		return EX_DATAERR
	default:
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		return EX_TEMPFAIL
	}
}

func adminArchivePlan(args []string) int {
	fs := flag.NewFlagSet("admin archive-plan", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	mailbox := fs.String("mailbox", "", "Mailbox name (required)")
	rulesPath := fs.String("rules", "", "Rules TOML file (default: built-in rules)")
	movesOut := fs.String("moves-out", "", "Also write every planned move to this file as TSV")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	*mailbox = strings.ToLower(strings.TrimSpace(*mailbox))
	if *mailbox == "" {
		fmt.Fprintln(os.Stderr, "archive-plan: -mailbox is required")
		return EX_USAGE
	}
	rules, err := loadRules(*rulesPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "archive-plan: %v\n", err)
		return EX_DATAERR
	}
	db, mailboxID, code := adminArchiveMailbox(*configPath, *mailbox)
	if code != EX_OK {
		return code
	}
	defer db.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	plan, err := archive.BuildPlan(ctx, db, mailboxID, rules, time.Now())
	if err != nil {
		return printPlanError("archive-plan", *mailbox, err)
	}
	if err := plan.WriteReport(os.Stdout); err != nil {
		return EX_IOERR
	}
	if *movesOut != "" {
		f, err := os.OpenFile(*movesOut, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "archive-plan: %v\n", err)
			return EX_CANTCREAT
		}
		w := bufio.NewWriter(f)
		if err := plan.WriteMoves(w); err != nil {
			f.Close()
			fmt.Fprintf(os.Stderr, "archive-plan: write %s: %v\n", *movesOut, err)
			return EX_IOERR
		}
		if err := w.Flush(); err != nil {
			f.Close()
			fmt.Fprintf(os.Stderr, "archive-plan: write %s: %v\n", *movesOut, err)
			return EX_IOERR
		}
		if err := f.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "archive-plan: write %s: %v\n", *movesOut, err)
			return EX_IOERR
		}
		fmt.Fprintf(os.Stderr, "wrote %d planned moves to %s\n", len(plan.Moves), *movesOut)
	}
	fmt.Println("\ndry run: nothing was moved; archive-apply with the same rules carries this out")
	return EX_OK
}

func adminArchiveApply(args []string) int {
	fs := flag.NewFlagSet("admin archive-apply", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	mailbox := fs.String("mailbox", "", "Mailbox name (required)")
	rulesPath := fs.String("rules", "", "Rules TOML file (default: built-in rules)")
	batchSize := fs.Int("batch-size", 500, "Messages moved per transaction")
	limit := fs.Int("limit", 0, "Move at most this many messages (0 = all); for a trial run")
	pause := fs.Duration("pause", 50*time.Millisecond, "Sleep between transactions, leaving room for live delivery")
	yes := fs.Bool("yes", false, "Confirm: move the messages")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	*mailbox = strings.ToLower(strings.TrimSpace(*mailbox))
	if *mailbox == "" {
		fmt.Fprintln(os.Stderr, "archive-apply: -mailbox is required")
		return EX_USAGE
	}
	if *batchSize <= 0 || *batchSize > 5000 || *limit < 0 || *pause < 0 {
		fmt.Fprintln(os.Stderr, "archive-apply: -batch-size must be 1..5000; -limit and -pause must not be negative")
		return EX_USAGE
	}
	if !*yes {
		fmt.Fprintln(os.Stderr, "archive-apply moves messages; review archive-plan first, then pass -yes")
		return EX_USAGE
	}
	rules, err := loadRules(*rulesPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "archive-apply: %v\n", err)
		return EX_DATAERR
	}
	db, mailboxID, code := adminArchiveMailbox(*configPath, *mailbox)
	if code != EX_OK {
		return code
	}
	defer db.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	plan, err := archive.BuildPlan(ctx, db, mailboxID, rules, time.Now())
	if err != nil {
		return printPlanError("archive-apply", *mailbox, err)
	}
	batch := "reorg-" + time.Now().UTC().Format("20060102T150405Z")
	total := len(plan.Moves)
	if *limit > 0 && total > *limit {
		total = *limit
	}
	fmt.Printf("mailbox %s: moving %d of %d planned messages as batch %s\n", *mailbox, total, len(plan.Moves), batch)
	lastReport := time.Now()
	stats, err := archive.Apply(ctx, db, plan, archive.ApplyOptions{
		Batch:     batch,
		BatchSize: *batchSize,
		Limit:     *limit,
		Pause:     *pause,
		Progress: func(done, total int) {
			if time.Since(lastReport) >= 10*time.Second || done == total {
				fmt.Printf("  %d / %d\n", done, total)
				lastReport = time.Now()
			}
		},
	})
	fmt.Printf("moved %d, skipped %d (left their planned folder since), %d transactions; batch %s\n",
		stats.Moved, stats.Skipped, stats.Transactions, batch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "archive-apply stopped: %v\n"+
			"Every committed batch is complete and journaled. Re-run archive-apply to continue, "+
			"or archive-undo -batch %s to reverse what moved.\n", err, batch)
		return EX_TEMPFAIL
	}
	fmt.Printf("undo with: epistula-database admin archive-undo -mailbox %s -batch %s -yes\n", *mailbox, batch)
	return EX_OK
}

func adminArchiveUndo(args []string) int {
	fs := flag.NewFlagSet("admin archive-undo", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	mailbox := fs.String("mailbox", "", "Mailbox name (required)")
	batch := fs.String("batch", "", "Journal batch to reverse, as archive-apply printed it or archive-batches lists it (required)")
	dryRun := fs.Bool("dry-run", false, "Report what would be restored without moving anything")
	yes := fs.Bool("yes", false, "Confirm: move the messages back")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	*mailbox = strings.ToLower(strings.TrimSpace(*mailbox))
	if *mailbox == "" || *batch == "" {
		fmt.Fprintln(os.Stderr, "archive-undo: -mailbox and -batch are required")
		return EX_USAGE
	}
	if !*dryRun && !*yes {
		fmt.Fprintln(os.Stderr, "archive-undo moves messages; pass -dry-run to preview or -yes to proceed")
		return EX_USAGE
	}
	db, mailboxID, code := adminArchiveMailbox(*configPath, *mailbox)
	if code != EX_OK {
		return code
	}
	defer db.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stats, err := archive.Undo(ctx, db, mailboxID, *batch, *dryRun, 500)
	verb := "restored"
	if *dryRun {
		verb = "would restore"
	}
	fmt.Printf("batch %s: %s %d message(s); %d have moved elsewhere since and are left where they are\n",
		*batch, verb, stats.Restored, stats.Moved)
	if stats.Redirects > 0 {
		redirectVerb := "removed"
		if *dryRun {
			redirectVerb = "would remove"
		}
		fmt.Printf("batch %s: %s %d folder redirect(s); imports go to the restored folders again\n",
			*batch, redirectVerb, stats.Redirects)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "archive-undo stopped: %v\nRe-run to continue; restored entries are marked and are not moved twice.\n", err)
		return EX_TEMPFAIL
	}
	return EX_OK
}

func adminArchiveBatches(args []string) int {
	fs := flag.NewFlagSet("admin archive-batches", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	mailbox := fs.String("mailbox", "", "Mailbox name (required)")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	*mailbox = strings.ToLower(strings.TrimSpace(*mailbox))
	if *mailbox == "" {
		fmt.Fprintln(os.Stderr, "archive-batches: -mailbox is required")
		return EX_USAGE
	}
	db, mailboxID, code := adminArchiveMailbox(*configPath, *mailbox)
	if code != EX_OK {
		return code
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rows, err := db.Pool().Query(ctx, `
		SELECT batch, reason, count(*), count(undone_at), min(moved_at), max(moved_at)
		  FROM archive_moves WHERE mailbox_id = $1
		 GROUP BY batch, reason ORDER BY min(moved_at)`, mailboxID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "archive-batches: %v\n", err)
		return EX_TEMPFAIL
	}
	defer rows.Close()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "BATCH\tREASON\tMOVES\tUNDONE\tFIRST\tLAST")
	for rows.Next() {
		var batch, reason string
		var n, undone int64
		var first, last time.Time
		if err := rows.Scan(&batch, &reason, &n, &undone, &first, &last); err != nil {
			fmt.Fprintf(os.Stderr, "archive-batches: %v\n", err)
			return EX_TEMPFAIL
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%s\n", batch, reason, n, undone,
			first.UTC().Format(time.RFC3339), last.UTC().Format(time.RFC3339))
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "archive-batches: %v\n", err)
		return EX_TEMPFAIL
	}
	if err := tw.Flush(); err != nil {
		return EX_IOERR
	}
	return EX_OK
}

func adminFolderPruneEmpty(args []string) int {
	fs := flag.NewFlagSet("admin folder-prune-empty", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	mailbox := fs.String("mailbox", "", "Mailbox name (required)")
	dryRun := fs.Bool("dry-run", false, "List the folders that would be removed")
	yes := fs.Bool("yes", false, "Confirm: delete the empty folders")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	*mailbox = strings.ToLower(strings.TrimSpace(*mailbox))
	if *mailbox == "" {
		fmt.Fprintln(os.Stderr, "folder-prune-empty: -mailbox is required")
		return EX_USAGE
	}
	if !*dryRun && !*yes {
		fmt.Fprintln(os.Stderr, "folder-prune-empty deletes folders; pass -dry-run to preview or -yes to proceed")
		return EX_USAGE
	}
	db, mailboxID, code := adminArchiveMailbox(*configPath, *mailbox)
	if code != EX_OK {
		return code
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	names, err := db.PruneEmptyFolders(ctx, mailboxID, *dryRun)
	if err != nil {
		fmt.Fprintf(os.Stderr, "folder-prune-empty: %v\n", err)
		return EX_TEMPFAIL
	}
	verb := "removed"
	if *dryRun {
		verb = "would remove"
	}
	fmt.Printf("mailbox %s: %s %d empty folder(s)\n", *mailbox, verb, len(names))
	for _, n := range names {
		fmt.Printf("  %s\n", n)
	}
	if *dryRun {
		fmt.Println("dry run: nothing was written")
	}
	return EX_OK
}

func adminFolderMerge(args []string) int {
	fs := flag.NewFlagSet("admin folder-merge", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	mailbox := fs.String("mailbox", "", "Mailbox name (required)")
	from := fs.String("from", "", "Folder to empty (required)")
	subtree := fs.Bool("subtree", false, "Also empty every folder below -from")
	to := fs.String("to", "", "Folder that receives the messages (required; created if missing)")
	dropDuplicates := fs.Bool("drop-duplicates", false, "Destroy a source message whose exact content is already in -to, instead of leaving it behind")
	dryRun := fs.Bool("dry-run", false, "Report what would move without moving anything")
	yes := fs.Bool("yes", false, "Confirm: move the messages")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	*mailbox = strings.ToLower(strings.TrimSpace(*mailbox))
	if *mailbox == "" || *from == "" || *to == "" {
		fmt.Fprintln(os.Stderr, "folder-merge: -mailbox, -from and -to are required")
		return EX_USAGE
	}
	if !*dryRun && !*yes {
		fmt.Fprintln(os.Stderr, "folder-merge moves messages; pass -dry-run to preview or -yes to proceed")
		return EX_USAGE
	}
	db, mailboxID, code := adminArchiveMailbox(*configPath, *mailbox)
	if code != EX_OK {
		return code
	}
	defer db.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	batch := ""
	if !*dryRun {
		batch = "merge-" + time.Now().UTC().Format("20060102T150405Z")
	}
	stats, err := archive.MergeFolders(ctx, db, mailboxID, archive.MergeOptions{
		From: *from, Subtree: *subtree, To: *to,
		DropDuplicates: *dropDuplicates, DryRun: *dryRun, Batch: batch,
	})
	dupVerb := "left behind"
	if *dropDuplicates {
		dupVerb = "dropped"
	}
	for _, s := range stats.Sources {
		fmt.Printf("  %-50s %6d move, %d duplicate(s) of messages already in %q %s\n",
			s.Folder, s.Moved, s.Duplicates, *to, dupVerb)
	}
	switch {
	case errors.Is(err, storage.ErrNotFound):
		fmt.Fprintf(os.Stderr, "folder-merge: %v\n", err)
		return EX_USAGE
	case err != nil:
		fmt.Fprintf(os.Stderr, "folder-merge stopped: %v\nCommitted batches are journaled as %s; re-run to continue.\n", err, batch)
		return EX_TEMPFAIL
	}
	if *dryRun {
		fmt.Println("dry run: nothing was moved")
		return EX_OK
	}
	fmt.Printf("moved %d into %q as batch %s; %d duplicate(s) dropped, %d left behind\n",
		stats.Moved, *to, batch, stats.Dropped, stats.Skipped)
	scope := fmt.Sprintf("%q", *from)
	if *subtree {
		scope += " and every folder below it"
	}
	fmt.Printf("recorded a folder redirect: later imports into %s go to %q while the folder does not exist\n",
		scope, *to)
	fmt.Printf("undo the moves with: epistula-database admin archive-undo -mailbox %s -batch %s -yes\n", *mailbox, batch)
	fmt.Printf("then tidy up with:   epistula-database admin folder-prune-empty -mailbox %s -dry-run\n", *mailbox)
	return EX_OK
}

func adminArchiveCategorySuggest(args []string) int {
	fs := flag.NewFlagSet("admin archive-category-suggest", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	mailbox := fs.String("mailbox", "", "Mailbox name (required)")
	d := archive.DefaultSuggestOptions()
	minCount := fs.Int("min", d.MinCount, "Report a cluster of at least this many messages")
	share := fs.Float64("share", d.MinShare*100, "... or at least this percentage of its bucket")
	examples := fs.Int("examples", d.Examples, "Example subjects per cluster")
	minConf := fs.Float64("min-confidence", d.MinConfidence, "Confidence below which a classification counts as unsure (epistula-imap's [archive] min_confidence)")
	maxTag := fs.Float64("max-tag-share", d.MaxTagShare*100, "Skip a tag as a theme when more than this percentage of the mailbox's annotations carry it (100 keeps all)")
	format := fs.String("format", "text", "Output: text (a report) or toml (commented [[category]] proposals)")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	*mailbox = strings.ToLower(strings.TrimSpace(*mailbox))
	if *mailbox == "" {
		fmt.Fprintln(os.Stderr, "archive-category-suggest: -mailbox is required")
		return EX_USAGE
	}
	if *format != "text" && *format != "toml" {
		fmt.Fprintln(os.Stderr, "archive-category-suggest: -format is text or toml")
		return EX_USAGE
	}
	opts := archive.SuggestOptions{MinCount: *minCount, MinShare: *share / 100, Examples: *examples,
		MinConfidence: *minConf, MaxTagShare: *maxTag / 100, SenderOverlap: d.SenderOverlap}
	db, mailboxID, code := adminArchiveMailbox(*configPath, *mailbox)
	if code != EX_OK {
		return code
	}
	defer db.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	report, err := archive.Suggest(ctx, db, mailboxID, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "archive-category-suggest: %v\n", err)
		if errors.Is(err, storage.ErrNotFound) {
			return EX_USAGE
		}
		return EX_TEMPFAIL
	}
	w := bufio.NewWriter(os.Stdout)
	if *format == "toml" {
		err = report.WriteTOML(w)
	} else {
		err = report.WriteText(w)
	}
	if err == nil {
		err = w.Flush()
	}
	if err != nil {
		return EX_IOERR
	}
	return EX_OK
}

func adminArchiveReclassify(args []string) int {
	fs := flag.NewFlagSet("admin archive-reclassify", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	mailbox := fs.String("mailbox", "", "Mailbox name (required)")
	key := fs.String("key", "", "Category whose classifications to clear (required)")
	refile := fs.Bool("refile", false, "Also move what the sorter or a reorganization filed under the key back to \\Archive, to be filed again")
	dryRun := fs.Bool("dry-run", false, "Report what would happen without changing anything")
	yes := fs.Bool("yes", false, "Confirm")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	*mailbox = strings.ToLower(strings.TrimSpace(*mailbox))
	if *mailbox == "" || *key == "" {
		fmt.Fprintln(os.Stderr, "archive-reclassify: -mailbox and -key are required")
		return EX_USAGE
	}
	if !*dryRun && !*yes {
		fmt.Fprintln(os.Stderr, "archive-reclassify clears classifications; pass -dry-run to preview or -yes to proceed")
		return EX_USAGE
	}
	db, mailboxID, code := adminArchiveMailbox(*configPath, *mailbox)
	if code != EX_OK {
		return code
	}
	defer db.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	batch := ""
	if *refile && !*dryRun {
		batch = "refile-" + time.Now().UTC().Format("20060102T150405Z")
	}
	stats, err := archive.Reclassify(ctx, db, mailboxID, archive.ReclassifyOptions{
		Key: *key, Refile: *refile, DryRun: *dryRun, Batch: batch,
	})
	switch {
	case errors.Is(err, storage.ErrInvalidArchiveCategory), errors.Is(err, storage.ErrNoArchiveFolder):
		fmt.Fprintf(os.Stderr, "archive-reclassify: %v\n", err)
		return EX_USAGE
	case err != nil:
		fmt.Fprintf(os.Stderr, "archive-reclassify stopped: %v\n", err)
		return EX_TEMPFAIL
	}
	verb := map[bool]string{true: "would clear", false: "cleared"}[*dryRun]
	fmt.Printf("mailbox %s, key %s: %s %d classification(s)\n", *mailbox, *key, verb, stats.Cleared)
	if *refile {
		verb = map[bool]string{true: "would move", false: "moved"}[*dryRun]
		fmt.Printf("  %s %d filed message(s) back to the \\Archive folder; %d filed there have been moved since and stay\n",
			verb, stats.Refiled, stats.MovedSince)
		if batch != "" {
			fmt.Printf("  journaled as %s (archive-undo -batch %s puts them back)\n", batch, batch)
		}
	}
	fmt.Println("epistula-llm-worker's classification pass classifies them again against the current list")
	fmt.Println("once its running pass completes; refiled messages wait in the \\Archive folder until then,")
	fmt.Println("and epistula-imap's sorter (sort_enabled) files them by the new classification.")
	if *dryRun {
		fmt.Println("dry run: nothing was changed")
	}
	return EX_OK
}
