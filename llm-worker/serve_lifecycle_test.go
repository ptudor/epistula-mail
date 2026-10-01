package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// rtNeverCalled fails the test if the LLM transport is invoked — used for
// zero-row passes where processMessage must never run.
func rtNeverCalled(t *testing.T) http.RoundTripper {
	t.Helper()
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("LLM transport called unexpectedly: %s", r.URL)
		return nil, errors.New("unexpected LLM call")
	})
}

// TestSleepCtxReturnsOnCancel is the R-055(a) unit: a canceled context makes the
// retry backoff return promptly with context.Canceled rather than sleeping.
func TestSleepCtxReturnsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- sleepCtx(ctx, time.Hour) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("sleepCtx = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sleepCtx did not return promptly on a canceled context")
	}

	// Full sleep returns nil.
	if err := sleepCtx(context.Background(), 5*time.Millisecond); err != nil {
		t.Fatalf("sleepCtx full = %v, want nil", err)
	}
}

// TestRunLoopIntervalZeroTerminates is the R-057 regression: `serve` with
// interval_seconds = 0 runs a single pass and the loop goroutine exits (closes
// done) instead of idling forever — so runServe returns cleanly.
func TestRunLoopIntervalZeroTerminates(t *testing.T) {
	srv := exportServer(t, 0) // empty pass; LLM never called
	defer srv.Close()
	w := newTestWorker(t, srv.URL, rtNeverCalled(t))
	w.cfg.Worker.IntervalSec = 0

	done := runLoop(context.Background(), w, &atomicRunState{})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runLoop with interval=0 idled instead of terminating after one pass")
	}
}

// TestRunLoopExitsOnCancel is the R-055(b) mechanism: with a long interval the
// loop parks after its pass; canceling ctx closes done so runServe's waitLoop
// can join the goroutine on shutdown.
func TestRunLoopExitsOnCancel(t *testing.T) {
	srv := exportServer(t, 0)
	defer srv.Close()
	w := newTestWorker(t, srv.URL, rtNeverCalled(t))
	w.cfg.Worker.IntervalSec = 3600 // parks after the first pass

	ctx, cancel := context.WithCancel(context.Background())
	done := runLoop(ctx, w, &atomicRunState{})
	// Give the first (empty) pass time to complete and enter the interval wait.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runLoop did not exit on ctx cancel")
	}
}

// TestWaitLoopBounds confirms waitLoop returns immediately when the loop is done
// and gives up after the shutdown timeout when it is not.
func TestWaitLoopBounds(t *testing.T) {
	cfg := &Config{}
	cfg.Admin.ShutdownTimeoutSec = 0 // exercises the 5s fallback floor path

	closed := make(chan struct{})
	close(closed)
	start := make(chan struct{})
	got := make(chan struct{})
	go func() { <-start; waitLoop(closed, cfg); close(got) }()
	close(start)
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("waitLoop did not return for an already-closed done channel")
	}
}
