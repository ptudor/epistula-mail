package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/ptudor/epistula-mail/database/migrations"
	"github.com/ptudor/epistula-mail/database/storage"
)

// migrateUpContext builds the run context for `migrate up`. A zero or negative
// timeout means no deadline (for a very large CONCURRENT-style build that may
// legitimately run for hours); otherwise the whole run is bounded. Extracted so
// the timeout policy is unit-testable without a live database.
func migrateUpContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(parent, timeout)
	}
	return context.WithCancel(parent)
}

// runMigrate dispatches the `migrate` subcommand:
//
//	epistula-database migrate status   — list applied vs. pending migrations
//	epistula-database migrate up       — apply every pending migration
func runMigrate(args []string) int {
	if len(args) == 0 {
		printMigrateUsage()
		return EX_USAGE
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "status":
		return migrateStatus(rest)
	case "up":
		return migrateUp(rest)
	case "help", "-h", "--help":
		printMigrateUsage()
		return EX_OK
	default:
		fmt.Fprintf(os.Stderr, "migrate: unknown subcommand %q\n\n", sub)
		printMigrateUsage()
		return EX_USAGE
	}
}

func printMigrateUsage() {
	fmt.Fprintln(os.Stderr, `migrate subcommands:
  status   List applied and pending migrations.
  up       Apply every pending migration in order. Each migration runs in its
           own transaction; a failure aborts the run and leaves prior
           migrations applied. Accepts -timeout <dur> (default 1h, 0 disables)
           to bound the whole run — raise it for large index-building
           migrations on hot tables.

All subcommands accept -config <path>.`)
}

func migrateStatus(args []string) int {
	cfg, code := loadMigrateConfig(args, "status")
	if code != EX_OK {
		return code
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, code := openMigrateDB(ctx, cfg)
	if code != EX_OK {
		return code
	}
	defer db.Close()

	all, err := migrations.All()
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate status: %v\n", err)
		return EX_SOFTWARE
	}
	applied, err := migrations.AppliedVersions(ctx, db.Pool())
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate status: %v\n", err)
		return EX_TEMPFAIL
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "VERSION\tSTATE\tDESCRIPTION")
	for _, m := range all {
		state := "pending"
		if _, ok := applied[m.Version]; ok {
			state = "applied"
		}
		fmt.Fprintf(w, "%03d\t%s\t%s\n", m.Version, state, m.Description)
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "migrate status: write output: %v\n", err)
		return EX_IOERR
	}
	return EX_OK
}

func migrateUp(args []string) int {
	fs := flag.NewFlagSet("migrate up", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	// The old 5-minute hard cap could permanently wedge `migrate up`: a future
	// index-adding migration on a multi-million-row messages table can exceed
	// it, after which the ctx cancel rolls the migration back and the command
	// can never complete without a code change (R-050). Default to a generous
	// hour and let operators disable the bound entirely with -timeout 0 for a
	// very large build. Index-adding migrations on hot tables should ship as
	// `CREATE INDEX CONCURRENTLY` in a non-transactional migration mode (a
	// "no-tx" migration flag) so they don't lock out concurrent deliveries;
	// that mode is future work — the runner is still one-tx-per-migration.
	timeout := fs.Duration("timeout", time.Hour,
		"Overall time budget for the whole run; 0 disables the bound (raise for large index builds).")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return EX_CONFIG
	}

	ctx, cancel := migrateUpContext(context.Background(), *timeout)
	defer cancel()

	db, code := openMigrateDB(ctx, cfg)
	if code != EX_OK {
		return code
	}
	defer db.Close()

	applied, err := migrations.Apply(ctx, db.Pool())
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate up: %v\n", err)
		if len(applied) > 0 {
			fmt.Fprintf(os.Stderr, "%d migration(s) succeeded before failure\n", len(applied))
		}
		return EX_TEMPFAIL
	}
	if len(applied) == 0 {
		fmt.Println("no pending migrations")
		return EX_OK
	}
	for _, m := range applied {
		fmt.Printf("applied %03d %s\n", m.Version, m.Description)
	}
	return EX_OK
}

func loadMigrateConfig(args []string, sub string) (*Config, int) {
	fs := flag.NewFlagSet("migrate "+sub, flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	if err := fs.Parse(args); err != nil {
		return nil, EX_USAGE
	}
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return nil, EX_CONFIG
	}
	return cfg, EX_OK
}

func openMigrateDB(ctx context.Context, cfg *Config) (*storage.DB, int) {
	db, err := storage.Open(ctx, storage.Config{
		DSN:              cfg.Postgres.DSN,
		StatementTimeout: cfg.StatementTimeoutDuration(),
		// Migrations run serially in one connection; like admin, this is a
		// one-shot path that pins a tiny pool rather than reading the
		// long-lived-pool knobs (RO5X-021).
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
