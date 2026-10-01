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
	if _, err := LoadConfig(*configPath); err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return EX_CONFIG
	}
	fmt.Fprintln(os.Stdout, "config ok")
	return EX_OK
}
