package main

import (
	"context"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestAnnotationModelsRegistry locks the migration-008 contract that the
// annotation-model admin CLI and epistula-api both rely on: one row per model
// (PK), `set` upserts priority/display and (re)activates by clearing
// retired_at, `set` preserves an existing display when -display is omitted,
// and `retire` soft-retires without deleting the row.
func TestAnnotationModelsRegistry(t *testing.T) {
	db, _ := pgtest.Open(t)
	pool := db.Pool()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The exact upsert `annotation-model-set` issues.
	const set = `INSERT INTO annotation_models (model, priority, display_name)
	               VALUES ($1, $2, NULLIF($3, ''))
	             ON CONFLICT (model) DO UPDATE
	               SET priority     = EXCLUDED.priority,
	                   display_name = COALESCE(NULLIF($3, ''), annotation_models.display_name),
	                   retired_at   = NULL,
	                   updated_at   = now()
	             RETURNING (xmax = 0)`

	var inserted bool
	if err := pool.QueryRow(ctx, set, "lmstudio:qwen", 100, "Qwen3.6").Scan(&inserted); err != nil {
		t.Fatalf("first set: %v", err)
	}
	if !inserted {
		t.Error("first set should report inserted=true")
	}

	// Retire it (the exact UPDATE `annotation-model-retire` issues).
	tag, err := pool.Exec(ctx,
		`UPDATE annotation_models SET retired_at = now(), updated_at = now()
		  WHERE model = $1 AND retired_at IS NULL`, "lmstudio:qwen")
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("retire affected %d rows, want 1", tag.RowsAffected())
	}

	// Re-set reactivates (clears retired_at), updates priority, preserves the
	// display when omitted, and reports inserted=false (update path).
	if err := pool.QueryRow(ctx, set, "lmstudio:qwen", 5, "").Scan(&inserted); err != nil {
		t.Fatalf("re-set: %v", err)
	}
	if inserted {
		t.Error("re-set should report inserted=false (update path)")
	}

	var (
		priority  int
		display   string
		retiredAt *time.Time
		count     int
	)
	if err := pool.QueryRow(ctx,
		`SELECT priority, COALESCE(display_name, ''), retired_at,
		        (SELECT count(*) FROM annotation_models WHERE model = $1)
		   FROM annotation_models WHERE model = $1`, "lmstudio:qwen",
	).Scan(&priority, &display, &retiredAt, &count); err != nil {
		t.Fatalf("read after re-set: %v", err)
	}
	if count != 1 {
		t.Errorf("rows for model = %d, want 1 (one row per model)", count)
	}
	if retiredAt != nil {
		t.Error("re-set must clear retired_at (reactivate)")
	}
	if priority != 5 {
		t.Errorf("priority = %d, want 5 (updated)", priority)
	}
	if display != "Qwen3.6" {
		t.Errorf("display = %q, want preserved 'Qwen3.6' when -display omitted", display)
	}

	// Retire is idempotent against an already-retired/absent model: zero rows.
	if _, err := pool.Exec(ctx,
		`UPDATE annotation_models SET retired_at = now() WHERE model = $1 AND retired_at IS NULL`,
		"lmstudio:qwen"); err != nil {
		t.Fatalf("retire active: %v", err)
	}
	tag, err = pool.Exec(ctx,
		`UPDATE annotation_models SET retired_at = now() WHERE model = $1 AND retired_at IS NULL`,
		"lmstudio:qwen")
	if err != nil {
		t.Fatalf("retire already-retired: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Errorf("retiring an already-retired model affected %d rows, want 0", tag.RowsAffected())
	}
}
