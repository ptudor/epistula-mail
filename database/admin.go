package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/term"
	"golang.org/x/text/unicode/norm"

	"github.com/ptudor/epistula-mail/database/auth"
	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/maintenance"
	"github.com/ptudor/epistula-mail/database/storage"
)

// isUniqueViolation classifies a Postgres unique-constraint error by
// SQLSTATE (23505) instead of matching the human-readable message, which
// varies across server versions and locales.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func runAdmin(args []string) int {
	if len(args) == 0 {
		printAdminUsage()
		return EX_USAGE
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "mailbox-add":
		return adminMailboxAdd(rest)
	case "mailbox-list":
		return adminMailboxList(rest)
	case "mailbox-passwd":
		return adminMailboxPasswd(rest)
	case "mailbox-disable":
		return adminMailboxDisable(rest)
	case "mailbox-enable":
		return adminMailboxEnable(rest)
	case "mailbox-delete":
		return adminMailboxDelete(rest)
	case "mailbox-rename":
		return adminMailboxRename(rest)
	case "mailbox-maintenance":
		return adminMailboxMaintenance(rest)
	case "auth-cost-audit":
		return adminAuthCostAudit(rest)
	case "domain-add":
		return adminDomainAdd(rest)
	case "domain-list":
		return adminDomainList(rest)
	case "domain-delete":
		return adminDomainDelete(rest)
	case "alias-add":
		return adminAliasAdd(rest)
	case "alias-list":
		return adminAliasList(rest)
	case "alias-delete":
		return adminAliasDelete(rest)
	case "folder-list":
		return adminFolderList(rest)
	case "folder-repair-ancestors":
		return adminFolderRepairAncestors(rest)
	case "folder-set-special-use":
		return adminFolderSetSpecialUse(rest)
	case "folder-prune-empty":
		return adminFolderPruneEmpty(rest)
	case "folder-merge":
		return adminFolderMerge(rest)
	case "folder-create":
		return adminFolderCreate(rest)
	case "archive-category-import":
		return adminArchiveCategoryImport(rest)
	case "archive-category-list":
		return adminArchiveCategoryList(rest)
	case "archive-plan":
		return adminArchivePlan(rest)
	case "archive-apply":
		return adminArchiveApply(rest)
	case "archive-undo":
		return adminArchiveUndo(rest)
	case "archive-batches":
		return adminArchiveBatches(rest)
	case "archive-category-suggest":
		return adminArchiveCategorySuggest(rest)
	case "archive-reclassify":
		return adminArchiveReclassify(rest)
	case "log-tail":
		return adminLogTail(rest)
	case "acl-add":
		return adminACLAdd(rest)
	case "acl-list":
		return adminACLList(rest)
	case "acl-delete":
		return adminACLDelete(rest)
	case "api-token-add":
		return adminAPITokenAdd(rest)
	case "api-token-list":
		return adminAPITokenList(rest)
	case "api-token-revoke":
		return adminAPITokenRevoke(rest)
	case "annotation-model-set":
		return adminAnnotationModelSet(rest)
	case "annotation-model-list":
		return adminAnnotationModelList(rest)
	case "annotation-model-retire":
		return adminAnnotationModelRetire(rest)
	case "reject-export":
		return runAdminRejectExport(rest)
	case "help", "-h", "--help":
		printAdminUsage()
		return EX_OK
	default:
		fmt.Fprintf(os.Stderr, "admin: unknown subcommand %q\n\n", sub)
		printAdminUsage()
		return EX_USAGE
	}
}

func printAdminUsage() {
	fmt.Fprintln(os.Stderr, `admin subcommands:

Mailbox:
  mailbox-add      -name N [-quota-bytes B] [-password-stdin]
  mailbox-list
  mailbox-passwd   -name N [-password-stdin]
  mailbox-disable  -name N
  mailbox-enable   -name N
  mailbox-delete   -name N -yes
  mailbox-rename   -from OLD -to NEW -yes
                   Move a mailbox to a new name, keeping its folders, messages
                   and aliases. The name is also the on-disk blob tenant, so the
                   directory <storage_root>/OLD must ALREADY have been moved to
                   NEW (on ZFS, via zfs rename) — the command refuses otherwise.
                   Requires the mailbox to be in maintenance first, and changes
                   the IMAP login. API token scopes are unaffected (they hold
                   durable mailbox ids, not names).
  auth-cost-audit  [-verbose]
                   Report the Argon2id cost of every stored credential. Accounts
                   hashed above the default cost cannot be covered by the IMAP
                   login timing floor and must be re-hashed.
  mailbox-maintenance -name N (-on | -off)
                   Quiesce a mailbox while its blob tenant tree is moved:
                   refuses delivery/APPEND/import for it and excludes it from
                   GC. -on blocks until in-flight writes finish. Not the same as
                   mailbox-disable, which still accepts delivery.

Domain:
  domain-add       -name N [-wildcard]
  domain-list
  domain-delete    -name N -yes

Alias:
  alias-add        -domain D -mailbox M (-localpart L | -catchall)
  alias-list       [-domain D]
  alias-delete     -domain D (-localpart L | -catchall)

ACL (per-domain allow/deny lists):
  acl-add          -domain D -localpart L (-allow | -deny) [-note "..."]
  acl-list         [-domain D] [-allow | -deny]
  acl-delete       -domain D -localpart L (-allow | -deny)

API tokens (bearer credentials for api; secret shown once):
  api-token-add    -name N -mailboxes ("*" | "a,b,c") -permission P [-permission P ...]
                   P ∈ read_metadata | read_content | write_annotation | write_classification
  api-token-list   [-all]
  api-token-revoke -name N

Annotation models (rank the per-model summaries epistula-api serves; highest priority is primary):
  annotation-model-set    -model M -priority N [-display "..."]
                          Register/re-rank a model and mark it active. Unregistered
                          models are served as priority-0 alternatives.
  annotation-model-list   [-all]
  annotation-model-retire -model M
                          Keep its annotations as alternatives but never primary.

Folder:
  folder-create    -mailbox M -folder F [-use ATTR] [-dry-run]
                   Create a folder (and its missing parents), optionally with
                   an RFC 6154 role, e.g. -folder Archive -use Archive. An
                   existing folder is left unchanged.
  folder-repair-ancestors (-mailbox M | -all) [-dry-run]
                   Create the missing parents of nested folders (Sent/2004 for
                   Sent/2004/11-Nov) left by imports that predate automatic
                   ancestor creation. Idempotent; -dry-run lists what it would
                   create without locking or writing.
  folder-set-special-use -mailbox M -folder F (-use ATTR | -clear) [-dry-run]
                   Give a folder its RFC 6154 role (\Sent, \Drafts, \Archive,
                   \Trash, \Junk, \All, \Flagged, \Important) so IMAP clients
                   use it; -clear removes it. One folder per attribute per
                   mailbox: clear the old holder before moving an attribute.
                   With epistula-imap's Trash purge on, whatever is in the
                   \Trash folder is destroyed after the retention period.
  folder-prune-empty -mailbox M (-dry-run | -yes)
                   Delete empty folders nothing needs (not INBOX, not
                   special-use, not an archive category folder or its parent),
                   deepest first. For tidying up after archive-apply.
  folder-merge -mailbox M -from F [-subtree] -to T [-drop-duplicates] (-dry-run | -yes)
                   Move every message of F (and, with -subtree, everything
                   below it) into T, journaled so archive-undo reverses it.
                   A message whose exact content is already in T is left
                   behind, or destroyed with -drop-duplicates (T keeps it).

Archive sorting (see ARCHIVE_SORTING.md):
  archive-category-import -mailbox M -file F [-dry-run]
                   Make the TOML category list in F the mailbox's approved
                   archive categories; keys not listed are retired. Every
                   folder must lie inside the mailbox's \Archive folder.
  archive-category-list   -mailbox M [-all]
  archive-category-suggest -mailbox M [-min N] [-share P] [-examples K] [-min-confidence C] [-format text|toml]
                   Read-only: find the senders and tags that recur where the
                   classifier was unsure (…/other keys, low confidence) and
                   propose sibling categories. Proposals only: add what you
                   want to the list and re-import it.
  archive-reclassify -mailbox M -key K [-refile] (-dry-run | -yes)
                   Clear K's classifications so the worker classifies those
                   messages again (after adding a sibling key). -refile also
                   moves what was filed under K back to \Archive, journaled.
  archive-plan     -mailbox M [-rules F] [-moves-out F]
                   Dry run of the one-time reorganization: where every
                   message would go and why the rest stay. Writes nothing.
  archive-apply    -mailbox M [-rules F] [-batch-size N] [-limit N] [-pause D] -yes
                   Carry the plan out in journaled batches; prints the batch
                   name. Safe to interrupt and re-run.
  archive-undo     -mailbox M -batch B (-dry-run | -yes)
                   Move everything batch B filed back where it came from.
  archive-batches  -mailbox M
                   List journaled batches (reorg and the live sorter's daily ones).

Inspection:
  folder-list      -mailbox M
  log-tail         [-since DUR] [-mailbox M] [-outcome STR] [-limit N]

Postfix policy export:
  reject-export    -in <csv> -out-dir <dir>
                   Translate CSV recipient and sender reject rules into Postfix
                   header_checks + check_recipient_access policy files.
                   See `+"`epistula-database admin reject-export -h`"+`.

All subcommands accept -config <path>. Destructive operations require -yes.`)
}

func adminMailboxAdd(args []string) int {
	fs := flag.NewFlagSet("admin mailbox-add", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	name := fs.String("name", "", "Mailbox name (required; lower-case [a-z0-9._-], 1-64 chars, must start alphanumeric)")
	quota := fs.Int64("quota-bytes", 0, "Quota in bytes (0 = unlimited)")
	passwordStdin := fs.Bool("password-stdin", false, "Read password from stdin instead of prompting on tty")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	if *name == "" {
		fmt.Fprintln(os.Stderr, "-name is required")
		return EX_USAGE
	}
	// The mailbox name is also the on-disk blob tenant (a single path
	// component), so it must satisfy the canonical, path-safe charset. Normalize
	// (lower-case + trim) the way domains are, then validate against the same
	// rule the blob store and the mailboxes.name CHECK constraint enforce.
	mailboxName := strings.ToLower(strings.TrimSpace(*name))
	if _, err := blob.ParseTenant(mailboxName); err != nil {
		fmt.Fprintf(os.Stderr, "invalid mailbox name %q: must be lower-case [a-z0-9._-], 1-64 chars, starting with a letter or digit\n", *name)
		return EX_USAGE
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return EX_CONFIG
	}
	setupLogging(cfg)

	password, err := readPassword(*passwordStdin, cfg.Admin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "password: %v\n", err)
		return EX_USAGE
	}

	hash, err := auth.HashPassword(password, auth.Params{
		Memory:     cfg.Argon2.Memory,
		Iterations: cfg.Argon2.Iterations,
		Parallel:   cfg.Argon2.Parallel,
		KeyLen:     cfg.Argon2.KeyLen,
		SaltLen:    cfg.Argon2.SaltLen,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hash: %v\n", err)
		return EX_SOFTWARE
	}

	db, code := adminOpenDB(cfg)
	if code != EX_OK {
		return code
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var quotaArg any
	if *quota > 0 {
		quotaArg = *quota
	}
	var id int64
	err = db.Pool().QueryRow(ctx,
		`INSERT INTO mailboxes (name, quota_bytes, password_hash) VALUES ($1, $2, $3) RETURNING id`,
		mailboxName, quotaArg, hash,
	).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			fmt.Fprintf(os.Stderr, "mailbox %q already exists\n", mailboxName)
			return EX_USAGE
		}
		fmt.Fprintf(os.Stderr, "insert: %v\n", err)
		return EX_TEMPFAIL
	}
	fmt.Printf("created mailbox %q (id=%d)\n", mailboxName, id)
	return EX_OK
}

func adminMailboxList(args []string) int {
	fs := flag.NewFlagSet("admin mailbox-list", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return EX_CONFIG
	}
	setupLogging(cfg)

	db, code := adminOpenDB(cfg)
	if code != EX_OK {
		return code
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rows, err := db.Pool().Query(ctx,
		`SELECT id, name, COALESCE(quota_bytes, 0), used_bytes,
		        disabled_at, created_at
		   FROM mailboxes
		  ORDER BY name`,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "query: %v\n", err)
		return EX_TEMPFAIL
	}
	defer rows.Close()

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tQUOTA\tUSED\tSTATE\tCREATED")
	for rows.Next() {
		var (
			id          int64
			name        string
			quota, used int64
			disabled    *time.Time
			created     time.Time
		)
		if err := rows.Scan(&id, &name, &quota, &used, &disabled, &created); err != nil {
			fmt.Fprintf(os.Stderr, "scan: %v\n", err)
			return EX_TEMPFAIL
		}
		state := "active"
		if disabled != nil {
			state = "disabled"
		}
		quotaStr := "∞"
		if quota > 0 {
			quotaStr = fmt.Sprintf("%d", quota)
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%d\t%s\t%s\n",
			id, name, quotaStr, used, state, created.Format(time.RFC3339))
	}
	tw.Flush()
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "rows: %v\n", err)
		return EX_TEMPFAIL
	}
	return EX_OK
}

func adminDomainAdd(args []string) int {
	fs := flag.NewFlagSet("admin domain-add", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	name := fs.String("name", "", "Domain name (required)")
	wildcard := fs.Bool("wildcard", false, "Treat as a wildcard domain")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	if *name == "" {
		fmt.Fprintln(os.Stderr, "-name is required")
		return EX_USAGE
	}
	normalized := normalizeDomain(*name)
	if err := validateDomainName(normalized); err != nil {
		fmt.Fprintf(os.Stderr, "domain-add: %v\n", err)
		return EX_USAGE
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return EX_CONFIG
	}
	setupLogging(cfg)

	db, code := adminOpenDB(cfg)
	if code != EX_OK {
		return code
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var id int64
	err = db.Pool().QueryRow(ctx,
		`INSERT INTO domains (name, is_wildcard) VALUES ($1, $2) RETURNING id`,
		normalized, *wildcard,
	).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			fmt.Fprintf(os.Stderr, "domain %q already exists\n", normalized)
			return EX_USAGE
		}
		fmt.Fprintf(os.Stderr, "insert: %v\n", err)
		return EX_TEMPFAIL
	}
	fmt.Printf("created domain %q (id=%d wildcard=%v)\n", normalized, id, *wildcard)
	return EX_OK
}

func adminAliasAdd(args []string) int {
	fs := flag.NewFlagSet("admin alias-add", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	domain := fs.String("domain", "", "Domain name (required)")
	mailbox := fs.String("mailbox", "", "Mailbox name (required)")
	localpart := fs.String("localpart", "", "Localpart (or use -catchall)")
	catchall := fs.Bool("catchall", false, "Add as the domain's catchall (localpart='')")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	if *domain == "" || *mailbox == "" {
		fmt.Fprintln(os.Stderr, "-domain and -mailbox are required")
		return EX_USAGE
	}
	if *catchall && *localpart != "" {
		fmt.Fprintln(os.Stderr, "specify -catchall OR -localpart, not both")
		return EX_USAGE
	}
	if !*catchall && *localpart == "" {
		fmt.Fprintln(os.Stderr, "specify -catchall OR -localpart")
		return EX_USAGE
	}
	// Normalize the localpart exactly as the delivery resolver does
	// (lower + trim + NFC) so the stored alias matches at delivery time.
	// Without this an `alias-add -localpart JDoe` inserts a row the resolver
	// (which lowercases + NFC-normalizes) never matches — mail bounces
	// EX_NOUSER and `alias-delete` (which lowercases) can't remove it (R-006).
	// Catchall still stores the empty localpart.
	lp := norm.NFC.String(strings.ToLower(strings.TrimSpace(*localpart)))
	if *catchall {
		lp = ""
	}
	domainNorm := normalizeDomain(*domain)
	// The -mailbox target is matched against the canonical stored name (lower +
	// trimmed), so `-mailbox JDoe` resolves to "jdoe" (R-048).
	*mailbox = strings.ToLower(strings.TrimSpace(*mailbox))

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return EX_CONFIG
	}
	setupLogging(cfg)

	db, code := adminOpenDB(cfg)
	if code != EX_OK {
		return code
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var aliasID int64
	err = db.Pool().QueryRow(ctx,
		`INSERT INTO aliases (domain_id, localpart, mailbox_id)
		 SELECT d.id, $2, m.id
		   FROM domains d, mailboxes m
		  WHERE d.name = $1 AND m.name = $3
		 RETURNING id`,
		domainNorm, lp, *mailbox,
	).Scan(&aliasID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			fmt.Fprintln(os.Stderr, "domain or mailbox not found")
			return EX_USAGE
		}
		if isUniqueViolation(err) {
			fmt.Fprintln(os.Stderr, "alias already exists")
			return EX_USAGE
		}
		fmt.Fprintf(os.Stderr, "insert: %v\n", err)
		return EX_TEMPFAIL
	}
	label := lp
	if lp == "" {
		label = "(catchall)"
	}
	fmt.Printf("created alias %s@%s → %s (id=%d)\n", label, domainNorm, *mailbox, aliasID)
	return EX_OK
}

func adminOpenDB(cfg *Config) (*storage.DB, int) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := storage.Open(ctx, storage.Config{
		DSN:              cfg.Postgres.DSN,
		StatementTimeout: cfg.StatementTimeoutDuration(),
		// A one-shot CLI command runs one transaction and exits, so it
		// deliberately pins a tiny pool rather than reading
		// postgres.max_open_conns — a pool of 20 here would be pure waste.
		// The knobs tune the long-lived serve/gc pools (RO5X-021).
		MaxConns:        2,
		MinConns:        0,
		ConnMaxLifetime: cfg.ConnMaxLifetimeDuration(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres: %v\n", err)
		return nil, EX_TEMPFAIL
	}
	return db, EX_OK
}

// readPassword reads a password from stdin (when fromStdin is true) or
// prompts interactively on the terminal. Both paths enforce the configured
// min/max length; the interactive path also requires confirmation.
//
// Reads via -password-stdin are bounded by an io.LimitReader so a runaway
// stdin (e.g. accidental `< /dev/urandom`) cannot allocate unbounded
// memory before the length check fires.
func readPassword(fromStdin bool, admin AdminConfig) (string, error) {
	minLen, maxLen := admin.MinPasswordLength, admin.MaxPasswordLength
	if minLen < 8 {
		minLen = 8
	}
	if maxLen <= minLen {
		maxLen = minLen + 1024
	}

	if fromStdin {
		// +1 so we can detect "exactly at limit" vs. "over limit".
		data, err := io.ReadAll(io.LimitReader(os.Stdin, int64(maxLen)+1))
		if err != nil {
			return "", err
		}
		pw := strings.TrimRight(string(data), "\r\n")
		return validatePassword(pw, minLen, maxLen)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("stdin is not a terminal; pass -password-stdin")
	}
	fmt.Fprint(os.Stderr, "Password: ")
	pw1, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	fmt.Fprint(os.Stderr, "Re-enter: ")
	pw2, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if string(pw1) != string(pw2) {
		return "", errors.New("passwords do not match")
	}
	return validatePassword(string(pw1), minLen, maxLen)
}

// validatePassword enforces length bounds and rejects passwords containing
// embedded NUL bytes (a common cause of confusing auth failures when an
// Argon2 backend silently truncates).
func validatePassword(pw string, minLen, maxLen int) (string, error) {
	if len(pw) < minLen {
		return "", fmt.Errorf("password must be at least %d characters", minLen)
	}
	if len(pw) > maxLen {
		return "", fmt.Errorf("password must be at most %d characters", maxLen)
	}
	if strings.ContainsRune(pw, 0) {
		return "", errors.New("password contains a NUL byte")
	}
	return pw, nil
}

// adminACLAdd inserts a (domain, localpart, allow|deny) row into domain_acl.
// Deny rows reject delivery absolutely; allow rows gate the wildcard catchall.
func adminACLAdd(args []string) int {
	fs := flag.NewFlagSet("acl-add", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	domain := fs.String("domain", "", "Domain name (required)")
	localpart := fs.String("localpart", "", "Localpart to add to the ACL (required)")
	allow := fs.Bool("allow", false, "Add as an allow entry")
	deny := fs.Bool("deny", false, "Add as a deny entry")
	note := fs.String("note", "", "Optional human-readable reason")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	if *domain == "" || *localpart == "" {
		fmt.Fprintln(os.Stderr, "acl-add: -domain and -localpart are required")
		return EX_USAGE
	}
	if *allow == *deny {
		fmt.Fprintln(os.Stderr, "acl-add: exactly one of -allow or -deny must be set")
		return EX_USAGE
	}
	kind := "allow"
	if *deny {
		kind = "deny"
	}
	domainNorm, localNorm, code := normalizeACLArgs(*domain, *localpart)
	if code != EX_OK {
		return code
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var notePtr *string
	if *note != "" {
		notePtr = note
	}
	var id int64
	err = db.Pool().QueryRow(ctx,
		`INSERT INTO domain_acl (domain_id, localpart, kind, note)
		 SELECT d.id, $2, $3, $4 FROM domains d WHERE d.name = $1
		 RETURNING id`,
		domainNorm, localNorm, kind, notePtr,
	).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			fmt.Fprintln(os.Stderr, "domain not found")
			return EX_USAGE
		}
		if isUniqueViolation(err) {
			fmt.Fprintln(os.Stderr, "acl entry already exists")
			return EX_USAGE
		}
		fmt.Fprintf(os.Stderr, "insert: %v\n", err)
		return EX_TEMPFAIL
	}
	fmt.Printf("created %s acl %s@%s (id=%d)\n", kind, localNorm, domainNorm, id)
	return EX_OK
}

// adminACLList prints ACL rows, optionally filtered by domain and/or kind.
func adminACLList(args []string) int {
	fs := flag.NewFlagSet("acl-list", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	domain := fs.String("domain", "", "Restrict to this domain")
	allow := fs.Bool("allow", false, "Show only allow rows")
	deny := fs.Bool("deny", false, "Show only deny rows")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	if *allow && *deny {
		fmt.Fprintln(os.Stderr, "acl-list: -allow and -deny are mutually exclusive")
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	query := `SELECT d.name, a.localpart, a.kind, COALESCE(a.note, ''), a.created_at
	            FROM domain_acl a
	            JOIN domains d ON d.id = a.domain_id`
	conds := []string{}
	argsSQL := []any{}
	if *domain != "" {
		argsSQL = append(argsSQL, normalizeDomain(*domain))
		conds = append(conds, fmt.Sprintf("d.name = $%d", len(argsSQL)))
	}
	if *allow {
		conds = append(conds, "a.kind = 'allow'")
	}
	if *deny {
		conds = append(conds, "a.kind = 'deny'")
	}
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += " ORDER BY d.name, a.kind, a.localpart"

	rows, err := db.Pool().Query(ctx, query, argsSQL...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "query: %v\n", err)
		return EX_TEMPFAIL
	}
	defer rows.Close()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "DOMAIN\tLOCALPART\tKIND\tCREATED\tNOTE")
	count := 0
	for rows.Next() {
		var (
			dn, lp, kind, note string
			created            time.Time
		)
		if err := rows.Scan(&dn, &lp, &kind, &note, &created); err != nil {
			fmt.Fprintf(os.Stderr, "scan: %v\n", err)
			return EX_TEMPFAIL
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", dn, lp, kind, created.UTC().Format("2006-01-02"), note)
		count++
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "rows: %v\n", err)
		return EX_TEMPFAIL
	}
	if err := w.Flush(); err != nil {
		return EX_IOERR
	}
	if count == 0 {
		fmt.Println("(no acl rows)")
	}
	return EX_OK
}

// adminACLDelete removes a single (domain, localpart, kind) row.
func adminACLDelete(args []string) int {
	fs := flag.NewFlagSet("acl-delete", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	domain := fs.String("domain", "", "Domain name (required)")
	localpart := fs.String("localpart", "", "Localpart (required)")
	allow := fs.Bool("allow", false, "Target an allow entry")
	deny := fs.Bool("deny", false, "Target a deny entry")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	if *domain == "" || *localpart == "" {
		fmt.Fprintln(os.Stderr, "acl-delete: -domain and -localpart are required")
		return EX_USAGE
	}
	if *allow == *deny {
		fmt.Fprintln(os.Stderr, "acl-delete: exactly one of -allow or -deny must be set")
		return EX_USAGE
	}
	kind := "allow"
	if *deny {
		kind = "deny"
	}
	domainNorm, localNorm, code := normalizeACLArgs(*domain, *localpart)
	if code != EX_OK {
		return code
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tag, err := db.Pool().Exec(ctx,
		`DELETE FROM domain_acl
		  WHERE domain_id = (SELECT id FROM domains WHERE name = $1)
		    AND localpart = $2 AND kind = $3`,
		domainNorm, localNorm, kind,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "delete: %v\n", err)
		return EX_TEMPFAIL
	}
	if tag.RowsAffected() == 0 {
		fmt.Fprintln(os.Stderr, "no matching acl entry")
		return EX_USAGE
	}
	fmt.Printf("deleted %s acl %s@%s\n", kind, localNorm, domainNorm)
	return EX_OK
}

// normalizeDomain lowercases, trims, and NFC-normalizes a domain name.
//
// The delivery resolver NFC-normalizes the envelope domain
// (recipients/recipients.go), but the admin CLI did not — so an
// internationalized domain added in NFD form could never match a delivery.
// That is the exact class R-006 fixed for localparts; the domain half was
// missed (RO5X-033).
func normalizeDomain(s string) string {
	return norm.NFC.String(strings.ToLower(strings.TrimSpace(s)))
}

// maxDomainBytes is the RFC 1035 limit on a fully-qualified name.
const maxDomainBytes = 253

// validateDomainName checks that a domain is a sequence of LDH labels
// (letters, digits, hyphen), each 1-63 bytes and not starting or ending with a
// hyphen, totalling <= 253 bytes.
//
// domains.name has no CHECK constraint and domain-add applied no charset,
// length, or shape check at all, so `-name "example .invalid"` or a name
// containing a NUL was accepted straight into the table (RO5X-033).
//
// Deliberately permissive about the TLD (no "must contain a dot") — a
// single-label internal domain is legitimate in a private deployment.
// Punycode (xn--) passes as ordinary LDH; a non-ASCII domain must be
// IDNA-encoded by the operator, which is what the resolver compares against.
func validateDomainName(d string) error {
	if d == "" {
		return errors.New("domain name is empty")
	}
	if len(d) > maxDomainBytes {
		return fmt.Errorf("domain name is %d bytes; the limit is %d", len(d), maxDomainBytes)
	}
	if strings.HasPrefix(d, ".") || strings.HasSuffix(d, ".") {
		return errors.New("domain name must not start or end with a dot")
	}
	for _, label := range strings.Split(d, ".") {
		if label == "" {
			return errors.New("domain name has an empty label (consecutive dots)")
		}
		if len(label) > 63 {
			return fmt.Errorf("domain label %q is %d bytes; the limit is 63", label, len(label))
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("domain label %q must not start or end with a hyphen", label)
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			default:
				return fmt.Errorf("domain label %q contains an invalid character %q "+
					"(letters, digits, and hyphen only; use IDNA/punycode for non-ASCII)",
					label, string(rune(c)))
			}
		}
	}
	return nil
}

// normalizeACLArgs lowercases and trims the domain and localpart. The
// localpart MUST be non-empty (the catchall slot is owned by aliases, not
// the ACL table).
func normalizeACLArgs(domain, localpart string) (string, string, int) {
	d := normalizeDomain(domain)
	// NFC-normalize the localpart to match the delivery resolver; a decomposed
	// non-ASCII ACL localpart would otherwise never match its NFC envelope
	// form (R-006). Domain lookup stays lowercase-only (matches domain storage).
	lp := norm.NFC.String(strings.ToLower(strings.TrimSpace(localpart)))
	if d == "" {
		fmt.Fprintln(os.Stderr, "domain is empty")
		return "", "", EX_USAGE
	}
	if lp == "" {
		fmt.Fprintln(os.Stderr, "localpart is empty (the catchall slot is managed via alias-add -catchall)")
		return "", "", EX_USAGE
	}
	return d, lp, EX_OK
}

// adminMailboxPasswd resets a mailbox's Argon2id password hash.
func adminMailboxPasswd(args []string) int {
	fs := flag.NewFlagSet("admin mailbox-passwd", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	name := fs.String("name", "", "Mailbox name (required)")
	passwordStdin := fs.Bool("password-stdin", false, "Read password from stdin instead of prompting on tty")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	// Mailbox names are stored canonical (lower + trimmed, enforced by the
	// CHECK); match the same way so `mailbox-passwd -name JDoe` finds "jdoe"
	// like the domain commands do (R-048). Exact-match-after-normalization.
	*name = strings.ToLower(strings.TrimSpace(*name))
	if *name == "" {
		fmt.Fprintln(os.Stderr, "-name is required")
		return EX_USAGE
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return EX_CONFIG
	}
	setupLogging(cfg)

	password, err := readPassword(*passwordStdin, cfg.Admin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "password: %v\n", err)
		return EX_USAGE
	}
	hash, err := auth.HashPassword(password, auth.Params{
		Memory:     cfg.Argon2.Memory,
		Iterations: cfg.Argon2.Iterations,
		Parallel:   cfg.Argon2.Parallel,
		KeyLen:     cfg.Argon2.KeyLen,
		SaltLen:    cfg.Argon2.SaltLen,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hash: %v\n", err)
		return EX_SOFTWARE
	}

	db, code := adminOpenDB(cfg)
	if code != EX_OK {
		return code
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tag, err := db.Pool().Exec(ctx,
		`UPDATE mailboxes SET password_hash = $1, updated_at = now() WHERE name = $2`,
		hash, *name,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: %v\n", err)
		return EX_TEMPFAIL
	}
	if tag.RowsAffected() == 0 {
		fmt.Fprintf(os.Stderr, "mailbox %q not found\n", *name)
		return EX_USAGE
	}
	fmt.Printf("updated password for mailbox %q\n", *name)
	return EX_OK
}

// adminMailboxDisable sets disabled_at = now(). IMAP auth will fail; mail
// continues to be delivered (the messages accumulate in the mailbox).
func adminMailboxDisable(args []string) int {
	return mailboxToggleDisabled(args, "mailbox-disable", true)
}

// adminMailboxEnable clears disabled_at.
func adminMailboxEnable(args []string) int {
	return mailboxToggleDisabled(args, "mailbox-enable", false)
}

func mailboxToggleDisabled(args []string, label string, disable bool) int {
	fs := flag.NewFlagSet("admin "+label, flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	name := fs.String("name", "", "Mailbox name (required)")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	// Match the canonical stored name (lower + trimmed) so case-variant input
	// resolves like the domain commands (R-048).
	*name = strings.ToLower(strings.TrimSpace(*name))
	if *name == "" {
		fmt.Fprintln(os.Stderr, "-name is required")
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var query string
	if disable {
		query = `UPDATE mailboxes SET disabled_at = now(), updated_at = now()
		          WHERE name = $1 AND disabled_at IS NULL`
	} else {
		query = `UPDATE mailboxes SET disabled_at = NULL, updated_at = now()
		          WHERE name = $1 AND disabled_at IS NOT NULL`
	}
	tag, err := db.Pool().Exec(ctx, query, *name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: %v\n", err)
		return EX_TEMPFAIL
	}
	if tag.RowsAffected() == 0 {
		// Either no such mailbox, or it's already in the target state.
		var exists bool
		_ = db.Pool().QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM mailboxes WHERE name = $1)`, *name,
		).Scan(&exists)
		if !exists {
			fmt.Fprintf(os.Stderr, "mailbox %q not found\n", *name)
			return EX_USAGE
		}
		fmt.Printf("mailbox %q already in target state, no change\n", *name)
		return EX_OK
	}
	if disable {
		fmt.Printf("disabled mailbox %q\n", *name)
	} else {
		fmt.Printf("enabled mailbox %q\n", *name)
	}
	return EX_OK
}

// adminMailboxDelete removes a mailbox and (via ON DELETE CASCADE) its
// aliases, folders, messages, and subscriptions. Blob bytes survive until
// `gc mark` + `gc sweep` reap them.
func adminMailboxDelete(args []string) int {
	fs := flag.NewFlagSet("admin mailbox-delete", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	name := fs.String("name", "", "Mailbox name (required)")
	yes := fs.Bool("yes", false, "Confirm the destructive operation")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	// Match the canonical stored name (lower + trimmed), like the domain
	// commands, so case-variant input resolves (R-048).
	*name = strings.ToLower(strings.TrimSpace(*name))
	if *name == "" {
		fmt.Fprintln(os.Stderr, "-name is required")
		return EX_USAGE
	}
	if !*yes {
		fmt.Fprintln(os.Stderr, "mailbox-delete is destructive; pass -yes to confirm")
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Pre-compute counts so the operator sees what's about to vanish.
	var folderCount, messageCount, aliasCount int64
	_ = db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM folders WHERE mailbox_id = (SELECT id FROM mailboxes WHERE name = $1)`,
		*name,
	).Scan(&folderCount)
	_ = db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM messages
		   WHERE folder_id IN (
		     SELECT id FROM folders WHERE mailbox_id = (SELECT id FROM mailboxes WHERE name = $1)
		   )`,
		*name,
	).Scan(&messageCount)
	_ = db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM aliases WHERE mailbox_id = (SELECT id FROM mailboxes WHERE name = $1)`,
		*name,
	).Scan(&aliasCount)

	// Delete the mailbox and retire every API-token grant that referred to it,
	// in ONE transaction (RA6X-012).
	//
	// Token scope is a list of durable mailbox IDs, so a stale entry can never
	// be reinterpreted as access to a mailbox that later takes this name —
	// mailboxes.id is a BIGSERIAL and is never reused. Removing the dead entry
	// anyway keeps `api-token-list` honest and, more importantly, surfaces the
	// tokens whose LAST scope just went away: those authorize nothing at all
	// now, so they are revoked rather than left as live credentials with an
	// empty grant that some future reader might mistake for "unrestricted".
	//
	// Locking the mailbox row FOR UPDATE serializes this against
	// `api-token-add`, which takes FOR SHARE on the rows it is about to scope
	// a new token to. Without it, a token could be minted against this mailbox
	// between the DELETE and the scope cleanup and survive as a grant on a
	// deleted account.
	var (
		deleted       bool
		rescoped      int64
		revokedTokens []string
	)
	err = db.RunTx(ctx, func(tx pgx.Tx) error {
		var mailboxID int64
		if err := tx.QueryRow(ctx,
			`SELECT id FROM mailboxes WHERE name = $1 FOR UPDATE`, *name,
		).Scan(&mailboxID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("lookup: %w", err)
		}
		deleted = true

		if _, err := tx.Exec(ctx, `DELETE FROM mailboxes WHERE id = $1`, mailboxID); err != nil {
			return fmt.Errorf("delete: %w", err)
		}

		// Drop the dead scope entry and revoke in ONE statement. Splitting
		// them cannot work: api_tokens_scope_nonempty is a row CHECK, so an
		// UPDATE that empties a live token's scope is rejected before any
		// follow-up revoke could run. Doing both in one row update lets the
		// constraint see the final row, where the token is already revoked.
		rows, err := tx.Query(ctx,
			`UPDATE api_tokens
			    SET scope_mailbox_ids = array_remove(scope_mailbox_ids, $1),
			        revoked_at = CASE
			          WHEN revoked_at IS NULL
			           AND NOT scope_all_mailboxes
			           AND cardinality(array_remove(scope_mailbox_ids, $1)) = 0
			          THEN now()
			          ELSE revoked_at
			        END
			  WHERE $1 = ANY(scope_mailbox_ids)
			RETURNING name, cardinality(scope_mailbox_ids) = 0 AND NOT scope_all_mailboxes`,
			mailboxID)
		if err != nil {
			return fmt.Errorf("rescope tokens: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			var nowScopeless bool
			if err := rows.Scan(&n, &nowScopeless); err != nil {
				return fmt.Errorf("scan token: %w", err)
			}
			rescoped++
			if nowScopeless {
				revokedTokens = append(revokedTokens, n)
			}
		}
		return rows.Err()
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "delete: %v\n", err)
		return EX_TEMPFAIL
	}
	if !deleted {
		fmt.Fprintf(os.Stderr, "mailbox %q not found\n", *name)
		return EX_USAGE
	}
	fmt.Printf("deleted mailbox %q (cascaded: %d folders, %d messages, %d aliases). Blob bytes remain on disk until `gc mark` + `gc sweep` reap them.\n",
		*name, folderCount, messageCount, aliasCount)
	if rescoped > 0 {
		fmt.Printf("removed this mailbox from %d API token scope(s)\n", rescoped)
	}
	if len(revokedTokens) > 0 {
		fmt.Printf("revoked %d API token(s) left with no mailbox in scope: %s\n",
			len(revokedTokens), strings.Join(revokedTokens, ", "))
	}
	return EX_OK
}

// errMailboxNotFound / errRenameTargetExists classify the two operator
// mistakes mailbox-rename can hit inside its transaction, so the exit code
// can be EX_USAGE (the operator typed a bad name) rather than EX_TEMPFAIL
// (the database is unhappy).
var (
	errMailboxNotFound    = errors.New("mailbox not found")
	errRenameTargetExists = errors.New("target mailbox name already exists")
	errMailboxNotQuiesced = errors.New("mailbox is not in maintenance")
)

// dirExists reports whether p is an existing directory. A path that exists
// but is not a directory is reported as an error rather than "false": under
// storage_root that means something is badly wrong and silently treating it
// as absent would let the rename proceed into it.
func dirExists(p string) (bool, error) {
	fi, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !fi.IsDir() {
		return false, fmt.Errorf("%s exists but is not a directory", p)
	}
	return true, nil
}

// errFoundFile stops the tenantHoldsBlobs walk at the first hit.
var errFoundFile = errors.New("found a file")

// tenantHoldsBlobs reports whether the tenant subtree at dir still holds any
// content. An absent directory holds nothing.
//
// Existence of the directory is NOT the question the rename guard needs
// answered — a tenant that is its own ZFS dataset gets created empty, ahead
// of the mailbox that will live in it, so "the directory is there" and "the
// blobs are still there" are different facts. Only the second one can strand
// mail. The walk short-circuits on the first non-directory entry, so the
// dangerous case (a populated tree) is decided almost immediately and the
// safe case (a fresh empty dataset) costs one readdir.
func tenantHoldsBlobs(dir string) (bool, error) {
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return errFoundFile
		}
		return nil
	})
	switch {
	case errors.Is(err, errFoundFile):
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	}
	return false, nil
}

// adminMailboxRename renames a mailbox in place. Every folder, message,
// alias, subscription and delivery-log row stays attached: they all
// reference mailbox_id, so they follow the rename for free.
//
// Three things key off the NAME rather than the id, and each one fails
// silently rather than loudly if it is missed:
//
//  1. The on-disk blob tenant directory, <storage_root>/<name>/{raw,att,tmp}.
//     deliver.go derives it with blob.ParseTenant(match.MailboxName) on every
//     message.
//  2. The IMAP login name, and therefore every MUA account profile.
//
// It no longer has to do anything about API tokens: since migration 012
// (RA6X-012) api_tokens scope is a list of durable mailbox IDs, so a rename
// leaves every token pointing at the same account. That also closed the window
// where the old name was briefly unclaimed and a quick recreate under it could
// inherit the grant.
//
// This command reports (2). It deliberately does NOT do (1):
// moving a tenant directory is a filesystem operation whose correct form is
// deployment-specific — where each mailbox is its own ZFS dataset, the move
// is `zfs rename`, not os.Rename — and it must happen with deliveries stopped.
// Instead the command REQUIRES the move to have already happened and refuses
// otherwise. Renaming the row while the blobs sit under the old directory
// splits the store in two: reads for the renamed mailbox find nothing, and the
// next mailbox created under the old name inherits a stranger's mail.
//
// Because the move and the row update cannot be one atomic step, the mailbox
// must be in maintenance for the whole of both (RA6X-013). "Stop Postfix" is
// not enough: it leaves authenticated IMAP sessions APPENDing under their
// cached tenant, imports running, and `gc mark` free to see a tenant directory
// that resolves to no mailbox and treat the entire freshly moved tree as
// reapable. An exclusive root-directory lease drains all blob processes before
// maintenance begins; the durable flag refuses new processes throughout the
// move.
//
// Before committing, every blob the database says this mailbox owns is
// verified present under the DESTINATION tree, so a partial move is caught
// while it is still recoverable rather than discovered by a user whose mail has
// gone missing.
//
// Recovery, filesystem move succeeded but this command failed: the tree is
// under the new name and the row still says the old one. NOTHING has been lost
// — the mailbox stays quiesced, so no writer can act on the disagreement. Fix
// whatever the command reported and run it again; or, to abandon the rename,
// move the tree back to the old name and release maintenance. Do not run `gc`
// until one of those is done.
func adminMailboxRename(args []string) int {
	fs := flag.NewFlagSet("admin mailbox-rename", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	from := fs.String("from", "", "Current mailbox name (required)")
	to := fs.String("to", "", "New mailbox name (required)")
	yes := fs.Bool("yes", false, "Confirm: this invalidates IMAP logins for the mailbox")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	// Canonicalize both sides the way mailbox-add does, so `-from JDoe`
	// resolves the stored "jdoe" (R-048) and `-to` is validated against the
	// exact rule the blob store and the mailboxes.name CHECK both enforce.
	oldName := strings.ToLower(strings.TrimSpace(*from))
	newName := strings.ToLower(strings.TrimSpace(*to))
	if oldName == "" || newName == "" {
		fmt.Fprintln(os.Stderr, "-from and -to are both required")
		return EX_USAGE
	}
	if oldName == newName {
		fmt.Fprintf(os.Stderr, "-from and -to are the same mailbox (%q); nothing to do\n", oldName)
		return EX_USAGE
	}
	if _, err := blob.ParseTenant(newName); err != nil {
		fmt.Fprintf(os.Stderr, "invalid mailbox name %q: must be lower-case [a-z0-9._-], 1-64 chars, starting with a letter or digit\n", *to)
		return EX_USAGE
	}
	if !*yes {
		fmt.Fprintln(os.Stderr, `mailbox-rename changes the IMAP login name; pass -yes to confirm`)
		return EX_USAGE
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return EX_CONFIG
	}
	setupLogging(cfg)

	db, code := adminOpenDB(cfg)
	if code != EX_OK {
		return code
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	lease, lerr := maintenance.Acquire(ctx, cfg.Storage.Root, db.Pool(), true, false)
	if lerr != nil {
		fmt.Fprintf(os.Stderr, "offline maintenance barrier: %v\n", lerr)
		return EX_TEMPFAIL
	}
	defer lease.Close()

	// The mailbox must already be quiesced (RA6X-013). Stopping Postfix is not
	// enough: authenticated IMAP sessions keep APPENDing under the tenant they
	// cached at LOGIN, imports keep running, and `gc mark` is free to see a
	// tenant directory that resolves to no mailbox — which is exactly what the
	// destination tree looks like between the move and this command — and treat
	// the whole thing as reapable.
	var (
		mailboxID     int64
		maintenanceAt *time.Time
		storedMsgs    int64
	)
	if err := db.Pool().QueryRow(ctx,
		`SELECT id, maintenance_at FROM mailboxes WHERE name = $1`, oldName,
	).Scan(&mailboxID, &maintenanceAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			fmt.Fprintf(os.Stderr, "mailbox %q not found\n", oldName)
			return EX_USAGE
		}
		fmt.Fprintf(os.Stderr, "lookup: %v\n", err)
		return EX_TEMPFAIL
	}
	if maintenanceAt == nil {
		fmt.Fprintf(os.Stderr, `refusing: mailbox %q is not in maintenance.

The mailbox name IS its on-disk blob tenant path, so a rename cannot be one
atomic step: the tree moves, then this command updates the row. Every writer
and the garbage collector must be held off across BOTH, or an APPEND lands its
blob under one name and its row under another, or gc reaps the moved tree.

  # Stop IMAP/API, Postfix and all import/reparse/GC processes; wait for exit.
  epistula-database admin mailbox-maintenance -name %s -on
  # ... move the tree, then re-run this command ...
`, oldName, oldName)
		return EX_USAGE
	}

	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM messages
		   WHERE folder_id IN (SELECT id FROM folders WHERE mailbox_id = $1)`,
		mailboxID,
	).Scan(&storedMsgs); err != nil {
		fmt.Fprintf(os.Stderr, "count messages: %v\n", err)
		return EX_TEMPFAIL
	}

	oldDir := filepath.Join(cfg.Storage.Root, oldName)
	newDir := filepath.Join(cfg.Storage.Root, newName)
	// The question is whether BLOBS remain under the old name, not whether a
	// directory does. Where each tenant is its own ZFS dataset, the successor
	// mailbox's dataset is routinely created empty before this runs, so an
	// existence test would refuse a perfectly safe rename.
	oldHasBlobs, err := tenantHoldsBlobs(oldDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "storage: %v\n", err)
		return EX_CONFIG
	}
	newOnDisk, err := dirExists(newDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "storage: %v\n", err)
		return EX_CONFIG
	}
	switch {
	case oldHasBlobs && newOnDisk:
		fmt.Fprintf(os.Stderr, `refusing: the old tenant tree still holds blobs and the new one already exists.

  still populated: %s
  also present:    %s

Renaming now would strand whichever tree is not the live one. Work out which
holds this mailbox's blobs, merge or remove the other, then re-run.
`, oldDir, newDir)
		return EX_USAGE
	case oldHasBlobs && !newOnDisk:
		fmt.Fprintf(os.Stderr, `refusing: the blob tenant directory has not been moved yet.

  found:   %s
  wanted:  %s

The mailbox name IS the on-disk tenant path, so the directory must move first.
Renaming the database row while the blobs sit under the old name splits the
store: reads for the renamed mailbox would find nothing, and the next mailbox
created as %q would inherit this one's mail.

Correct order:

  # Stop IMAP/API, Postfix and all import/reparse/GC processes; wait for exit.
  epistula-database admin mailbox-maintenance -name %s -on
  sudo zfs rename <pool>/var/spool/epistula-database/%s <pool>/var/spool/epistula-database/%s
  # or, if the tenant is a plain directory rather than its own dataset:
  #   sudo mv %s %s
  epistula-database admin mailbox-rename -from %s -to %s -yes
`, oldDir, newDir, oldName, oldName, oldName, newName, oldDir, newDir, oldName, newName)
		return EX_USAGE
	case !oldHasBlobs && !newOnDisk:
		// Neither tree exists. That is normal for an account that has never
		// stored a message — and indistinguishable, from the filesystem alone,
		// from a storage root that failed to mount or is misconfigured. The
		// database can tell the two apart, so ask it rather than inferring
		// "no blobs to move" from absence (RA6X-013).
		if storedMsgs > 0 {
			fmt.Fprintf(os.Stderr, `refusing: this mailbox holds %d message(s) but neither tenant tree exists.

  absent: %s
  absent: %s

Either the storage root is not mounted or storage.root is misconfigured. Do NOT
re-run until %s resolves to the real blob store: proceeding would rename the row
while its blobs are somewhere this process cannot see.
`, storedMsgs, oldDir, newDir, cfg.Storage.Root)
			return EX_CONFIG
		}
		fmt.Printf("note: nothing stored under %s for either name, and the mailbox has no messages — no blobs to move.\n",
			cfg.Storage.Root)
	}

	// The move looks done. Before committing the row, prove it: every blob the
	// database says this mailbox owns must be readable under the DESTINATION
	// tree. A partial move (an interrupted rsync, a dataset that only half
	// replicated) is caught here, while it is still recoverable, instead of
	// surfacing later as a user's missing mail.
	if storedMsgs > 0 {
		missing, checked, err := verifyTenantBlobs(ctx, db, mailboxID, cfg.Storage.Root, newName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "verify blobs under %s: %v\n", newDir, err)
			return EX_TEMPFAIL
		}
		if len(missing) > 0 {
			fmt.Fprintf(os.Stderr, `refusing: %d of %d referenced blob(s) are missing or corrupt under the destination tree.

  destination: %s

First few missing/corrupt:
`, len(missing), checked, newDir)
			for i, m := range missing {
				if i >= 10 {
					fmt.Fprintf(os.Stderr, "  ... and %d more\n", len(missing)-i)
					break
				}
				fmt.Fprintf(os.Stderr, "  %s\n", m)
			}
			fmt.Fprintf(os.Stderr, `
The move is incomplete. The mailbox stays in maintenance and the row still says
%q, so nothing can act on the disagreement. Finish copying the tree (or move it
back and release maintenance to abandon the rename) and re-run.
`, oldName)
			return EX_USAGE
		}
		fmt.Printf("verified %d referenced blob(s) under %s\n", checked, newDir)
	}

	var (
		folderCount, messageCount, aliasCount int64
		rescopedTokens                        []string
	)
	err = db.RunTx(ctx, func(tx pgx.Tx) error {
		// FOR UPDATE pins the row for the whole transaction. It is also the
		// write barrier: storage.Ingest takes this same row exclusively for
		// every delivery, APPEND and import, so no writer can be mid-flight
		// while the name changes, and none can start afterwards without
		// reading the new name (RA6X-013).
		var id int64
		var stillQuiesced *time.Time
		if err := tx.QueryRow(ctx,
			`SELECT id, maintenance_at FROM mailboxes WHERE name = $1 FOR UPDATE`, oldName,
		).Scan(&id, &stillQuiesced); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errMailboxNotFound
			}
			return fmt.Errorf("lookup: %w", err)
		}
		// Re-checked inside the transaction: maintenance could have been
		// released between the precondition check and here, which would mean
		// writers are live again and the tree is moving under them.
		if stillQuiesced == nil {
			return errMailboxNotQuiesced
		}

		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM folders WHERE mailbox_id = $1`, id,
		).Scan(&folderCount); err != nil {
			return fmt.Errorf("count folders: %w", err)
		}
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM messages WHERE folder_id IN (SELECT id FROM folders WHERE mailbox_id = $1)`, id,
		).Scan(&messageCount); err != nil {
			return fmt.Errorf("count messages: %w", err)
		}
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM aliases WHERE mailbox_id = $1`, id,
		).Scan(&aliasCount); err != nil {
			return fmt.Errorf("count aliases: %w", err)
		}

		// Release the barrier in the same transaction that commits the name:
		// the disagreement between database and filesystem ends exactly here,
		// so there is no moment where writers are live and the row is stale.
		if _, err := tx.Exec(ctx,
			`UPDATE mailboxes SET name = $1, maintenance_at = NULL, updated_at = now() WHERE id = $2`,
			newName, id,
		); err != nil {
			if isUniqueViolation(err) {
				return errRenameTargetExists
			}
			return fmt.Errorf("rename: %w", err)
		}

		// Token scope needs no rewrite: it is stored as durable mailbox IDs
		// (migration 012, RA6X-012), and this rename does not change the ID.
		// A token scoped to this account stays scoped to this account, which
		// is what "scoped to this mailbox" always meant — and, unlike the old
		// name rewrite, there is no window in which the old name is briefly
		// unclaimed and a fast recreate could inherit the grant.
		//
		// The count is reported so the operator still sees the blast radius.
		rows, err := tx.Query(ctx, `
			SELECT name FROM api_tokens
			 WHERE revoked_at IS NULL AND $1 = ANY(scope_mailbox_ids)
			 ORDER BY id`, id)
		if err != nil {
			return fmt.Errorf("list scoped tokens: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return fmt.Errorf("scan token: %w", err)
			}
			rescopedTokens = append(rescopedTokens, n)
		}
		return rows.Err()
	})
	switch {
	case errors.Is(err, errMailboxNotFound):
		fmt.Fprintf(os.Stderr, "mailbox %q not found\n", oldName)
		return EX_USAGE
	case errors.Is(err, errRenameTargetExists):
		fmt.Fprintf(os.Stderr, "mailbox %q already exists\n", newName)
		return EX_USAGE
	case errors.Is(err, errMailboxNotQuiesced):
		fmt.Fprintf(os.Stderr,
			"refusing: maintenance on %q was released while this command ran; re-run after setting it again\n",
			oldName)
		return EX_USAGE
	case err != nil:
		fmt.Fprintf(os.Stderr, "rename: %v\n", err)
		return EX_TEMPFAIL
	}

	fmt.Printf("renamed mailbox %q -> %q (carried: %d folders, %d messages, %d aliases); maintenance released\n",
		oldName, newName, folderCount, messageCount, aliasCount)
	if len(rescopedTokens) > 0 {
		fmt.Printf("%d live API token(s) follow this mailbox unchanged: %s\n",
			len(rescopedTokens), strings.Join(rescopedTokens, ", "))
	} else {
		fmt.Println("no live API token is scoped to this mailbox")
	}
	fmt.Printf("IMAP clients must now authenticate as %q — update every MUA profile and any epistula-imap credential store.\n", newName)
	return EX_OK
}

// adminDomainList prints every domain and its routing state.
func adminDomainList(args []string) int {
	fs := flag.NewFlagSet("admin domain-list", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	if err := fs.Parse(args); err != nil {
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rows, err := db.Pool().Query(ctx, `
		SELECT d.id, d.name, d.is_wildcard, d.created_at,
		       (SELECT count(*) FROM aliases   WHERE domain_id = d.id)   AS alias_count,
		       (SELECT count(*) FROM domain_acl WHERE domain_id = d.id AND kind = 'allow') AS allow_count,
		       (SELECT count(*) FROM domain_acl WHERE domain_id = d.id AND kind = 'deny')  AS deny_count
		  FROM domains d
		 ORDER BY d.name`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "query: %v\n", err)
		return EX_TEMPFAIL
	}
	defer rows.Close()

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tWILDCARD\tALIASES\tALLOW\tDENY\tCREATED")
	count := 0
	for rows.Next() {
		var (
			id                              int64
			name                            string
			wildcard                        bool
			created                         time.Time
			aliasCount, allowCount, denyCnt int64
		)
		if err := rows.Scan(&id, &name, &wildcard, &created, &aliasCount, &allowCount, &denyCnt); err != nil {
			fmt.Fprintf(os.Stderr, "scan: %v\n", err)
			return EX_TEMPFAIL
		}
		fmt.Fprintf(tw, "%d\t%s\t%t\t%d\t%d\t%d\t%s\n",
			id, name, wildcard, aliasCount, allowCount, denyCnt, created.UTC().Format("2006-01-02"))
		count++
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "rows: %v\n", err)
		return EX_TEMPFAIL
	}
	if err := tw.Flush(); err != nil {
		return EX_IOERR
	}
	if count == 0 {
		fmt.Println("(no domains)")
	}
	return EX_OK
}

// adminDomainDelete drops a domain (cascading aliases and ACL rows). Does
// NOT drop mailboxes (mailboxes are independent of domains in this schema).
func adminDomainDelete(args []string) int {
	fs := flag.NewFlagSet("admin domain-delete", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	name := fs.String("name", "", "Domain name (required)")
	yes := fs.Bool("yes", false, "Confirm the destructive operation")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	if *name == "" {
		fmt.Fprintln(os.Stderr, "-name is required")
		return EX_USAGE
	}
	if !*yes {
		fmt.Fprintln(os.Stderr, "domain-delete is destructive; pass -yes to confirm")
		return EX_USAGE
	}
	normalized := normalizeDomain(*name)

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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var aliasCount, aclCount int64
	_ = db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM aliases WHERE domain_id = (SELECT id FROM domains WHERE name = $1)`,
		normalized,
	).Scan(&aliasCount)
	_ = db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM domain_acl WHERE domain_id = (SELECT id FROM domains WHERE name = $1)`,
		normalized,
	).Scan(&aclCount)

	tag, err := db.Pool().Exec(ctx, `DELETE FROM domains WHERE name = $1`, normalized)
	if err != nil {
		fmt.Fprintf(os.Stderr, "delete: %v\n", err)
		return EX_TEMPFAIL
	}
	if tag.RowsAffected() == 0 {
		fmt.Fprintf(os.Stderr, "domain %q not found\n", normalized)
		return EX_USAGE
	}
	fmt.Printf("deleted domain %q (cascaded: %d aliases, %d acl rows)\n",
		normalized, aliasCount, aclCount)
	return EX_OK
}

// adminAliasList prints aliases optionally filtered by domain.
func adminAliasList(args []string) int {
	fs := flag.NewFlagSet("admin alias-list", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	domain := fs.String("domain", "", "Restrict to this domain")
	if err := fs.Parse(args); err != nil {
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	query := `SELECT a.id, d.name, a.localpart, m.name, a.created_at
	            FROM aliases a
	            JOIN domains d   ON d.id = a.domain_id
	            JOIN mailboxes m ON m.id = a.mailbox_id`
	argsSQL := []any{}
	if *domain != "" {
		argsSQL = append(argsSQL, normalizeDomain(*domain))
		query += " WHERE d.name = $1"
	}
	query += " ORDER BY d.name, a.localpart"

	rows, err := db.Pool().Query(ctx, query, argsSQL...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "query: %v\n", err)
		return EX_TEMPFAIL
	}
	defer rows.Close()

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tDOMAIN\tLOCALPART\tMAILBOX\tCREATED")
	count := 0
	for rows.Next() {
		var (
			id         int64
			dn, lp, mb string
			created    time.Time
		)
		if err := rows.Scan(&id, &dn, &lp, &mb, &created); err != nil {
			fmt.Fprintf(os.Stderr, "scan: %v\n", err)
			return EX_TEMPFAIL
		}
		display := lp
		if lp == "" {
			display = "(catchall)"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n",
			id, dn, display, mb, created.UTC().Format("2006-01-02"))
		count++
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "rows: %v\n", err)
		return EX_TEMPFAIL
	}
	if err := tw.Flush(); err != nil {
		return EX_IOERR
	}
	if count == 0 {
		fmt.Println("(no aliases)")
	}
	return EX_OK
}

// adminAliasDelete removes a single alias by (domain, localpart). Use
// -catchall for the localpart=” slot.
func adminAliasDelete(args []string) int {
	fs := flag.NewFlagSet("admin alias-delete", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	domain := fs.String("domain", "", "Domain name (required)")
	localpart := fs.String("localpart", "", "Localpart")
	catchall := fs.Bool("catchall", false, "Target the catchall (localpart='')")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	if *domain == "" {
		fmt.Fprintln(os.Stderr, "-domain is required")
		return EX_USAGE
	}
	if *catchall == (*localpart != "") {
		// either both set or neither set
		fmt.Fprintln(os.Stderr, "exactly one of -localpart or -catchall must be set")
		return EX_USAGE
	}
	lp := ""
	if !*catchall {
		// Same normalization as alias-add and the delivery resolver, so the
		// spelling that created a row always deletes it (R-006).
		lp = norm.NFC.String(strings.ToLower(strings.TrimSpace(*localpart)))
	}
	domainNorm := normalizeDomain(*domain)

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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tag, err := db.Pool().Exec(ctx,
		`DELETE FROM aliases
		  WHERE domain_id = (SELECT id FROM domains WHERE name = $1)
		    AND localpart = $2`,
		domainNorm, lp,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "delete: %v\n", err)
		return EX_TEMPFAIL
	}
	if tag.RowsAffected() == 0 {
		fmt.Fprintln(os.Stderr, "no matching alias")
		return EX_USAGE
	}
	display := lp
	if lp == "" {
		display = "(catchall)"
	}
	fmt.Printf("deleted alias %s@%s\n", display, domainNorm)
	return EX_OK
}

// adminFolderList prints every folder owned by a mailbox, with message
// count and the IMAP UID counters. Useful for debugging "why doesn't this
// folder show up in Mail.app".
func adminFolderList(args []string) int {
	fs := flag.NewFlagSet("admin folder-list", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	mailbox := fs.String("mailbox", "", "Mailbox name (required)")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	// Match the canonical stored name (lower + trimmed) so case-variant input
	// resolves (R-048).
	*mailbox = strings.ToLower(strings.TrimSpace(*mailbox))
	if *mailbox == "" {
		fmt.Fprintln(os.Stderr, "-mailbox is required")
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rows, err := db.Pool().Query(ctx, `
		SELECT f.id, f.name, f.uidvalidity, f.uidnext,
		       COALESCE(f.special_use, ''),
		       (SELECT count(*) FROM messages WHERE folder_id = f.id) AS msg_count
		  FROM folders f
		 WHERE f.mailbox_id = (SELECT id FROM mailboxes WHERE name = $1)
		 ORDER BY f.name`, *mailbox)
	if err != nil {
		fmt.Fprintf(os.Stderr, "query: %v\n", err)
		return EX_TEMPFAIL
	}
	defer rows.Close()

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tUIDVALIDITY\tUIDNEXT\tSPECIAL-USE\tMESSAGES")
	count := 0
	for rows.Next() {
		var (
			id, uidValidity, uidNext, msgCount int64
			name, specialUse                   string
		)
		if err := rows.Scan(&id, &name, &uidValidity, &uidNext, &specialUse, &msgCount); err != nil {
			fmt.Fprintf(os.Stderr, "scan: %v\n", err)
			return EX_TEMPFAIL
		}
		fmt.Fprintf(tw, "%d\t%s\t%d\t%d\t%s\t%d\n",
			id, name, uidValidity, uidNext, specialUse, msgCount)
		count++
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "rows: %v\n", err)
		return EX_TEMPFAIL
	}
	if err := tw.Flush(); err != nil {
		return EX_IOERR
	}
	if count == 0 {
		fmt.Println("(no folders — mailbox may not exist or has never received mail)")
	}
	return EX_OK
}

// adminLogTail paginates delivery_log with optional filters. The most
// recent entries print last so a tty user sees them at the bottom (matches
// what `tail -F` gives you on a logfile).
func adminLogTail(args []string) int {
	fs := flag.NewFlagSet("admin log-tail", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	since := fs.Duration("since", 0, "Restrict to entries received in the last DUR (e.g. 1h, 24h)")
	mailbox := fs.String("mailbox", "", "Restrict to deliveries for this mailbox")
	outcome := fs.String("outcome", "", "Restrict to a specific outcome (delivered, rejected:denylist, ...)")
	limit := fs.Int("limit", 50, "Max rows to return")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	// The -mailbox filter matches the canonical stored name (R-048); empty
	// stays empty (no filter).
	*mailbox = strings.ToLower(strings.TrimSpace(*mailbox))
	if *limit <= 0 || *limit > 1000 {
		fmt.Fprintln(os.Stderr, "-limit must be in (0, 1000]")
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conds := []string{}
	argsSQL := []any{}
	if *since > 0 {
		argsSQL = append(argsSQL, time.Now().Add(-*since))
		conds = append(conds, fmt.Sprintf("dl.received_at >= $%d", len(argsSQL)))
	}
	if *mailbox != "" {
		argsSQL = append(argsSQL, *mailbox)
		conds = append(conds, fmt.Sprintf("dl.matched_mailbox_id = (SELECT id FROM mailboxes WHERE name = $%d)", len(argsSQL)))
	}
	if *outcome != "" {
		argsSQL = append(argsSQL, *outcome)
		conds = append(conds, fmt.Sprintf("dl.outcome = $%d", len(argsSQL)))
	}
	query := `SELECT dl.received_at, COALESCE(dl.envelope_from, ''), dl.envelope_to,
	                 COALESCE(m.name, ''), dl.bytes, dl.outcome,
	                 COALESCE(dl.error_detail, ''), COALESCE(dl.raw_sha256_short, '')
	            FROM delivery_log dl
	            LEFT JOIN mailboxes m ON m.id = dl.matched_mailbox_id`
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	// ORDER DESC + reverse later so the most recent prints last.
	argsSQL = append(argsSQL, *limit)
	query += fmt.Sprintf(" ORDER BY dl.received_at DESC LIMIT $%d", len(argsSQL))

	rows, err := db.Pool().Query(ctx, query, argsSQL...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "query: %v\n", err)
		return EX_TEMPFAIL
	}
	defer rows.Close()

	type row struct {
		received                                   time.Time
		from, to, mailbox, outcome, detail, shaHex string
		bytes                                      int64
	}
	var entries []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.received, &r.from, &r.to, &r.mailbox, &r.bytes, &r.outcome, &r.detail, &r.shaHex); err != nil {
			fmt.Fprintf(os.Stderr, "scan: %v\n", err)
			return EX_TEMPFAIL
		}
		entries = append(entries, r)
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "rows: %v\n", err)
		return EX_TEMPFAIL
	}

	// Reverse so oldest prints first, newest last.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RECEIVED\tFROM\tTO\tMAILBOX\tBYTES\tOUTCOME\tSHA16\tDETAIL")
	for _, r := range entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			r.received.UTC().Format("2006-01-02T15:04:05Z"),
			truncate(r.from, 32), truncate(r.to, 32), r.mailbox,
			r.bytes, r.outcome, r.shaHex, truncate(r.detail, 40))
	}
	if err := tw.Flush(); err != nil {
		return EX_IOERR
	}
	if len(entries) == 0 {
		fmt.Println("(no matching log entries)")
	}
	return EX_OK
}

// truncate clips s to at most n runes; an ellipsis replaces the tail.
func truncate(s string, n int) string {
	if n <= 1 || len(s) <= n {
		return s
	}
	// len(s) is a byte count; slicing s[:n-1] can split a multibyte rune and
	// emit an invalid UTF-8 sequence in the log-tail table (envelope addresses
	// are internationalizable). Cut on rune count instead (R-051). The fast
	// path above already covers strings that fit in n bytes; a string longer
	// than n bytes may still be <= n runes, so re-check after decoding.
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
