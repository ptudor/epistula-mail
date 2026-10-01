package imapsess

import (
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"
)

func TestPerIPLimiterAcquireRelease(t *testing.T) {
	l := NewPerIPLimiter(2)
	if !l.Acquire("1.1.1.1") {
		t.Fatal("1st should succeed")
	}
	if !l.Acquire("1.1.1.1") {
		t.Fatal("2nd should succeed (at cap)")
	}
	if l.Acquire("1.1.1.1") {
		t.Error("3rd should fail (over cap)")
	}
	// Different IP unaffected.
	if !l.Acquire("2.2.2.2") {
		t.Error("different IP should succeed")
	}
	l.Release("1.1.1.1")
	if !l.Acquire("1.1.1.1") {
		t.Error("after release a slot reopens")
	}
}

func TestPerIPLimiterZeroCapAlwaysAllows(t *testing.T) {
	l := NewPerIPLimiter(0)
	for i := 0; i < 100; i++ {
		if !l.Acquire("x.x.x.x") {
			t.Fatalf("zero-cap limiter should always allow (iter %d)", i)
		}
	}
}

func TestMailboxSessionLimiter(t *testing.T) {
	l := NewMailboxSessionLimiter(3)
	for i := 0; i < 3; i++ {
		if !l.Acquire(42) {
			t.Fatalf("iter %d should succeed", i)
		}
	}
	if l.Acquire(42) {
		t.Error("4th should fail")
	}
	// Different mailbox unaffected.
	if !l.Acquire(43) {
		t.Error("different mailbox blocked unexpectedly")
	}
	l.Release(42)
	if !l.Acquire(42) {
		t.Error("release should reopen a slot")
	}
}

func TestLoginThrottleLocksAfterAttempts(t *testing.T) {
	tt := NewLoginThrottle(3, 60*time.Second, 30*time.Second)
	now := time.Now()
	tt.now = func() time.Time { return now }

	if !tt.Allow("1.1.1.1") {
		t.Fatal("initial Allow should succeed")
	}
	tt.RecordResult("1.1.1.1", false)
	tt.RecordResult("1.1.1.1", false)
	tt.RecordResult("1.1.1.1", false)
	if tt.Allow("1.1.1.1") {
		t.Error("3rd failure should trip lockout")
	}
	// A different IP is unaffected.
	if !tt.Allow("2.2.2.2") {
		t.Error("different IP should be allowed")
	}
	// After cooldown elapses, the IP becomes allowed again.
	now = now.Add(31 * time.Second)
	if !tt.Allow("1.1.1.1") {
		t.Error("after cooldown the IP should be allowed")
	}
}

func TestLoginThrottleSuccessClearsCounter(t *testing.T) {
	tt := NewLoginThrottle(2, 60*time.Second, 30*time.Second)
	now := time.Now()
	tt.now = func() time.Time { return now }

	tt.RecordResult("1.1.1.1", false) // 1 fail
	tt.RecordResult("1.1.1.1", true)  // clear
	tt.RecordResult("1.1.1.1", false) // 1 fail again
	if !tt.Allow("1.1.1.1") {
		t.Error("after success-clear, single failure must not lock out")
	}
}

func TestLoginThrottleWindowExpires(t *testing.T) {
	tt := NewLoginThrottle(2, 10*time.Second, 30*time.Second)
	now := time.Now()
	tt.now = func() time.Time { return now }

	tt.RecordResult("1.1.1.1", false)
	now = now.Add(11 * time.Second) // first failure falls outside window
	tt.RecordResult("1.1.1.1", false)
	if !tt.Allow("1.1.1.1") {
		t.Error("expired failures should not contribute to lockout")
	}
}

func TestLoginThrottleZeroAttemptsDisables(t *testing.T) {
	tt := NewLoginThrottle(0, time.Second, time.Second)
	for i := 0; i < 50; i++ {
		tt.RecordResult("1.1.1.1", false)
	}
	if !tt.Allow("1.1.1.1") {
		t.Error("zero-attempts throttle should always allow")
	}
}

func TestLimitedListenerRefusesOverCap(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer inner.Close()
	addr := inner.Addr().String()

	limiter := NewPerIPLimiter(1)
	wrapped := NewLimitedListener(inner, limiter, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Server goroutine accepts forever until inner closes.
	accepted := make(chan net.Conn, 4)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := wrapped.Accept()
			if err != nil {
				close(accepted)
				return
			}
			accepted <- c
		}
	}()

	// 1st connection: kept.
	c1, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	defer c1.Close()
	first := <-accepted
	defer first.Close()

	// 2nd connection: should be refused inline (we'll get a BYE then EOF).
	c2, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	defer c2.Close()
	// Expect the BYE message then EOF.
	buf := make([]byte, 256)
	_ = c2.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _ := c2.Read(buf)
	if n == 0 || string(buf[:n]) == "" {
		t.Error("expected BYE message from refused connection")
	}

	// Closing the first connection must release the slot so a 3rd attempt
	// from the same IP succeeds.
	first.Close()
	c1.Close()
	time.Sleep(50 * time.Millisecond) // let limiter.Release run
	if limiter.Active("127.0.0.1") != 0 {
		t.Errorf("expected 0 active after close, got %d", limiter.Active("127.0.0.1"))
	}

	c3, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial 3: %v", err)
	}
	defer c3.Close()
	select {
	case third := <-accepted:
		third.Close()
	case <-time.After(2 * time.Second):
		t.Error("3rd connection should have been accepted after slot reopened")
	}

	inner.Close()
	wg.Wait()
}
