package imapsess

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
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

// TestTokenBucketReserveQueuesExcess pins the pacing arithmetic with explicit
// times: a full bucket serves its burst at once, each command past it is due
// one token interval after the one before, and the deficit is repaid by the
// refill before anything is served immediately again.
func TestTokenBucketReserveQueuesExcess(t *testing.T) {
	b := newTokenBucket(10) // 10/s: one token per 100ms, burst 10
	t0 := b.last
	for i := 0; i < 10; i++ {
		if d := b.reserve(t0); d != 0 {
			t.Fatalf("command %d within the burst waited %v", i+1, d)
		}
	}
	for i, want := range []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond} {
		if d := b.reserve(t0); d != want {
			t.Errorf("command %d past the burst: wait %v, want %v", i+1, d, want)
		}
	}
	// Three tokens are owed. After 250ms only 2.5 have accrued: still owed.
	if d := b.reserve(t0.Add(250 * time.Millisecond)); d != 150*time.Millisecond {
		t.Errorf("wait while repaying = %v, want 150ms", d)
	}
	// Two seconds later the bucket is full again and serves at once.
	if d := b.reserve(t0.Add(2 * time.Second)); d != 0 {
		t.Errorf("wait after a full refill = %v, want 0", d)
	}
}

// TestTokenBucketWaitPacesWithoutRefusing is the folder-sweep case in real
// time: every command of a burst twice the bucket's size is served, and the
// part past the burst takes about as long as the rate says it must.
func TestTokenBucketWaitPacesWithoutRefusing(t *testing.T) {
	b := newTokenBucket(50) // burst 50, then one per 20ms
	start := time.Now()
	var paced int
	for i := 0; i < 100; i++ {
		waited, err := b.wait(context.Background())
		if err != nil {
			t.Fatalf("command %d: %v", i+1, err)
		}
		if waited > 0 {
			paced++
		}
	}
	elapsed := time.Since(start)
	if elapsed < 900*time.Millisecond || elapsed > 3*time.Second {
		t.Errorf("100 commands at 50/s took %v, want about 1s (50 burst + 50 paced)", elapsed)
	}
	if paced < 45 || paced > 50 {
		t.Errorf("%d commands waited, want about 50", paced)
	}
}

// TestTokenBucketWaitStopsWhenContextEnds: a command waiting for its token
// gives up as soon as its session ends, rather than sleeping out the wait.
func TestTokenBucketWaitStopsWhenContextEnds(t *testing.T) {
	b := newTokenBucket(1) // the second command would wait a full second
	if _, err := b.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	start := time.Now()
	if _, err := b.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("wait returned after %v, want promptly after cancel", elapsed)
	}
}

// TestRequireAuthPacesInsteadOfRefusing is the regression for the
// thousand-folder mailbox: a burst past the command rate used to fail with
// NO [LIMIT] and log a WARN per command. Now every command succeeds, the
// burst is slowed to the rate, and the log gets one line for the burst, not
// at the first command the bucket can serve at once but once it is over.
func TestRequireAuthPacesInsteadOfRefusing(t *testing.T) {
	var logs bytes.Buffer
	be := &Backend{CommandRatePerSec: 20, Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	sess := be.NewSession()
	defer sess.Close()
	sess.mailboxID = 42 // authenticated
	sess.mailboxName = "jdoe"

	start := time.Now()
	for i := 0; i < 30; i++ {
		if err := sess.requireAuth(); err != nil {
			t.Fatalf("command %d refused: %v", i+1, err)
		}
	}
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond {
		t.Errorf("30 commands at 20/s took %v; the 10 past the burst should take about 500ms", elapsed)
	}
	if logs.Len() != 0 {
		t.Errorf("logged during the burst: %q", logs.String())
	}

	// A command the refilled bucket serves at once does not end the burst:
	// in a real sweep that happens every few dozen commands.
	time.Sleep(200 * time.Millisecond)
	if err := sess.requireAuth(); err != nil {
		t.Fatal(err)
	}
	if logs.Len() != 0 {
		t.Errorf("logged within the quiet gap: %q", logs.String())
	}
	// Closing the session reports what it had not reported yet.
	sess.Close()
	out := logs.String()
	if n := strings.Count(out, "\n"); n != 1 {
		t.Fatalf("log has %d lines, want 1 summary: %q", n, out)
	}
	for _, want := range []string{"level=INFO", `msg="command burst paced"`, "mailbox=jdoe", "commands=10", "span="} {
		if !strings.Contains(out, want) {
			t.Errorf("summary %q lacks %q", out, want)
		}
	}
}

// TestRequireAuthStopsWaitingWhenSessionCloses: closing the session releases
// a command that is waiting for its token.
func TestRequireAuthStopsWaitingWhenSessionCloses(t *testing.T) {
	be := &Backend{CommandRatePerSec: 1, Logger: testLogger()}
	sess := be.NewSession()
	sess.mailboxID = 42
	if err := sess.requireAuth(); err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(20*time.Millisecond, func() { sess.sessCancel() })
	start := time.Now()
	err := sess.requireAuth()
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) || imapErr.Type != imap.StatusResponseTypeNo {
		t.Fatalf("err = %v, want a NO once the session is closing", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("returned after %v, want promptly after the session closed", elapsed)
	}
	sess.Close()
}

// TestCommandPacingReportsSustainedPacingEachMinute pins the log cadence: a
// client that is paced continuously is reported once a minute with the
// totals since the last report, which is how a looping client shows up.
func TestCommandPacingReportsSustainedPacingEachMinute(t *testing.T) {
	var p commandPacing
	t0 := time.Date(2026, 10, 13, 5, 0, 0, 0, time.UTC)
	if p.pending() {
		t.Fatal("new pacing record is pending")
	}
	if p.note(t0, 10*time.Millisecond) || p.note(t0.Add(30*time.Second), 10*time.Millisecond) {
		t.Fatal("reported before a minute of pacing")
	}
	if !p.note(t0.Add(61*time.Second), 10*time.Millisecond) {
		t.Fatal("a minute of pacing was not reported")
	}
	commands, waited, span := p.take()
	if commands != 3 || waited != 30*time.Millisecond || span != 61*time.Second {
		t.Errorf("take = %d commands, %v waited over %v; want 3, 30ms over 61s", commands, waited, span)
	}
	if p.pending() {
		t.Error("still pending after take")
	}
	// The next period starts from its own first command.
	if p.note(t0.Add(90*time.Second), time.Millisecond) {
		t.Error("new period reported immediately")
	}
}

// TestCommandPacingCoalescesASweep: within one sweep, paced commands
// alternate with ones the refilled bucket serves at once. The burst is over
// only after pacingQuietGap without a paced command, so a sweep is one line.
func TestCommandPacingCoalescesASweep(t *testing.T) {
	var p commandPacing
	t0 := time.Date(2026, 10, 13, 6, 24, 48, 0, time.UTC)
	if p.ended(t0) {
		t.Fatal("nothing pending, yet ended")
	}
	p.note(t0, 10*time.Millisecond)
	for _, gap := range []time.Duration{50 * time.Millisecond, 2 * time.Second} {
		if p.ended(t0.Add(gap)) {
			t.Fatalf("burst ended %v after a paced command, inside the quiet gap", gap)
		}
	}
	p.note(t0.Add(3*time.Second), 20*time.Millisecond)
	if p.ended(t0.Add(3*time.Second + pacingQuietGap - time.Millisecond)) {
		t.Fatal("burst ended just before the quiet gap elapsed")
	}
	if !p.ended(t0.Add(3*time.Second + pacingQuietGap)) {
		t.Fatal("burst not over after a full quiet gap")
	}
	commands, waited, span := p.take()
	if commands != 2 || waited != 30*time.Millisecond || span != 3*time.Second {
		t.Errorf("take = %d commands, %v waited over %v; want 2, 30ms over 3s", commands, waited, span)
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
