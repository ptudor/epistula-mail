package main

import (
	"net/http"
	"testing"
)

// TestAPIPerTokenRateLimit is the R-017 wire-level check: with a tight limiter,
// a burst on one token yields at least one 429; the response is the standard
// problem shape (status only asserted here).
func TestAPIPerTokenRateLimit(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.limiter = newRateLimiter(2, 3) // 2/s, burst 3

	got429 := false
	for i := 0; i < 10; i++ {
		resp := f.do(http.MethodGet, "/v1/mailboxes", f.classifierToken, nil)
		if resp.StatusCode == http.StatusTooManyRequests {
			got429 = true
		}
		resp.Body.Close()
	}
	if !got429 {
		t.Fatal("burst of 10 requests under rate=2/s burst=3 produced no 429")
	}
}

// TestAuthArgonSemaphoreRejects is the R-016 check: when the Argon2 verify
// semaphore is saturated, an uncached (cache-missing) request is refused 429
// before running the KDF, rather than piling up a 64 MiB verification.
func TestAuthArgonSemaphoreRejects(t *testing.T) {
	f := newAPIFixture(t)

	// Saturate the Argon2 semaphore so the verify slot is unavailable.
	n := cap(f.srv.auth.argonSem)
	for i := 0; i < n; i++ {
		f.srv.auth.argonSem <- struct{}{}
	}
	defer func() {
		for i := 0; i < n; i++ {
			<-f.srv.auth.argonSem
		}
	}()

	// An uncached bearer reaches the semaphore gate before verify.
	resp := f.do(http.MethodGet, "/v1/mailboxes", "mapi_uncached_probe_token", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("saturated-Argon request = %d, want 429", resp.StatusCode)
	}
}
