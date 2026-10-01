package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/ptudor/epistula-mail/database/auth"
)

// adminAuthCostAudit reports the Argon2id cost carried by every stored
// credential — mailbox passwords and API tokens — grouped by parameters
// (RA6X-060).
//
// The IMAP server equalizes login timing by padding every outcome to a floor
// calibrated against the default cost. That closes the account-existence
// oracle for accounts hashed at or below the default, and cannot close it for
// accounts hashed above it: no amount of padding hides work that takes longer
// than the pad. Those accounts must be re-hashed, and this is how an operator
// finds them without waiting for someone to log in and trip the runtime warning.
//
// The encoded hash carries its own parameters, so this is a pure read — it
// verifies nothing, computes no KDF, and never prints a hash.
func adminAuthCostAudit(args []string) int {
	fs := flag.NewFlagSet("admin auth-cost-audit", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	verbose := fs.Bool("verbose", false, "List every account rather than grouping by cost")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return EX_CONFIG
	}
	db, code := adminOpenDB(cfg)
	if code != EX_OK {
		return code
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	type costKey struct {
		Memory     uint32
		Iterations uint32
		Parallel   uint8
		KeyLen     int
	}
	type costGroup struct {
		key      costKey
		mailbox  []string
		token    []string
		invalid  []string
		unparsed int
	}

	groups := map[costKey]*costGroup{}
	var invalid []string

	record := func(kind, name, hash string) {
		p, err := auth.ParseEncodedParams(hash)
		if err != nil {
			invalid = append(invalid, fmt.Sprintf("%s %s: %v", kind, name, err))
			return
		}
		k := costKey{Memory: p.Memory, Iterations: p.Iterations, Parallel: p.Parallel, KeyLen: p.KeyLen}
		g := groups[k]
		if g == nil {
			g = &costGroup{key: k}
			groups[k] = g
		}
		if kind == "mailbox" {
			g.mailbox = append(g.mailbox, name)
		} else {
			g.token = append(g.token, name)
		}
	}

	rows, err := db.Pool().Query(ctx, `SELECT name, password_hash FROM mailboxes ORDER BY name`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list mailboxes: %v\n", err)
		return EX_TEMPFAIL
	}
	for rows.Next() {
		var name, hash string
		if err := rows.Scan(&name, &hash); err != nil {
			rows.Close()
			fmt.Fprintf(os.Stderr, "scan mailbox: %v\n", err)
			return EX_TEMPFAIL
		}
		record("mailbox", name, hash)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		fmt.Fprintf(os.Stderr, "iterate mailboxes: %v\n", err)
		return EX_TEMPFAIL
	}
	rows.Close()

	rows, err = db.Pool().Query(ctx,
		`SELECT name, token_hash FROM api_tokens WHERE revoked_at IS NULL ORDER BY name`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list api tokens: %v\n", err)
		return EX_TEMPFAIL
	}
	for rows.Next() {
		var name, hash string
		if err := rows.Scan(&name, &hash); err != nil {
			rows.Close()
			fmt.Fprintf(os.Stderr, "scan token: %v\n", err)
			return EX_TEMPFAIL
		}
		record("api-token", name, hash)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		fmt.Fprintf(os.Stderr, "iterate api tokens: %v\n", err)
		return EX_TEMPFAIL
	}
	rows.Close()

	keys := make([]costKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Memory != keys[j].Memory {
			return keys[i].Memory < keys[j].Memory
		}
		if keys[i].Iterations != keys[j].Iterations {
			return keys[i].Iterations < keys[j].Iterations
		}
		return keys[i].Parallel < keys[j].Parallel
	})

	def := auth.DefaultParams()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MEMORY_KIB\tITERATIONS\tPARALLEL\tKEY_LEN\tMAILBOXES\tAPI_TOKENS\tNOTE")
	var aboveDefault int
	for _, k := range keys {
		g := groups[k]
		note := ""
		switch {
		case k.Memory > def.Memory || k.Iterations > def.Iterations:
			note = "ABOVE DEFAULT — logins for these are distinguishable by timing; re-hash them"
			aboveDefault += len(g.mailbox) + len(g.token)
		case k.Memory == def.Memory && k.Iterations == def.Iterations && k.Parallel == def.Parallel:
			note = "default"
		default:
			note = "below default"
		}
		fmt.Fprintf(tw, "%d\t%d\t%d\t%d\t%d\t%d\t%s\n",
			k.Memory, k.Iterations, k.Parallel, k.KeyLen, len(g.mailbox), len(g.token), note)
	}
	_ = tw.Flush()

	if *verbose {
		fmt.Println()
		for _, k := range keys {
			g := groups[k]
			fmt.Printf("m=%d,t=%d,p=%d:\n", k.Memory, k.Iterations, k.Parallel)
			for _, n := range g.mailbox {
				fmt.Printf("  mailbox   %s\n", n)
			}
			for _, n := range g.token {
				fmt.Printf("  api-token %s\n", n)
			}
		}
	}

	if len(invalid) > 0 {
		fmt.Println()
		fmt.Fprintf(os.Stderr, "%d credential(s) have an unparseable or out-of-range hash:\n", len(invalid))
		for _, m := range invalid {
			fmt.Fprintf(os.Stderr, "  %s\n", m)
		}
		fmt.Fprintln(os.Stderr, "These can never authenticate; reset them with mailbox-passwd / api-token-add.")
	}

	fmt.Println()
	fmt.Printf("default: m=%d,t=%d,p=%d (key %d bytes)\n",
		def.Memory, def.Iterations, def.Parallel, def.KeyLen)
	if aboveDefault > 0 {
		fmt.Printf("%d credential(s) cost MORE than the default. The IMAP login timing floor "+
			"cannot equalize them: re-hash with `admin mailbox-passwd` under the current argon2 config.\n",
			aboveDefault)
	} else {
		fmt.Println("no credential costs more than the default; the login timing floor covers them all")
	}
	if len(invalid) > 0 {
		return EX_DATAERR
	}
	return EX_OK
}
