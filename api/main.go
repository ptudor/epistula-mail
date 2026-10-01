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
	case "serve":
		return runServe(rest)
	case "check-config":
		return runCheckConfig(rest)
	case "version", "-v", "--version":
		fmt.Printf("epistula-api %s (built %s)\n", Version, BuildTime)
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
	fmt.Fprintln(os.Stderr, `epistula-api - HTTP/JSON read API over the epistula-database store

Usage:
  epistula-api <subcommand> [flags]

Subcommands:
  serve         Run the API listener and the admin (metrics/health) listener.
  check-config  Validate config file and exit.
  version       Print version and exit.
  help          Show this help.

Global flags (per subcommand):
  -config <path>    Path to TOML config file.

Token lifecycle (mint/list/revoke) lives in epistula-database:
  epistula-database admin api-token-add -name N -mailboxes "*" -permission read_content

See ./epistula-api.toml.example for a full config reference.`)
}
