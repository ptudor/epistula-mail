package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"
)

func adminDomainSet(args []string) int {
	fs := flag.NewFlagSet("admin domain-set", flag.ContinueOnError)
	config := fs.String("config", "", "Path to TOML config file")
	name := fs.String("name", "", "Domain name (required)")
	wildcard := fs.Bool("wildcard", false, "Enable wildcard")
	noWildcard := fs.Bool("no-wildcard", false, "Disable wildcard")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	if fs.NArg() != 0 || *wildcard == *noWildcard {
		fmt.Fprintln(os.Stderr, "specify exactly one of -wildcard or -no-wildcard")
		return EX_USAGE
	}
	domain := normalizeDomain(*name)
	if err := validateDomainName(domain); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return EX_USAGE
	}
	cfg, err := LoadConfig(*config)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return EX_CONFIG
	}
	db, code := adminOpenDB(cfg)
	if code != EX_OK {
		return code
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tag, err := db.Pool().Exec(ctx, `UPDATE domains SET is_wildcard=$2 WHERE name=$1`, domain, *wildcard)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return EX_TEMPFAIL
	}
	if tag.RowsAffected() == 0 {
		fmt.Fprintf(os.Stderr, "domain %q not found\n", domain)
		return EX_USAGE
	}
	fmt.Printf("updated domain %q (wildcard=%v)\n", domain, *wildcard)
	return EX_OK
}
