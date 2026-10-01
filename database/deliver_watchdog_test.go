package main

import (
	"testing"
	"time"
)

// TestDeliveryWatchdogFires verifies the wall-clock watchdog actually exits
// the process with EX_TEMPFAIL (via the exitProcess indirection) when the
// delivery overruns, and that a finished delivery's Stop prevents it.
func TestDeliveryWatchdogFires(t *testing.T) {
	fired := make(chan int, 1)
	orig := exitProcess
	exitProcess = func(code int) { fired <- code }
	defer func() { exitProcess = orig }()

	timeout := 20 * time.Millisecond
	// The real callback, not a copy of it, so this cannot drift from what
	// runDeliver actually arms (RA6X-059). No commit has been recorded, so the
	// retry code is the correct outcome.
	watchdog := time.AfterFunc(timeout, func() {
		deliveryDeadlineExceeded(&deliveryAcceptance{}, timeout, "rcpt@example.invalid")
	})
	defer watchdog.Stop()

	select {
	case code := <-fired:
		if code != EX_TEMPFAIL {
			t.Fatalf("watchdog exited with %d, want EX_TEMPFAIL (%d)", code, EX_TEMPFAIL)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not fire")
	}
}

func TestDeliveryWatchdogStops(t *testing.T) {
	fired := make(chan int, 1)
	orig := exitProcess
	exitProcess = func(code int) { fired <- code }
	defer func() { exitProcess = orig }()

	watchdog := time.AfterFunc(20*time.Millisecond, func() { exitProcess(EX_TEMPFAIL) })
	watchdog.Stop() // delivery finished in time

	select {
	case <-fired:
		t.Fatal("stopped watchdog still fired")
	case <-time.After(100 * time.Millisecond):
	}
}
