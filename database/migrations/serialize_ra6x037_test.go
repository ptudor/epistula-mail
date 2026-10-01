package migrations_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/migrations"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestConcurrentApplyRunsEachMigrationOnce is the RA6X-037 regression.
//
// Apply computed its pending set BEFORE taking any lock, so two runners each
// read the same list and each executed the same SQL. Idempotent DDL let both
// succeed, but a data migration ran twice and migration 009's sequence
// initialization advanced twice. `ON CONFLICT DO NOTHING` on the tracking row
// hides that: it makes RECORDING the version idempotent, which is a different
// thing from making the migration BODY run once.
//
// The observable here is a sequence: `nextval` is not idempotent, so a
// migration body that calls it twice is detectable afterwards.
func TestConcurrentApplyRunsEachMigrationOnce(t *testing.T) {
	db, _ := pgtest.OpenBare(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const runners = 4
	var wg sync.WaitGroup
	results := make([][]migrations.Migration, runners)
	errs := make([]error, runners)
	start := make(chan struct{})

	for i := 0; i < runners; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = migrations.Apply(ctx, db.Pool())
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("runner %d: %v", i, err)
		}
	}

	// Exactly one runner may report having applied the set; the others must
	// find nothing pending.
	appliedTotal := 0
	for _, r := range results {
		appliedTotal += len(r)
	}
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if appliedTotal != len(all) {
		t.Fatalf("%d migrations applied across %d concurrent runners; want exactly %d — "+
			"a migration body ran more than once", appliedTotal, runners, len(all))
	}

	// The tracking table has one row per version, not more.
	var versions, distinct int
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*), count(DISTINCT version) FROM schema_versions`,
	).Scan(&versions, &distinct); err != nil {
		t.Fatalf("count schema_versions: %v", err)
	}
	if versions != distinct || versions != len(all) {
		t.Fatalf("schema_versions has %d rows (%d distinct); want %d", versions, distinct, len(all))
	}

	// Migration 009 seeds folder_uidvalidity_seq. Running it twice would have
	// advanced the sequence a second time, so its current value is a direct
	// witness that the body executed once.
	var seq int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT last_value FROM folder_uidvalidity_seq`).Scan(&seq); err != nil {
		t.Fatalf("read folder_uidvalidity_seq: %v", err)
	}
	if seq <= 0 {
		t.Fatalf("folder_uidvalidity_seq = %d; the seeding migration did not run", seq)
	}
}

// TestApplyIsStillIdempotentAfterLocking pins that serializing did not change
// the ordinary case: a second Apply against a migrated database is a no-op.
func TestApplyIsStillIdempotentAfterLocking(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	applied, err := migrations.Apply(ctx, db.Pool())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("a second Apply applied %d migrations; want 0", len(applied))
	}
}

// TestApplyReleasesItsLock pins that a finished run does not wedge the next
// one: a second Apply must not block.
func TestApplyReleasesItsLock(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := migrations.Apply(ctx, db.Pool())
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a second Apply blocked; the previous run did not release its lock")
	}
}
