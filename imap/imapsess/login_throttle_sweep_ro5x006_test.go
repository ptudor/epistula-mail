package imapsess

import (
	"fmt"
	"testing"
	"time"
)

// stateLen reads the throttle's map size under the lock.
func stateLen(t *LoginThrottle) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.state)
}

// TestLoginThrottleSweepsAbandonedRecords is the RO5X-006 regression.
//
// An IP that fails fewer than `attempts` times and never returns used to keep
// its record forever: gcLocked only prunes the IP being recorded, and its
// guard requires an empty failure slice, which only happens on lockout. A
// spray from a large source pool grew the map until the process died.
func TestLoginThrottleSweepsAbandonedRecords(t *testing.T) {
	clock := time.Now()
	th := NewLoginThrottle(5, time.Minute, 2*time.Minute)
	th.now = func() time.Time { return clock }

	// 1,000 distinct IPs, one failure each — below the 5-attempt lockout,
	// so every record is the "abandoned" shape.
	for i := 0; i < 1000; i++ {
		th.RecordResult(fmt.Sprintf("198.51.100.%d", i), false)
	}
	if got := stateLen(th); got < 900 {
		t.Fatalf("state = %d after 1000 distinct failures, expected them retained pre-sweep", got)
	}

	// Age every record past max(window, cooldown), then drive enough
	// records to trip a sweep.
	clock = clock.Add(5 * time.Minute)
	for i := 0; i < throttleSweepInterval; i++ {
		th.RecordResult("203.0.113.1", false)
	}

	// Only the still-active sprayer should remain.
	if got := stateLen(th); got > 4 {
		t.Errorf("state = %d after the sweep, want it collapsed to a small constant", got)
	}
}

// TestLoginThrottleSweepPreservesLockout guards the load-bearing half of the
// fix: a currently-locked-out IP must survive the sweep, or an attacker could
// reset their own lockout by spraying from other addresses.
func TestLoginThrottleSweepPreservesLockout(t *testing.T) {
	clock := time.Now()
	th := NewLoginThrottle(5, time.Minute, 30*time.Minute)
	th.now = func() time.Time { return clock }

	const locked = "192.0.2.7"
	for i := 0; i < 5; i++ {
		th.RecordResult(locked, false)
	}
	if th.Allow(locked) {
		t.Fatal("IP should be locked out after 5 failures")
	}

	// Drive 300 failures from other IPs (>= one sweep interval), with the
	// clock advanced past the failure window but NOT past the 30m cooldown.
	clock = clock.Add(2 * time.Minute)
	for i := 0; i < 300; i++ {
		th.RecordResult(fmt.Sprintf("198.51.100.%d", i%250), false)
	}

	if th.Allow(locked) {
		t.Error("lockout was cleared by the sweep (RO5X-006 regression)")
	}
}

// TestLoginThrottleSweepKeepsRecentFailures ensures the sweep does not
// discard in-window failures, which would silently raise the effective
// attempt allowance.
func TestLoginThrottleSweepKeepsRecentFailures(t *testing.T) {
	clock := time.Now()
	th := NewLoginThrottle(5, time.Minute, time.Minute)
	th.now = func() time.Time { return clock }

	const victim = "192.0.2.9"
	// Four failures — one short of lockout.
	for i := 0; i < 4; i++ {
		th.RecordResult(victim, false)
	}

	// Trip a sweep without advancing the clock: the victim's failures are
	// still inside the window and must be retained.
	for i := 0; i < throttleSweepInterval; i++ {
		th.RecordResult(fmt.Sprintf("198.51.100.%d", i%200), false)
	}

	// The fifth failure must still trip the lockout.
	th.RecordResult(victim, false)
	if th.Allow(victim) {
		t.Error("in-window failures were dropped by the sweep; lockout did not trip")
	}
}

// TestLoginThrottleSuccessStillClears keeps the existing contract intact.
func TestLoginThrottleSuccessStillClears(t *testing.T) {
	th := NewLoginThrottle(5, time.Minute, time.Minute)
	const ip = "192.0.2.11"
	for i := 0; i < 3; i++ {
		th.RecordResult(ip, false)
	}
	th.RecordResult(ip, true)
	if got := stateLen(th); got != 0 {
		t.Errorf("state = %d after a successful login, want 0", got)
	}
	if !th.Allow(ip) {
		t.Error("IP should be allowed after a success cleared its record")
	}
}
