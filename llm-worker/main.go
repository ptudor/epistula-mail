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
	case "run-once":
		return runOnce(rest)
	case "check-config":
		return runCheckConfig(rest)
	case "version", "-v", "--version":
		fmt.Printf("epistula-llm-worker %s (built %s)\n", Version, BuildTime)
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
	fmt.Fprintln(os.Stderr, `epistula-llm-worker - Epistula model annotation worker

Usage:
  epistula-llm-worker <subcommand> [flags]

Subcommands:
  serve         Run continuously, polling epistula-api and annotating messages.
  run-once      Process one export pass and exit.
  check-config  Validate config file and exit.
  version       Print version and exit.
  help          Show this help.

Global flags (per subcommand):
  -config <path>    Path to TOML config file.

epistula-api token lifecycle:
  epistula-database admin api-token-add -name llm-worker -mailboxes "*" \
    -permission read_content -permission write_annotation \
    -permission write_classification   # only with [classify] enabled

See ./epistula-llm-worker.toml.example for a full config reference.`)
}
