package main

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ptudor/epistula-mail/database/pgtest"
)

// probeModelPriority runs the exact startup probe SQL from runServe and
// returns what modelPriorityAvailable would be set to. The query must never
// error on a grant gap — has_table_privilege returns false (not an error)
// when the current role lacks SELECT, and to_regclass returns NULL when the
// table is absent.
func probeModelPriority(ctx context.Context, pool *pgxpool.Pool) (available bool, err error) {
	var selectable *bool
	err = pool.QueryRow(ctx, `SELECT CASE
	                WHEN to_regclass('annotation_models') IS NULL THEN NULL
	                ELSE has_table_privilege('annotation_models', 'SELECT')
	            END`).Scan(&selectable)
	if err != nil {
		return false, err
	}
	return selectable != nil && *selectable, nil
}

// TestAnnotationModelsProbeOwnerSelectable is the R-008 owner path: at
// migration 008 the DB owner has SELECT, so the probe reports ranked-available.
func TestAnnotationModelsProbeOwnerSelectable(t *testing.T) {
	db, _ := pgtest.Open(t) // applies all migrations incl. 008
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ok, err := probeModelPriority(ctx, db.Pool())
	if err != nil {
		t.Fatalf("probe as owner errored: %v", err)
	}
	if !ok {
		t.Fatal("owner probe: modelPriorityAvailable=false, want true (table present + SELECT)")
	}
}

// TestAnnotationModelsProbeAbsentDegrades confirms the probe degrades (returns
// false) without erroring when the annotation_models table does not exist —
// the pre-migration-008 deployment. This is the branch that used to key off
// to_regclass alone; the CASE guard keeps has_table_privilege from being
// called on a missing relation (which would raise "does not exist").
func TestAnnotationModelsProbeAbsentDegrades(t *testing.T) {
	db, _ := pgtest.OpenBare(t) // EMPTY db: no annotation_models table
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ok, err := probeModelPriority(ctx, db.Pool())
	if err != nil {
		t.Fatalf("probe against bare DB errored (must degrade, not error): %v", err)
	}
	if ok {
		t.Fatal("bare-DB probe: modelPriorityAvailable=true, want false (table absent)")
	}
}
