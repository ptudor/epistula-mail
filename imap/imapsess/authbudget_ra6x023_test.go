package imapsess

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/auth"
)

// TestAuthBudgetCapsConcurrentVerification is the RA6X-023 regression: the
// number of Argon2id verifications running at once must be bounded by an
// explicit memory budget, no matter how many distinct source addresses ask for
// them. Per-IP limiters do not bound this, and the unknown-user path
// deliberately hashes for an account that does not exist.
//
// The test never runs a real 64 MiB KDF: it substitutes a verifier that only
// records concurrency, so the ceiling is observed without exhausting the host.
func TestAuthBudgetCapsConcurrentVerification(t *testing.T) {
	const perVerifyKiB = 64 * 1024 // the default cost
	const slots = 3

	b := NewAuthBudget(perVerifyKiB * slots)

	var (
		inFlight atomic.Int64
		peak     atomic.Int64
		release  = make(chan struct{})
		entered  = make(chan struct{}, 64)
		wg       sync.WaitGroup
	)

	// 40 simultaneous logins from 40 notional source addresses.
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = b.verifyFunc(context.Background(), perVerifyKiB, func() (bool, error) {
				n := inFlight.Add(1)
				for {
					old := peak.Load()
					if n <= old || peak.CompareAndSwap(old, n) {
						break
					}
				}
				entered <- struct{}{}
				<-release
				inFlight.Add(-1)
				return false, nil
			})
		}()
	}

	// Let the admitted set fill up, then hold it long enough that any
	// unbounded implementation would have admitted far more.
	for i := 0; i < slots; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d verifications started; expected %d", i, slots)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if got := peak.Load(); got > slots {
		t.Fatalf("peak concurrent verifications %d exceeds the %d-slot budget", got, slots)
	}

	close(release)
	wg.Wait()

	if got := peak.Load(); got > slots {
		t.Fatalf("peak concurrent verifications %d exceeds the %d-slot budget", got, slots)
	}
	if got := peak.Load(); got < 1 {
		t.Fatal("no verification ran at all")
	}
}

// TestAuthBudgetReleasesOnCancel pins that a client which disconnects while
// queued gives up its place, rather than holding a reservation for a result
// nobody will read.
func TestAuthBudgetReleasesOnCancel(t *testing.T) {
	const perVerifyKiB = 64 * 1024
	b := NewAuthBudget(perVerifyKiB) // exactly one slot

	hold := make(chan struct{})
	started := make(chan struct{})
	go func() {
		_, _ = b.verifyFunc(context.Background(), perVerifyKiB, func() (bool, error) {
			close(started)
			<-hold
			return false, nil
		})
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := b.verifyFunc(ctx, perVerifyKiB, func() (bool, error) {
			t.Error("a cancelled request must not run its verification")
			return false, nil
		})
		done <- err
	}()

	// Give the second request time to queue, then hang up on it.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, errAuthBudgetExhausted) {
			t.Fatalf("cancelled wait returned %v; want errAuthBudgetExhausted", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled request kept waiting for budget")
	}

	close(hold)
}

// TestAuthBudgetBoundsQueue pins that the queue itself is bounded, so a flood
// cannot convert a memory limit into an unbounded goroutine backlog.
func TestAuthBudgetBoundsQueue(t *testing.T) {
	const perVerifyKiB = 64 * 1024
	b := NewAuthBudget(perVerifyKiB)

	hold := make(chan struct{})
	started := make(chan struct{})
	go func() {
		_, _ = b.verifyFunc(context.Background(), perVerifyKiB, func() (bool, error) {
			close(started)
			<-hold
			return false, nil
		})
	}()
	<-started

	// Fill the queue past its cap. Everything beyond it must be refused
	// immediately rather than joining the line.
	var wg sync.WaitGroup
	var refused atomic.Int64
	for i := int64(0); i < b.maxWaiters*3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := b.verifyFunc(context.Background(), perVerifyKiB, func() (bool, error) {
				return false, nil
			})
			if errors.Is(err, errAuthBudgetExhausted) {
				refused.Add(1)
			}
		}()
	}

	// Once the queue is full the excess is refused promptly; release the
	// holder so the queued ones drain.
	time.Sleep(200 * time.Millisecond)
	if refused.Load() == 0 {
		close(hold)
		wg.Wait()
		t.Fatal("an oversubscribed queue admitted every waiter")
	}
	close(hold)
	wg.Wait()
}

// TestVerifyCostRejectsExcessiveHashes pins the other half of RA6X-023: a
// malformed or imported PHC row asking for more work than the verifier accepts
// must be rejected at parse cost, before it can occupy a rationed slot.
func TestVerifyCostRejectsExcessiveHashes(t *testing.T) {
	good, err := auth.HashPassword("x", auth.DefaultParams())
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	cost, err := verifyCost(good)
	if err != nil {
		t.Fatalf("verifyCost(default hash): %v", err)
	}
	if want := int64(auth.DefaultParams().Memory); cost != want {
		t.Fatalf("verifyCost = %d; want %d", cost, want)
	}

	bad := []struct {
		name string
		hash string
	}{
		{"memory beyond the accepted maximum", strings.Replace(good, "m=65536", "m=8388608", 1)},
		{"iterations beyond the accepted maximum", strings.Replace(good, "t=3", "t=100000", 1)},
		{"parallelism beyond the accepted maximum", strings.Replace(good, "p=4", "p=250", 1)},
		{"zero iterations would panic argon2.IDKey", strings.Replace(good, "t=3", "t=0", 1)},
		{"zero parallelism would panic argon2.IDKey", strings.Replace(good, "p=4", "p=0", 1)},
		{"not a PHC string", "not-a-hash"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := verifyCost(tc.hash); err == nil {
				t.Fatalf("verifyCost accepted %q", tc.hash)
			}
			// VerifyPassword must agree, so no caller can reach the work by
			// bypassing the cost check.
			if _, err := auth.VerifyPassword("x", tc.hash); err == nil {
				t.Fatalf("VerifyPassword accepted %q", tc.hash)
			}
		})
	}
}

// An explicit memory budget is a ceiling, even for one imported hash.
func TestAuthBudgetRejectsAnExpensiveHash(t *testing.T) {
	b := NewAuthBudget(1024)
	_, err := b.verifyFunc(context.Background(), auth.MaxMemoryKiB, func() (bool, error) {
		t.Fatal("hash exceeded the explicit memory budget")
		return true, nil
	})
	if !errors.Is(err, errAuthBudgetExhausted) {
		t.Fatalf("got %v", err)
	}
}

func TestVerificationArgon2MinimumMemoryIsBudgeted(t *testing.T) {
	cost, err := verifyCost("$argon2id$v=19$m=8,t=1,p=64$c2FsdA$a2V5")
	if err != nil {
		t.Fatal(err)
	}
	if cost != 512 {
		t.Fatalf("reserved %d KiB for 512 KiB of actual work", cost)
	}
}
