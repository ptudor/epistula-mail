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
	fmt.Printf("epistula-api: config OK (mode=%s, api=%s, admin=%s)\n",
		mode, cfg.Server.ListenAddr, cfg.Admin.ListenAddr)
	return EX_OK
}
