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
	tlsVer, err := cfg.MinTLSVersion()
	if err != nil {
		// Validate() already rejects bad versions, but a config checker
		// must never shrug off its own checks.
		fmt.Fprintf(os.Stderr, "check-config: %v\n", err)
		return EX_CONFIG
	}
	fmt.Printf("epistula-imap: config OK (mode=%s, imap=%s, admin=%s, min_tls=0x%x)\n",
		mode, cfg.Server.ListenAddr, cfg.Admin.ListenAddr, tlsVer)
	return EX_OK
}
