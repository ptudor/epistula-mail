package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ptudor/epistula-mail/database/storage"
)

func adminLogPrune(args []string) int {
	fs := flag.NewFlagSet("admin log-prune", flag.ContinueOnError)
	config := fs.String("config", "", "Path to TOML config file")
	age := fs.Duration("older-than", 0, "Retention duration (e.g. 2160h for 90 days)")
	dry := fs.Bool("dry-run", false, "Count eligible rows without deleting")
	yes := fs.Bool("yes", false, "Delete eligible rows")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	if fs.NArg() != 0 || *age <= 0 || *dry == *yes {
		fmt.Fprintln(os.Stderr, "log-prune requires positive -older-than and exactly one of -dry-run or -yes")
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
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var cutoff time.Time
	if err := db.Pool().QueryRow(ctx, `SELECT now()-make_interval(secs=>$1)`, age.Seconds()).Scan(&cutoff); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return EX_TEMPFAIL
	}
	count, err := pruneDeliveryLog(ctx, db, cutoff, *dry)
	action := "deleted"
	if *dry {
		action = "would delete"
	}
	fmt.Printf("log-prune: %s %d rows before %s\n", action, count, cutoff.UTC().Format(time.RFC3339))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return EX_TEMPFAIL
	}
	return EX_OK
}

func pruneDeliveryLog(ctx context.Context, db *storage.DB, cutoff time.Time, dry bool) (int64, error) {
	var total int64
	if dry {
		err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM delivery_log WHERE received_at<$1`, cutoff).Scan(&total)
		return total, err
	}
	for {
		var deleted int64
		err := db.RunTx(ctx, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `DELETE FROM delivery_log WHERE id IN (
    SELECT id FROM delivery_log WHERE received_at<$1 ORDER BY received_at,id LIMIT 1000)`, cutoff)
			deleted = tag.RowsAffected()
			return err
		})
		if err != nil {
			return total, err
		}
		total += deleted
		if deleted < 1000 {
			return total, nil
		}
	}
}
