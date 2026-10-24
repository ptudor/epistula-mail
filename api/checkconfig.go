package main

import (
	"flag"
	"fmt"
	"os"
)

func runCheckConfig(args []string) int {
	fs := flag.NewFlagSet("check-config", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "check-config: %v\n", err)
		return EX_CONFIG
	}

	mode := "development"
	if cfg.Production {
		mode = "production"
	}
	submission := "off"
	if cfg.Submission.Enabled() {
		submission = fmt.Sprintf("%s as %d mailbox(es)", cfg.Submission.Address, len(cfg.Submission.Accounts))
	}
	fmt.Printf("epistula-api: config OK (mode=%s, api=%s, admin=%s, submission=%s)\n",
		mode, cfg.Server.ListenAddr, cfg.Admin.ListenAddr, submission)
	return EX_OK
}
