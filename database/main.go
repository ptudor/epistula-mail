package main

import (
	"fmt"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printUsage()
		return EX_USAGE
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "deliver":
		return runDeliver(rest)
	case "serve":
		return runServe(rest)
	case "import":
		return runImport(rest)
	case "import-blobs":
		return runImportBlobs(rest)
	case "import-verify":
		return runImportVerify(rest)
	case "reparse-bodystructure":
		return runReparseBodystructure(rest)
	case "gc":
		return runGC(rest)
	case "admin":
		return runAdmin(rest)
	case "migrate":
		return runMigrate(rest)
	case "check-config":
		return runCheckConfig(rest)
	case "version", "-v", "--version":
		fmt.Printf("epistula-database %s (built %s)\n", Version, BuildTime)
		return EX_OK
	case "help", "-h", "--help":
		printUsage()
		return EX_OK
	default:
		fmt.Fprintf(os.Stderr, "Unknown subcommand: %s\n\n", cmd)
		printUsage()
		return EX_USAGE
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `epistula-database - Postgres-backed mail delivery agent and store

Usage:
  epistula-database <subcommand> [flags]

Subcommands:
  deliver       Read RFC 5322 message from stdin, store in Postgres + blob tree.
                Invoked by Postfix pipe transport.
  serve         Run health checks + Prometheus exporter.
  import        Migrate a Maildir tree into the store.
  import-blobs  Disaster recovery: re-ingest the raw/yyyy/mm/dd blob tree into a
                mailbox after Postgres loss. Folder placement and flags are not
                recoverable from disk; prefer a pg_dump restore when one exists.
  import-verify Verify a prior import: every Maildir file → PG row, every row → blob.
  reparse-bodystructure
                Re-derive messages.bodystructure from each row's raw blob.
                Pure derived data, idempotent, safe to run hot. Use after a
                parser change alters the computed structure.
  gc            Garbage-collect orphan blobs (-phase mark|sweep|reconcile-quotas).
  admin         CLI mailbox/domain/alias/ACL management (see admin help).
  migrate       Apply schema migrations (status | up).
  check-config  Validate config file and exit.
  version       Print version and exit.
  help          Show this help.

Global flags (per subcommand):
  -config <path>    Path to TOML config file.

See ./epistula-database.toml.example for a full config reference.`)
}
