package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

// Annotation-model registry for api. message_annotations
// already keeps one annotation per (message_id, model), so a message carries
// every model's summary as an alternative; this registry ranks those models so
// epistula-api can serve a primary plus the alternatives. epistula-database owns the
// schema and this CRUD surface; epistula-api only reads the table.

func adminAnnotationModelSet(args []string) int {
	fs := flag.NewFlagSet("admin annotation-model-set", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	model := fs.String("model", "", `Model identifier, exactly as written to annotations (required), e.g. "lmstudio:qwen/qwen3.6-35b-a3b"`)
	priority := fs.Int("priority", 0, "Ranking priority; higher wins, ties broken by newest (required)")
	display := fs.String("display", "", "Optional human-friendly label for UIs")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	if strings.TrimSpace(*model) == "" {
		fmt.Fprintln(os.Stderr, "-model is required")
		return EX_USAGE
	}
	prioritySet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "priority" {
			prioritySet = true
		}
	})
	if !prioritySet {
		fmt.Fprintln(os.Stderr, "-priority is required")
		return EX_USAGE
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return EX_CONFIG
	}
	setupLogging(cfg)

	db, code := adminOpenDB(cfg)
	if code != EX_OK {
		return code
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Upsert. Setting a model (re)activates it: retired_at is cleared so an
	// operator re-ranking a retired model brings it back as a primary
	// candidate. (xmax = 0) distinguishes insert from update for the message.
	var inserted bool
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO annotation_models (model, priority, display_name)
		   VALUES ($1, $2, NULLIF($3, ''))
		 ON CONFLICT (model) DO UPDATE
		   SET priority     = EXCLUDED.priority,
		       display_name = COALESCE(NULLIF($3, ''), annotation_models.display_name),
		       retired_at   = NULL,
		       updated_at   = now()
		 RETURNING (xmax = 0)`,
		strings.TrimSpace(*model), *priority, strings.TrimSpace(*display),
	).Scan(&inserted); err != nil {
		fmt.Fprintf(os.Stderr, "upsert: %v\n", err)
		return EX_TEMPFAIL
	}

	verb := "updated"
	if inserted {
		verb = "registered"
	}
	fmt.Printf("%s annotation model %q (priority=%d, active)\n", verb, strings.TrimSpace(*model), *priority)
	if d := strings.TrimSpace(*display); d != "" {
		fmt.Printf("  display: %s\n", d)
	}
	return EX_OK
}

func adminAnnotationModelList(args []string) int {
	fs := flag.NewFlagSet("admin annotation-model-list", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	all := fs.Bool("all", false, "Include retired models")
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// annotations = how many messages this model has annotated, a quick sense
	// of coverage per model. The registry is tiny, so the per-row subquery is
	// cheap.
	query := `SELECT model, priority, COALESCE(display_name, ''), retired_at, updated_at,
	                 (SELECT count(*) FROM message_annotations a WHERE a.model = annotation_models.model)
	            FROM annotation_models`
	if !*all {
		query += ` WHERE retired_at IS NULL`
	}
	query += ` ORDER BY (retired_at IS NOT NULL), priority DESC, model`

	rows, err := db.Pool().Query(ctx, query)
	if err != nil {
		fmt.Fprintf(os.Stderr, "query: %v\n", err)
		return EX_TEMPFAIL
	}
	defer rows.Close()

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tPRIORITY\tDISPLAY\tANNOTATIONS\tSTATE\tUPDATED")
	count := 0
	for rows.Next() {
		var (
			model, display string
			priority       int
			annotations    int64
			retired        *time.Time
			updated        time.Time
		)
		if err := rows.Scan(&model, &priority, &display, &retired, &updated, &annotations); err != nil {
			fmt.Fprintf(os.Stderr, "scan: %v\n", err)
			return EX_TEMPFAIL
		}
		state := "active"
		if retired != nil {
			state = "retired " + retired.UTC().Format("2006-01-02")
		}
		if display == "" {
			display = "-"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%d\t%s\t%s\n",
			model, priority, display, annotations, state, updated.UTC().Format("2006-01-02"))
		count++
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "rows: %v\n", err)
		return EX_TEMPFAIL
	}
	if err := tw.Flush(); err != nil {
		return EX_IOERR
	}
	if count == 0 {
		fmt.Println("(no annotation models registered)")
	}
	return EX_OK
}

func adminAnnotationModelRetire(args []string) int {
	fs := flag.NewFlagSet("admin annotation-model-retire", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	model := fs.String("model", "", "Model identifier (required)")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}
	if strings.TrimSpace(*model) == "" {
		fmt.Fprintln(os.Stderr, "-model is required")
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tag, err := db.Pool().Exec(ctx,
		`UPDATE annotation_models SET retired_at = now(), updated_at = now()
		  WHERE model = $1 AND retired_at IS NULL`,
		strings.TrimSpace(*model),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: %v\n", err)
		return EX_TEMPFAIL
	}
	if tag.RowsAffected() == 0 {
		fmt.Fprintf(os.Stderr, "no active annotation model named %q (already retired or never registered)\n", strings.TrimSpace(*model))
		return EX_USAGE
	}
	fmt.Printf("retired annotation model %q (its annotations stay as alternatives; re-activate with annotation-model-set)\n", strings.TrimSpace(*model))
	return EX_OK
}
