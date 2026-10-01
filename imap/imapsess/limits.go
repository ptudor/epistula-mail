// Per-IP connection caps, per-mailbox session caps, and per-IP login
// throttling. All three rate limits already have TOML knobs in config;
// this file does the enforcement so the daemon actually honors them.
//
// Three independent mechanisms because they answer different questions:
//
//   - PerIPLimiter (listener-level): rejects new TCP connections from
//     an IP that already has N open. Stops a misbehaving / compromised
//     client from monopolizing the listener.
//   - MailboxSessionLimiter (post-auth): rejects login when the same
//     mailbox already has N concurrent sessions. Catches a legitimate
//     account being credential-stuffed across many clients.
//   - LoginThrottle (per-IP): exponential lockout after repeated
//     failed Login() calls from the same IP. Defeats password-spray.
package imapsess

import (
	"log/slog"
	"net"
	"sync"
	"time"
)

// PerIPLimiter caps simultaneous connections per remote IP. A counter is
// bumped on Accept and decremented when the listener-wrapper sees a
// connection close. Excess connections are accepted at the kernel and
// then immediately closed with a "BYE Too many connections" line written
// to the client; this matches what go-imap's server expects to write at
// session creation time but avoids needing a session at all.
type PerIPLimiter struct {
	maxPerIP int

	mu      sync.Mutex
	current map[string]int
}

// NewPerIPLimiter returns a limiter with the given cap. A non-positive
// cap disables the limit (returns a limiter that always permits).
func NewPerIPLimiter(maxPerIP int) *PerIPLimiter {
	return &PerIPLimiter{
		maxPerIP: maxPerIP,
		current:  make(map[string]int),
	}
}

// Acquire returns true if a connection from ip is allowed. On a true
// return the caller MUST eventually call Release to drop the count.
func (l *PerIPLimiter) Acquire(ip string) bool {
	if l == nil || l.maxPerIP <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.current[ip] >= l.maxPerIP {
		return false
	}
	l.current[ip]++
	return true
}

// Release decrements the count for ip. Safe to call on an IP that
// wasn't previously Acquire'd (the count clamps at zero).
func (l *PerIPLimiter) Release(ip string) {
	if l == nil || l.maxPerIP <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.current[ip] > 0 {
		l.current[ip]--
	}
	if l.current[ip] == 0 {
		delete(l.current, ip)
	}
}

// Active returns the current count for ip. For tests / metrics.
func (l *PerIPLimiter) Active(ip string) int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.current[ip]
}

// MailboxSessionLimiter caps concurrent sessions per mailbox ID.
type MailboxSessionLimiter struct {
	maxPerMailbox int

	mu      sync.Mutex
	current map[int64]int
}

func NewMailboxSessionLimiter(maxPerMailbox int) *MailboxSessionLimiter {
	return &MailboxSessionLimiter{
		maxPerMailbox: maxPerMailbox,
		current:       make(map[int64]int),
	}
}

func (l *MailboxSessionLimiter) Acquire(mailboxID int64) bool {
	if l == nil || l.maxPerMailbox <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.current[mailboxID] >= l.maxPerMailbox {
		return false
	}
	l.current[mailboxID]++
	return true
}

func (l *MailboxSessionLimiter) Release(mailboxID int64) {
	if l == nil || l.maxPerMailbox <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.current[mailboxID] > 0 {
		l.current[mailboxID]--
	}
	if l.current[mailboxID] == 0 {
		delete(l.current, mailboxID)
	}
}

// LoginThrottle implements a per-IP rolling-window failure count with a
// post-lockout cooldown. On Allow() returning false the caller must NOT
// proceed with the password-hash comparison — letting an attacker burn
// CPU on Argon2id verifies is exactly the resource they want.
//
// Semantics:
//   - Each Allow() returning true permits one attempt.
//   - After a successful Allow() the caller calls RecordResult(ip, ok)
//     with the verification outcome. ok=true clears the IP's record;
//     ok=false increments the failure counter.
//   - When failures within `window` reach `attempts`, the IP is locked
//     out for `cooldown`. While locked, Allow() returns false
//     immediately.
//   - Records older than max(window, cooldown) age out lazily on next
//     access for that IP — no background goroutine.
type LoginThrottle struct {
	attempts int
	window   time.Duration
	cooldown time.Duration

	mu    sync.Mutex
	state map[string]*loginState
	now   func() time.Time // overridden in tests

	// recordsSinceGC drives the amortized global sweep in RecordResult.
	// Guarded by mu.
	recordsSinceGC int
}

// throttleSweepInterval bounds how often RecordResult runs the opportunistic
// global prune. Matches epistula-api's failsSweepInterval (R-069).
const throttleSweepInterval = 256

type loginState struct {
	failures   []time.Time
	lockedTill time.Time
}

func NewLoginThrottle(attempts int, window, cooldown time.Duration) *LoginThrottle {
	return &LoginThrottle{
		attempts: attempts,
		window:   window,
		cooldown: cooldown,
		state:    make(map[string]*loginState),
		now:      time.Now,
	}
}

// Allow reports whether ip may attempt a login right now. Returns false
// if the IP is currently locked out.
func (t *LoginThrottle) Allow(ip string) bool {
	if t == nil || t.attempts <= 0 {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.state[ip]
	if !ok {
		return true
	}
	now := t.now()
	if now.Before(s.lockedTill) {
		return false
	}
	t.gcLocked(ip, s, now)
	return true
}

// RecordResult records the outcome of a login attempt. ok=true clears
// the IP's failure record; ok=false bumps the counter and may trip the
// lockout.
func (t *LoginThrottle) RecordResult(ip string, ok bool) {
	if t == nil || t.attempts <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if ok {
		delete(t.state, ip)
		return
	}
	s, exists := t.state[ip]
	if !exists {
		s = &loginState{}
		t.state[ip] = s
	}
	s.failures = append(s.failures, now)
	t.gcLocked(ip, s, now)

	// Count failures within the window.
	cutoff := now.Add(-t.window)
	fresh := s.failures[:0]
	for _, f := range s.failures {
		if f.After(cutoff) {
			fresh = append(fresh, f)
		}
	}
	s.failures = fresh
	if len(s.failures) >= t.attempts {
		s.lockedTill = now.Add(t.cooldown)
		s.failures = nil // reset on lockout
	}

	t.sweepLocked(now)
}

// sweepLocked drops inert per-IP records. Caller holds t.mu.
//
// gcLocked only ever touches the IP being recorded, so an address that fails
// fewer than `attempts` times and never returns keeps its entry for the life
// of the process. A credential spray from a large pool — trivially cheap over
// IPv6, where a single /64 offers 2^64 sources — therefore grows t.state
// without bound until the daemon is OOM-killed, taking IMAP down for
// everyone. One connection and one failed LOGIN per source is enough, so the
// per-IP connection cap does not help (RO5X-006).
//
// Every throttleSweepInterval records, drop every IP that is no longer
// locked out and whose newest failure has aged past the window. Amortized
// O(1) per failure, no background goroutine — mirroring epistula-api's R-069.
func (t *LoginThrottle) sweepLocked(now time.Time) {
	t.recordsSinceGC++
	if t.recordsSinceGC < throttleSweepInterval {
		return
	}
	t.recordsSinceGC = 0

	// A record whose failures have aged out is inert, but one whose lockout
	// has not elapsed must survive — evicting it would hand an attacker a
	// lockout reset. Hence max(window, cooldown) as the horizon and the
	// explicit lockedTill guard.
	horizon := t.window
	if t.cooldown > horizon {
		horizon = t.cooldown
	}
	cutoff := now.Add(-horizon)
	for ip, st := range t.state {
		if now.Before(st.lockedTill) {
			continue // still locked out — keep
		}
		if len(st.failures) == 0 || !st.failures[len(st.failures)-1].After(cutoff) {
			delete(t.state, ip)
		}
	}
}

// gcLocked clears expired state. Caller holds t.mu.
func (t *LoginThrottle) gcLocked(ip string, s *loginState, now time.Time) {
	if now.After(s.lockedTill) && len(s.failures) == 0 {
		delete(t.state, ip)
	}
}

// remoteIP extracts the IP literal from a net.Addr, dropping the port.
// Empty string for nil or unparseable addresses (which the caller
// treats as "no IP-based limit applies").
func remoteIP(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}

// LimitedListener wraps a net.Listener and rejects Accept'd connections
// from IPs that already have the per-IP cap. Rejected connections get
// a short IMAP-style "BYE" line and are closed immediately so the
// client gets a useful signal rather than silent EOF.
//
// Close on the returned listener also closes the underlying listener.
type LimitedListener struct {
	net.Listener
	limiter *PerIPLimiter
	logger  *slog.Logger
}

// NewLimitedListener wraps inner with a per-IP cap. If limiter is nil or
// has a non-positive cap, the wrapper just passes through.
func NewLimitedListener(inner net.Listener, limiter *PerIPLimiter, logger *slog.Logger) *LimitedListener {
	if logger == nil {
		logger = slog.Default()
	}
	return &LimitedListener{Listener: inner, limiter: limiter, logger: logger}
}

// Accept loops until it returns a permitted connection or the inner
// listener errors out. Rejected connections are closed in-line and
// don't surface to the caller — go-imap's server expects every Accept
// to yield a session-worthy conn.
func (l *LimitedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		ip := remoteIP(c.RemoteAddr())
		if ip == "" || l.limiter.Acquire(ip) {
			return &limitedConn{Conn: c, limiter: l.limiter, ip: ip}, nil
		}
		_, _ = c.Write([]byte("* BYE Too many connections from your IP\r\n"))
		_ = c.Close()
		metricLimiterRejections.WithLabelValues("per_ip").Inc()
		l.logger.Warn("connection refused: per-ip cap", "ip", ip)
	}
}

// limitedConn calls limiter.Release exactly once, when the underlying
// net.Conn is closed.
type limitedConn struct {
	net.Conn
	limiter   *PerIPLimiter
	ip        string
	closeOnce sync.Once
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.closeOnce.Do(func() {
		c.limiter.Release(c.ip)
	})
	return err
}

// tokenBucket is the per-session command-rate limiter (the
// command_rate_per_sec config key). It detects a misbehaving or
// compromised client looping commands; the per-IP connection cap already
// bounds connection-level abuse. A nil bucket allows everything.
type tokenBucket struct {
	mu     sync.Mutex
	rate   float64 // tokens added per second
	burst  float64 // bucket capacity (one second of tokens)
	tokens float64
	last   time.Time
}

// newTokenBucket returns a bucket allowing ratePerSec commands per second
// with a one-second burst, or nil (unlimited) for ratePerSec <= 0.
func newTokenBucket(ratePerSec int) *tokenBucket {
	if ratePerSec <= 0 {
		return nil
	}
	r := float64(ratePerSec)
	return &tokenBucket{rate: r, burst: r, tokens: r, last: time.Now()}
}

// allow consumes one token, refilling first. Nil receiver always allows.
func (b *tokenBucket) allow() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tokens += now.Sub(b.last).Seconds() * b.rate
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
