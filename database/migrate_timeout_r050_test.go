package main

import (
	"context"
	"testing"
	"time"
)

// TestMigrateUpContext is the R-050 regression: `migrate up` no longer imposes
// a fixed 5-minute ceiling. A positive timeout bounds the run; a zero/negative
// timeout disables the bound entirely so a large CONCURRENT-style index build
// can run to completion instead of being rolled back at 5 minutes.
func TestMigrateUpContext(t *testing.T) {
	// Bounded: a deadline exists and is roughly the requested budget.
	ctx, cancel := migrateUpContext(context.Background(), 90*time.Minute)
	defer cancel()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("timeout>0 must set a deadline")
	}
	if until := time.Until(dl); until < 80*time.Minute || until > 91*time.Minute {
		t.Errorf("deadline is %v out, want ~90m", until)
	}

	// Unbounded: 0 disables the ceiling.
	ctx0, cancel0 := migrateUpContext(context.Background(), 0)
	defer cancel0()
	if _, ok := ctx0.Deadline(); ok {
		t.Error("timeout=0 must not set a deadline")
	}

	// Negative behaves like 0.
	ctxNeg, cancelNeg := migrateUpContext(context.Background(), -time.Second)
	defer cancelNeg()
	if _, ok := ctxNeg.Deadline(); ok {
		t.Error("negative timeout must not set a deadline")
	}
}
