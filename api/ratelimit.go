package main

import (
	"sync"
	"time"
)

// rateLimiter is a per-token (keyed by token id) token-bucket limiter. It
// blunts a valid but misbehaving/compromised token hammering expensive
// endpoints (/raw streaming, limit=500 FTS pages) without affecting a
// well-behaved consumer. Disabled when rate <= 0 (the default), so a
// deployment opts in via [limits].
//
// It mirrors the auth-fail map's discipline: one mutex, lazy prune of idle
// buckets so the map stays bounded by the count of recently-active tokens.
type rateLimiter struct {
	rate  float64 // tokens refilled per second; <= 0 disables the limiter
	burst float64 // bucket capacity

	mu        sync.Mutex
	buckets   map[int64]*tokenBucket
	opsCount  int // allow() calls since the last prune sweep
	pruneEach int // sweep cadence
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(rate float64, burst int) *rateLimiter {
	return &rateLimiter{
		rate:      rate,
		burst:     float64(burst),
		buckets:   make(map[int64]*tokenBucket),
		pruneEach: 1024,
	}
}

// allow reports whether a request from token id may proceed, consuming one
// token from its bucket. A disabled limiter always allows.
func (rl *rateLimiter) allow(id int64) bool {
	if rl == nil || rl.rate <= 0 {
		return true
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	rl.opsCount++
	if rl.opsCount >= rl.pruneEach {
		rl.pruneLocked(now)
		rl.opsCount = 0
	}

	b := rl.buckets[id]
	if b == nil {
		// A fresh token starts with a full burst allowance.
		rl.buckets[id] = &tokenBucket{tokens: rl.burst - 1, last: now}
		return true
	}
	b.tokens += now.Sub(b.last).Seconds() * rl.rate
	if b.tokens > rl.burst {
		b.tokens = rl.burst
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// pruneLocked drops buckets that have fully refilled (idle long enough to be
// back at capacity), so the map does not grow with every token ever seen. The
// caller holds rl.mu.
func (rl *rateLimiter) pruneLocked(now time.Time) {
	for id, b := range rl.buckets {
		refilled := b.tokens + now.Sub(b.last).Seconds()*rl.rate
		if refilled >= rl.burst {
			delete(rl.buckets, id)
		}
	}
}
