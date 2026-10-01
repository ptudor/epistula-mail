package imapsess

import (
	"log/slog"
	"sync"
	"time"

	"github.com/ptudor/epistula-mail/database/auth"
)

// AuthTimingFloor preserves the configured minimum LOGIN duration for lookup
// noise and early invalid/throttled requests. Cost classes slower than a floor
// require equal work, which authprofiles.go enforces independently. Calibration
// measures one default-cost verification; it is not a mixed-cost security proof.
// The overrun diagnostic remains available for explicit calibration tooling.
type AuthTimingFloor struct {
	floor time.Duration

	// overrunOnce keeps the "this account is slower than the floor" warning to
	// one line per account per process, so a slow account cannot flood the log
	// under a login flood.
	mu        sync.Mutex
	overrun   map[string]struct{}
	overruns  int64
	calibrate time.Duration
}

// DefaultAuthMinDuration is the configured floor when the operator sets none.
// It is a lower bound on the calibration, not a replacement for it: on a fast
// host the measured default-cost verification will exceed it and win.
const DefaultAuthMinDuration = 150 * time.Millisecond

// NewAuthTimingFloor calibrates against this host by timing one verification of
// the package's dummy hash. configured is the operator's minimum; a
// non-positive value selects DefaultAuthMinDuration.
func NewAuthTimingFloor(configured time.Duration) *AuthTimingFloor {
	if configured <= 0 {
		configured = DefaultAuthMinDuration
	}
	start := time.Now()
	_, _ = auth.VerifyPassword("calibration-not-a-password", dummyPasswordHash)
	measured := time.Since(start)

	floor := configured
	if measured > floor {
		floor = measured
	}
	return &AuthTimingFloor{
		floor:     floor,
		overrun:   make(map[string]struct{}),
		calibrate: measured,
	}
}

// Floor reports the equalized minimum LOGIN duration.
func (f *AuthTimingFloor) Floor() time.Duration {
	if f == nil {
		return 0
	}
	return f.floor
}

// Calibration reports the measured default-cost verification time this floor
// was derived from. Useful in check-config output and tests.
func (f *AuthTimingFloor) Calibration() time.Duration {
	if f == nil {
		return 0
	}
	return f.calibrate
}

// Wait pads the remaining time so a LOGIN that began at start takes at least
// the floor. A nil floor is a no-op, which is what an embedder that has not
// wired one gets.
//
// It sleeps rather than busy-waiting, and is interrupted by nothing: a client
// that disconnects mid-pad simply gets its response written to a closed socket.
// Cutting the pad short on disconnect would reintroduce the very signal being
// suppressed, because whether the attacker disconnects is under the attacker's
// control.
func (f *AuthTimingFloor) Wait(start time.Time) {
	if f == nil || f.floor <= 0 {
		return
	}
	if remaining := f.floor - time.Since(start); remaining > 0 {
		time.Sleep(remaining)
	}
}

// NoteOverrun records that verifying `account` took longer than the floor, so
// the floor cannot equalize it. Logged once per account per process.
func (f *AuthTimingFloor) NoteOverrun(logger *slog.Logger, account string, took time.Duration) {
	if f == nil || f.floor <= 0 || took <= f.floor {
		return
	}
	f.mu.Lock()
	_, seen := f.overrun[account]
	if !seen {
		f.overrun[account] = struct{}{}
	}
	f.overruns++
	f.mu.Unlock()
	if seen || logger == nil {
		return
	}
	logger.Warn("account password hash is more expensive than the authentication timing floor; "+
		"its logins are distinguishable by timing until it is re-hashed at the configured cost "+
		"(see `epistula-database admin auth-cost-audit`)",
		"mailbox", account, "verify", took, "floor", f.floor)
}

// Overruns reports how many verifications have exceeded the floor.
func (f *AuthTimingFloor) Overruns() int64 {
	if f == nil {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.overruns
}
