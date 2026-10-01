package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"testing"
	"time"
)

type watchdogLogHandler struct{ handle func() }

func (h watchdogLogHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (h watchdogLogHandler) Handle(context.Context, slog.Record) error { h.handle(); return nil }
func (h watchdogLogHandler) WithAttrs([]slog.Attr) slog.Handler        { return h }
func (h watchdogLogHandler) WithGroup(string) slog.Handler             { return h }

// This is the exact previous failure, deterministically: the callback begins
// uncommitted, and a commit is recorded while its diagnostic is being written.
// Test in a child so the actual os.Exit decision is exercised.
func TestRemainingWatchdogProcess(t *testing.T) {
	if mode := os.Getenv("MAIL_TEST_WATCHDOG_MODE"); mode != "" {
		a := &deliveryAcceptance{}
		switch mode {
		case "during-log":
			slog.SetDefault(slog.New(watchdogLogHandler{handle: a.markCommitted}))
		case "before":
			a.markCommitted()
		}
		deliveryDeadlineExceeded(a, time.Second, "fixture@example.invalid")
		os.Exit(99)
	}
	for _, mode := range []string{"before", "during-log", "uncommitted"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestRemainingWatchdogProcess$")
			cmd.Env = append(os.Environ(), "MAIL_TEST_WATCHDOG_MODE="+mode)
			err := cmd.Run()
			want := EX_OK
			if mode == "uncommitted" {
				want = EX_TEMPFAIL
			}
			got := cmd.ProcessState.ExitCode()
			if got != want {
				t.Fatalf("exit=%d want=%d err=%v", got, want, err)
			}
		})
	}
}

func TestRemainingWatchdogDoesNotWaitForStuckLogging(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	logger := slog.Default()
	defer slog.SetDefault(logger)
	slog.SetDefault(slog.New(watchdogLogHandler{handle: func() { <-release }}))
	old := exitProcess
	defer func() { exitProcess = old }()
	exitProcess = func(code int) {
		if code != EX_TEMPFAIL {
			t.Errorf("exit=%d", code)
		}
	}
	start := time.Now()
	deliveryDeadlineExceeded(&deliveryAcceptance{}, time.Second, "fixture@example.invalid")
	if time.Since(start) > time.Second {
		t.Fatal("watchdog waited for blocked logging")
	}
}

func TestRemainingCommitDisarmsPrimaryDeadline(t *testing.T) {
	old := exitProcess
	defer func() { exitProcess = old }()
	fired := make(chan int, 1)
	exitProcess = func(code int) { fired <- code }
	a := &deliveryAcceptance{}
	stop := armDeliveryWatchdogs(a, 10*time.Millisecond, 50*time.Millisecond, "fixture@example.invalid")
	defer stop()
	a.markCommitted()
	a.markCommitted() // commit recording and cleanup must be idempotent
	select {
	case code := <-fired:
		if code != EX_OK {
			t.Fatalf("committed deadline exited %d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("post-commit work has no finite deadline")
	}
}
