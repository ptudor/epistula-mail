package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestWatchdogExitsOKAfterACommit is the RA6X-059 regression at the boundary
// itself. The delivery timer stays armed while the post-delivery hook runs, so a
// transaction that commits near the deadline can be followed by EX_TEMPFAIL
// during a hook that is itself correctly bounded. Postfix then redelivers mail
// that is already durably stored — and the folder-scoped dedup that would
// absorb the retry does not protect a message the user has since moved or
// expunged.
func TestWatchdogExitsOKAfterACommit(t *testing.T) {
	cases := []struct {
		name      string
		committed bool
		want      int
	}{
		{"timeout before the commit requeues", false, EX_TEMPFAIL},
		{"timeout after the commit does not", true, EX_OK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fired := make(chan int, 1)
			orig := exitProcess
			exitProcess = func(code int) { fired <- code }
			defer func() { exitProcess = orig }()

			accepted := &deliveryAcceptance{}
			if tc.committed {
				accepted.markCommitted()
			}

			timeout := 20 * time.Millisecond
			watchdog := time.AfterFunc(timeout, func() {
				deliveryDeadlineExceeded(accepted, timeout, "rcpt@example.invalid")
			})
			defer watchdog.Stop()

			select {
			case code := <-fired:
				if code != tc.want {
					t.Fatalf("watchdog exited %d (%s), want %d (%s)",
						code, ExitCodeName(code), tc.want, ExitCodeName(tc.want))
				}
			case <-time.After(5 * time.Second):
				t.Fatal("watchdog did not fire")
			}
		})
	}
}

// TestAcceptanceRacesStopWithoutRequeueing covers the callback-versus-Stop
// race: Stop returning false does not wait for a callback that has already
// begun. Whichever order the runtime picks, a committed message must
// never be given a retry exit code.
func TestAcceptanceRacesStopWithoutRequeueing(t *testing.T) {
	for i := 0; i < 200; i++ {
		accepted := &deliveryAcceptance{}
		orig := exitProcess
		exitProcess = func(code int) {
			if accepted.isCommitted() && code != EX_OK {
				t.Errorf("recorded commit exited %d", code)
			}
		}
		watchdog := newDeliveryTimer(time.Nanosecond, func() {
			deliveryDeadlineExceeded(accepted, time.Millisecond, "rcpt@example.invalid")
		})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); accepted.markCommitted(); watchdog.stop() }()
		wg.Wait()
		watchdog.stop() // joins the callback before restoring the process exit hook
		exitProcess = orig
	}
}

// TestDeliverMarksAcceptanceOnCommit pins the wiring: a real delivery through
// the real code path must set the boundary, and must not set it when the
// delivery fails.
func TestDeliverMarksAcceptanceOnCommit(t *testing.T) {
	cfg, _, teardown := deliverFixture(t)
	defer teardown()

	accepted := &deliveryAcceptance{}
	code := deliverBytesWithAcceptance(t, cfg, accepted,
		"ra6x008@ra6x008.invalid", "sender@ra6x008.invalid",
		"From: sender@ra6x008.invalid\r\nSubject: durable\r\n\r\nbody\r\n")
	if code != EX_OK {
		t.Fatalf("delivery exit=%d (%s), want EX_OK", code, ExitCodeName(code))
	}
	if !accepted.isCommitted() {
		t.Fatal("a committed delivery did not record durable acceptance")
	}

	// A rejected recipient never commits, so the boundary stays closed and a
	// late timeout would still requeue.
	rejected := &deliveryAcceptance{}
	code = deliverBytesWithAcceptance(t, cfg, rejected,
		"nobody@not-a-domain-here.invalid", "sender@ra6x008.invalid",
		"From: sender@ra6x008.invalid\r\nSubject: nope\r\n\r\nbody\r\n")
	if code == EX_OK {
		t.Fatalf("unknown domain returned EX_OK")
	}
	if rejected.isCommitted() {
		t.Fatal("a failed delivery recorded durable acceptance")
	}
}

// TestHookFailureAfterCommitDoesNotRequeue pins the RA6X-059 scenario end to
// end: a delivery that commits and then runs a hook which outlives the
// delivery deadline must still exit EX_OK.
func TestHookFailureAfterCommitDoesNotRequeue(t *testing.T) {
	cfg, _, teardown := deliverFixture(t)
	defer teardown()

	// Wait for the hook's marker before firing the watchdog. This proves the
	// delivery has committed even when database setup is slow under -race.
	markerDir := t.TempDir()
	entered := filepath.Join(markerDir, "entered")
	release := filepath.Join(markerDir, "release")
	cfg.Delivery.PostHookTimeout = "15s"
	cfg.Delivery.PostHookCommand = []string{"/bin/sh", "-c", `touch "$1"; while [ ! -f "$2" ]; do sleep 0.01; done; exit 3`, "hook", entered, release}

	fired := make(chan int, 1)
	orig := exitProcess
	exitProcess = func(code int) { fired <- code }
	defer func() { exitProcess = orig }()
	accepted := &deliveryAcceptance{}
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(entered); err == nil {
				deliveryDeadlineExceeded(accepted, cfg.DeliveryTimeoutDuration(), "ra6x008@ra6x008.invalid")
				_ = os.WriteFile(release, nil, 0600)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	code := deliverBytesWithAcceptanceCtx(t, ctx, cfg, accepted,
		"ra6x008@ra6x008.invalid", "sender@ra6x008.invalid",
		"From: sender@ra6x008.invalid\r\nSubject: hooked\r\n\r\nbody\r\n")
	<-watcherDone
	if code != EX_OK {
		t.Fatalf("delivery exit=%d (%s), want EX_OK — the hook's failure must not requeue committed mail",
			code, ExitCodeName(code))
	}

	select {
	case fired := <-fired:
		if fired != EX_OK {
			t.Fatalf("the watchdog fired with %d (%s) after a successful commit; want EX_OK",
				fired, ExitCodeName(fired))
		}
	default:
		t.Fatal("post-commit hook marker was never observed; the boundary was not exercised")
	}
}

func deliverBytesWithAcceptance(t *testing.T, cfg *Config, accepted *deliveryAcceptance, envTo, envFrom, raw string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return deliverBytesWithAcceptanceCtx(t, ctx, cfg, accepted, envTo, envFrom, raw)
}

func deliverBytesWithAcceptanceCtx(t *testing.T, ctx context.Context, cfg *Config, accepted *deliveryAcceptance, envTo, envFrom, raw string) int {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "msg")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	if _, err := f.WriteString(raw); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatalf("seek: %v", err)
	}
	saved := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = saved; _ = f.Close() }()

	return deliver(ctx, cfg, accepted, envTo, envFrom)
}
