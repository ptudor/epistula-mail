package imapsess

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestTokenBucketEnforcesRate(t *testing.T) {
	b := newTokenBucket(5)
	allowed := 0
	for i := 0; i < 50; i++ {
		if b.allow() {
			allowed++
		}
	}
	// The full burst (one second of tokens) plus at most a couple of
	// refill tokens for the loop's wall time.
	if allowed < 5 || allowed > 8 {
		t.Errorf("allowed = %d of 50 instant commands at 5/s, want ~5 (burst)", allowed)
	}

	// Refill: after 500ms at 5/s, roughly 2 more tokens are available.
	time.Sleep(500 * time.Millisecond)
	refilled := 0
	for i := 0; i < 10; i++ {
		if b.allow() {
			refilled++
		}
	}
	if refilled < 1 || refilled > 4 {
		t.Errorf("refilled = %d after 500ms at 5/s, want 1..4", refilled)
	}
}

func TestTokenBucketNilAllowsEverything(t *testing.T) {
	b := newTokenBucket(0) // disabled
	for i := 0; i < 1000; i++ {
		if !b.allow() {
			t.Fatal("disabled bucket refused a command")
		}
	}
}

func TestRequireAuthAppliesCommandRate(t *testing.T) {
	be := &Backend{CommandRatePerSec: 3, Logger: testLogger()}
	sess := be.NewSession()
	sess.mailboxID = 42 // authenticated

	var limited bool
	for i := 0; i < 20; i++ {
		if err := sess.requireAuth(); err != nil {
			var imapErr *imap.Error
			if !errors.As(err, &imapErr) || imapErr.Code != imap.ResponseCodeLimit {
				t.Fatalf("unexpected error shape: %v", err)
			}
			limited = true
			break
		}
	}
	if !limited {
		t.Error("20 instant commands at 3/s never hit the rate limit")
	}
}

func TestBackendIdlePoolFallsBackToMain(t *testing.T) {
	be := &Backend{}
	if be.idlePool() != nil {
		t.Error("idlePool with neither pool set should be nil (falls back to nil main pool)")
	}
	if got := be.heartbeat(); got != 29*time.Minute {
		t.Errorf("default heartbeat = %v, want 29m", got)
	}
	be.IdleHeartbeat = 5 * time.Minute
	if got := be.heartbeat(); got != 5*time.Minute {
		t.Errorf("configured heartbeat = %v, want 5m", got)
	}
}
