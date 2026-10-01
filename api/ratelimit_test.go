package main

import "testing"

// TestRateLimiterBucket is the R-017 core: a burst beyond the bucket capacity
// yields rejections; a disabled or nil limiter always allows.
func TestRateLimiterBucket(t *testing.T) {
	rl := newRateLimiter(2, 3) // 2/s refill, burst 3
	allowed, rejected := 0, 0
	for i := 0; i < 10; i++ {
		if rl.allow(42) {
			allowed++
		} else {
			rejected++
		}
	}
	if allowed < 1 {
		t.Errorf("no requests allowed under burst=3; got allowed=%d", allowed)
	}
	if rejected < 1 {
		t.Errorf("burst of 10 under rate=2 burst=3 produced no rejection; allowed=%d", allowed)
	}
	// A different token id has its own independent bucket.
	if !rl.allow(99) {
		t.Error("a fresh token id should be allowed immediately")
	}
}

func TestRateLimiterDisabledAndNil(t *testing.T) {
	disabled := newRateLimiter(0, 0)
	for i := 0; i < 1000; i++ {
		if !disabled.allow(1) {
			t.Fatal("disabled limiter (rate=0) rejected a request")
		}
	}
	var nilLimiter *rateLimiter
	if !nilLimiter.allow(1) {
		t.Fatal("nil limiter should allow (treated as disabled)")
	}
}
