package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ptudor/epistula-mail/database/auth"
)

// errUnknownMailboxScope marks a -mailboxes entry that names no mailbox, so
// the transaction's failure can be reported as an operator typo (EX_USAGE)
// rather than a backend problem.
var errUnknownMailboxScope = errors.New("unknown mailbox scope")

// formatMailboxIDs renders a scope's durable IDs for the operator, so the
// value actually stored is visible next to the names that produced it.
func formatMailboxIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ", ")
}

// API-token lifecycle for api. epistula-database owns the
// schema and the mint/revoke surface; epistula-api only ever verifies. The
// secret is printed exactly once at mint time — only its Argon2id hash is
// stored, so a lost token is replaced, never recovered.

// permissionListFlag collects repeatable -permission flags.
type permissionListFlag []string

func (p *permissionListFlag) String() string { return strings.Join(*p, ",") }

func (p *permissionListFlag) Set(v string) error {
	v = strings.TrimSpace(v)
	if !auth.ValidPermission(v) {
		return fmt.Errorf("unknown permission %q (valid: %s, %s, %s, %s)",
			v, auth.PermissionReadMetadata, auth.PermissionReadContent, auth.PermissionWriteAnnotation,
			auth.PermissionWriteClassification)
	}
	for _, existing := range *p {
		if existing == v {
			return nil // duplicates collapse silently
		}
	}
	*p = append(*p, v)
	return nil
}

func adminAPITokenAdd(args []string) int {
	fs := flag.NewFlagSet("admin api-token-add", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	name := fs.String("name", "", "Token name (required; unique among live tokens)")
	mailboxes := fs.String("mailboxes", "", `Comma-separated mailbox names the token may touch, or "*" for all (required)`)
	var perms permissionListFlag
	fs.Var(&perms, "permission", "Permission level (repeatable): read_metadata | read_content | write_annotation | write_classification")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	if *name == "" {
		fmt.Fprintln(os.Stderr, "-name is required")
		return EX_USAGE
	}
	if *mailboxes == "" {
		fmt.Fprintln(os.Stderr, `-mailboxes is required ("*" or a comma-separated list)`)
		return EX_USAGE
	}
	if len(perms) == 0 {
		fmt.Fprintln(os.Stderr, "at least one -permission is required")
		return EX_USAGE
	}
	scope, code := parseTokenScope(*mailboxes)
	if code != EX_OK {
		return code
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	secret, err := auth.GenerateAPITokenSecret()
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate secret: %v\n", err)
		return EX_SOFTWARE
	}
	hash, err := auth.HashPassword(secret, auth.Params{
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

	// Resolve the requested names to durable mailbox IDs and insert in ONE
	// transaction, with the mailbox rows locked FOR SHARE (RA6X-012).
	//
	// Scope is stored as IDs, so a typo must be caught here — it would
	// otherwise surface much later as a mystifying 403 in epistula-api. The lock
	// closes the check-then-insert race with `admin mailbox-delete`: without
	// it, a mailbox could be deleted between resolving its ID and committing
	// the token, minting a credential scoped to an account that no longer
	// exists. FOR SHARE (not FOR UPDATE) lets concurrent token creations for
	// the same mailbox proceed together while still blocking the delete.
	allMailboxes := scope[0] == "*"
	var (
		id int64
		// Non-nil so a wildcard token binds an empty array, not NULL: the
		// column is NOT NULL.
		scopeIDs   = []int64{}
		scopeNames []string
	)
	err = db.RunTx(ctx, func(tx pgx.Tx) error {
		if !allMailboxes {
			rows, err := tx.Query(ctx,
				`SELECT id, name FROM mailboxes WHERE name = ANY($1::text[]) ORDER BY id FOR SHARE`,
				scope)
			if err != nil {
				return fmt.Errorf("mailbox lookup: %w", err)
			}
			defer rows.Close()
			found := make(map[string]struct{}, len(scope))
			for rows.Next() {
				var mid int64
				var mname string
				if err := rows.Scan(&mid, &mname); err != nil {
					return fmt.Errorf("mailbox scan: %w", err)
				}
				scopeIDs = append(scopeIDs, mid)
				scopeNames = append(scopeNames, mname)
				found[mname] = struct{}{}
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("mailbox rows: %w", err)
			}
			var missing []string
			for _, want := range scope {
				if _, ok := found[want]; !ok {
					missing = append(missing, want)
				}
			}
			if len(missing) > 0 {
				return fmt.Errorf("%w: %s", errUnknownMailboxScope, strings.Join(missing, ", "))
			}
		}
		return tx.QueryRow(ctx,
			`INSERT INTO api_tokens (name, token_hash, scope_all_mailboxes, scope_mailbox_ids, permissions)
			 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			*name, hash, allMailboxes, scopeIDs, []string(perms),
		).Scan(&id)
	})
	if err != nil {
		if errors.Is(err, errUnknownMailboxScope) {
			fmt.Fprintf(os.Stderr, "unknown mailbox(es): %s\n",
				strings.TrimPrefix(err.Error(), errUnknownMailboxScope.Error()+": "))
			return EX_USAGE
		}
		if isUniqueViolation(err) {
			fmt.Fprintf(os.Stderr, "a live token named %q already exists (revoke it first)\n", *name)
			return EX_USAGE
		}
		fmt.Fprintf(os.Stderr, "insert: %v\n", err)
		return EX_TEMPFAIL
	}

	fmt.Printf("created api token %q (id=%d)\n", *name, id)
	if allMailboxes {
		fmt.Printf("  mailboxes:   * (all)\n")
	} else {
		fmt.Printf("  mailboxes:   %s\n", strings.Join(scopeNames, ", "))
		fmt.Printf("  mailbox ids: %s\n", formatMailboxIDs(scopeIDs))
	}
	fmt.Printf("  permissions: %s\n", strings.Join(perms, ", "))
	fmt.Println()
	fmt.Printf("  %s\n", auth.FormatAPIToken(id, secret))
	fmt.Println()
	fmt.Println("This token is shown ONCE and stored only as a hash. Copy it now.")
	return EX_OK
}

// parseTokenScope normalizes the -mailboxes argument: "*" alone, or a
// comma-separated list of non-empty names. Mixing "*" with names is an
// operator error, not a wider scope.
func parseTokenScope(arg string) ([]string, int) {
	trimmed := strings.TrimSpace(arg)
	if trimmed == "*" {
		return []string{"*"}, EX_OK
	}
	parts := strings.Split(trimmed, ",")
	seen := make(map[string]struct{}, len(parts))
	scope := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			fmt.Fprintln(os.Stderr, "-mailboxes contains an empty name")
			return nil, EX_USAGE
		}
		if p == "*" {
			fmt.Fprintln(os.Stderr, `"*" cannot be combined with explicit mailbox names`)
			return nil, EX_USAGE
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		scope = append(scope, p)
	}
	if len(scope) == 0 {
		fmt.Fprintln(os.Stderr, "-mailboxes resolved to an empty list")
		return nil, EX_USAGE
	}
	return scope, EX_OK
}

func adminAPITokenList(args []string) int {
	fs := flag.NewFlagSet("admin api-token-list", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	all := fs.Bool("all", false, "Include revoked tokens")
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

	// Scope is stored as durable IDs; render the names they currently resolve
	// to. An ID with no mailbox is shown as <deleted:N> rather than omitted,
	// so an operator can see that a token's scope has outlived its account
	// instead of reading a suspiciously short list (RA6X-012).
	query := `SELECT t.id, t.name,
	                 t.scope_all_mailboxes,
	                 COALESCE((
	                   SELECT array_agg(COALESCE(m.name, '<deleted:' || sid || '>') ORDER BY sid)
	                     FROM unnest(t.scope_mailbox_ids) AS sid
	                     LEFT JOIN mailboxes m ON m.id = sid
	                 ), '{}') AS scope_names,
	                 t.permissions, t.created_at, t.last_used_at, t.revoked_at
	            FROM api_tokens t`
	if !*all {
		query += ` WHERE revoked_at IS NULL`
	}
	query += ` ORDER BY id`

	rows, err := db.Pool().Query(ctx, query)
	if err != nil {
		fmt.Fprintf(os.Stderr, "query: %v\n", err)
		return EX_TEMPFAIL
	}
	defer rows.Close()

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tMAILBOXES\tPERMISSIONS\tCREATED\tLAST USED\tSTATE")
	count := 0
	for rows.Next() {
		var (
			id                 int64
			name               string
			allMailboxes       bool
			scope, permissions []string
			created            time.Time
			lastUsed, revoked  *time.Time
		)
		if err := rows.Scan(&id, &name, &allMailboxes, &scope, &permissions, &created, &lastUsed, &revoked); err != nil {
			fmt.Fprintf(os.Stderr, "scan: %v\n", err)
			return EX_TEMPFAIL
		}
		if allMailboxes {
			scope = []string{"*"}
		}
		state := "live"
		if revoked != nil {
			state = "revoked " + revoked.UTC().Format("2006-01-02")
		}
		lastUsedStr := "never"
		if lastUsed != nil {
			lastUsedStr = lastUsed.UTC().Format("2006-01-02")
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			id, name, strings.Join(scope, ","), strings.Join(permissions, ","),
			created.UTC().Format("2006-01-02"), lastUsedStr, state)
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
		fmt.Println("(no api tokens)")
	}
	return EX_OK
}

func adminAPITokenRevoke(args []string) int {
	fs := flag.NewFlagSet("admin api-token-revoke", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	name := fs.String("name", "", "Token name (required)")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
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

	tag, err := db.Pool().Exec(ctx,
		`UPDATE api_tokens SET revoked_at = now() WHERE name = $1 AND revoked_at IS NULL`,
		*name,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: %v\n", err)
		return EX_TEMPFAIL
	}
	if tag.RowsAffected() == 0 {
		fmt.Fprintf(os.Stderr, "no live token named %q\n", *name)
		return EX_USAGE
	}
	fmt.Printf("revoked api token %q (epistula-api rejects it within its verify-cache TTL)\n", *name)
	return EX_OK
}
