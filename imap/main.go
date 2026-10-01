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
		fmt.Printf("epistula-imap %s (built %s)\n", Version, BuildTime)
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
	fmt.Fprintln(os.Stderr, `epistula-imap - IMAP4 server backed by the epistula-database store

Usage:
  epistula-imap <subcommand> [flags]

Subcommands:
  serve         Run the IMAP server (TLS 993) and admin HTTP listener.
  check-config  Validate config file and exit.
  version       Print version and exit.
  help          Show this help.

Global flags (per subcommand):
  -config <path>    Path to TOML config file.

See ./epistula-imap.toml.example for a full config reference.`)
}
