package imapsess

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"

	"golang.org/x/sync/semaphore"

	"github.com/ptudor/epistula-mail/database/auth"
)

// errAuthBudgetExhausted reports that no verification budget was available
// before the caller's context expired. It is deliberately not surfaced to the
// client as a distinct condition — Login answers every failure the same way.
var errAuthBudgetExhausted = errors.New("imapsess: authentication verification budget exhausted")

// AuthBudget bounds the total Argon2id scratch memory in flight across the
// whole daemon (RA6X-023).
//
// The per-IP connection and login-failure limiters do not bound this. Every
// unknown username deliberately runs a full 64 MiB verification against the
// dummy hash so account existence cannot be probed by timing, and that work
// happens before the per-mailbox connection limit is consulted — an attacker
// possessing no credentials at all can therefore commission it. Spread the
// attempts across enough source addresses (trivial with an IPv6 allocation)
// and every per-IP limiter stays under its threshold while the host runs out
// of memory. The pre-auth timer closes sockets but cannot interrupt an
// argon2.IDKey call already running.
//
// So verification is rationed by an explicit memory budget rather than a
// request count: reservations are weighted by the KiB each hash declares it
// will allocate, which is the resource actually being contended. epistula-api
// already rations its own verifications this way (its argonSem); this brings
// the IMAP front door under the same discipline.
//
// A caller blocks until budget is free or its context is done, so a client
// that disconnects — or a daemon that is shutting down — releases its place in
// the queue instead of holding a slot for work nobody is waiting for.
type AuthBudget struct {
	sem      *semaphore.Weighted
	limitKiB int64

	// waiting counts requests queued for budget. Queued waiters are cheap
	// (a goroutine, no Argon2 memory yet), but they are not free, and an
	// unbounded queue converts a memory flood into a goroutine flood. The cap
	// keeps the queue proportional to the budget: past it, a login is refused
	// immediately rather than joining a line it will not reach in time.
	waiting    atomic.Int64
	maxWaiters int64
}

// DefaultAuthBudgetKiB is the budget used when none is configured: 4 GiB of
// concurrent Argon2 scratch memory, or 256 simultaneous verifications at the
// 64 MiB default. That is high enough that ordinary IMAP login traffic
// never queues and low enough to leave the host usable under a flood.
const DefaultAuthBudgetKiB int64 = 4 * 1024 * 1024

// NewAuthBudget returns a budget of limitKiB KiB of concurrent Argon2 scratch
// memory. A non-positive limit selects DefaultAuthBudgetKiB.
//
// The operator's number is honoured as written. A hash costing more than the
// entire budget is refused before hashing. Audit stored costs before lowering
// the budget; raise the budget or reset an outlier password to restore access.
func NewAuthBudget(limitKiB int64) *AuthBudget {
	if limitKiB <= 0 {
		limitKiB = DefaultAuthBudgetKiB
	}
	// Room for sixteen times as many queued logins as can run concurrently at
	// the default cost, and never fewer than 64. A mail client opening
	// several connections at once, or a whole office arriving at 09:00, must
	// queue rather than see failures; the cap exists only to stop an
	// unbounded backlog, and a waiter costs a blocked goroutine, not 64 MiB.
	maxWaiters := 16 * (limitKiB / int64(auth.DefaultParams().Memory))
	if maxWaiters < 64 {
		maxWaiters = 64
	}
	return &AuthBudget{
		sem:        semaphore.NewWeighted(limitKiB),
		limitKiB:   limitKiB,
		maxWaiters: maxWaiters,
	}
}

// NewAuthBudgetMiB is NewAuthBudget in the unit operators configure. A
// non-positive value selects a default scaled to this host's GOMAXPROCS.
func NewAuthBudgetMiB(limitMiB int) *AuthBudget {
	if limitMiB <= 0 {
		return NewAuthBudget(defaultAuthBudgetKiBForHost())
	}
	return NewAuthBudget(int64(limitMiB) * 1024)
}

// LimitKiB reports the configured budget.
func (b *AuthBudget) LimitKiB() int64 {
	if b == nil {
		return 0
	}
	return b.limitKiB
}

// verify runs one password verification inside the budget.
//
// cost is the memory the hash declares; it is reserved for the duration of the
// hash and released afterwards. A nil budget verifies without rationing, which
// is what unit tests and any embedder that has not wired a budget get.
//
// Returns errAuthBudgetExhausted if ctx is done before budget is available.
// The verification itself is not interruptible — argon2.IDKey has no context —
// so the cancellation window is the queue, which is where an attacker's
// requests pile up.
func (b *AuthBudget) verify(ctx context.Context, password, encoded string, cost int64) (bool, error) {
	return b.verifyFunc(ctx, cost, func() (bool, error) {
		return auth.VerifyPassword(password, encoded)
	})
}

// verifyFunc is verify with the hashing itself injectable, so the budget's
// behaviour can be tested at its real ceiling without allocating gigabytes of
// Argon2 scratch memory on the test host.
func (b *AuthBudget) verifyFunc(ctx context.Context, cost int64, run func() (bool, error)) (bool, error) {
	if b == nil {
		return run()
	}
	if cost < 1 {
		cost = 1
	}
	if cost > b.limitKiB {
		return false, errAuthBudgetExhausted
	}
	if b.waiting.Add(1) > b.maxWaiters {
		b.waiting.Add(-1)
		return false, errAuthBudgetExhausted
	}
	err := b.sem.Acquire(ctx, cost)
	b.waiting.Add(-1)
	if err != nil {
		return false, errAuthBudgetExhausted
	}
	defer b.sem.Release(cost)
	return run()
}

// verifyCost reports the budget weight for an encoded hash: the scratch memory
// it declares, or 0 with an error when the hash is malformed or asks for more
// work than the verifier accepts. Rejecting here means a corrupted PHC row
// never occupies a rationed slot.
func verifyCost(encoded string) (int64, error) {
	p, err := auth.ParseEncodedParams(encoded)
	if err != nil {
		return 0, err
	}
	// Argon2 rounds low requested memory up to eight blocks per lane.
	cost := max(int64(p.Memory), 8*int64(p.Parallel))
	if cost <= 0 {
		return 0, fmt.Errorf("invalid Argon2 memory cost")
	}
	return cost, nil
}

// defaultAuthBudgetKiBForHost scales the default budget to the machine when
// the operator has not chosen one, so a small VM does not adopt a limit its
// RAM cannot honour. It never exceeds DefaultAuthBudgetKiB.
func defaultAuthBudgetKiBForHost() int64 {
	// One verification per CPU at the default cost is the floor; the cap is
	// the documented default. GOMAXPROCS rather than NumCPU so a container
	// with a CPU quota is respected.
	perCPU := int64(runtime.GOMAXPROCS(0)) * int64(auth.DefaultParams().Memory)
	if perCPU < int64(auth.DefaultParams().Memory) {
		perCPU = int64(auth.DefaultParams().Memory)
	}
	if perCPU > DefaultAuthBudgetKiB {
		return DefaultAuthBudgetKiB
	}
	return perCPU
}
