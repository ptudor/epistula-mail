package main

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/imap/imapsess"
)

func TestDrainSessionsWaitsForClose(t *testing.T) {
	backend := &imapsess.Backend{}
	s1 := backend.NewSession()
	s2 := backend.NewSession()
	if backend.ActiveSessions() != 2 {
		t.Fatalf("active = %d, want 2", backend.ActiveSessions())
	}

	var forced atomic.Bool
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = s1.Close()
		_ = s2.Close()
	}()

	start := time.Now()
	drainSessions(backend, func() { forced.Store(true) }, 5*time.Second)
	if forced.Load() {
		t.Error("forceClose fired even though sessions closed within the timeout")
	}
	if backend.ActiveSessions() != 0 {
		t.Errorf("active = %d after drain, want 0", backend.ActiveSessions())
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("drain took %v, should return promptly after sessions close", elapsed)
	}
}

func TestDrainSessionsForceClosesOnTimeout(t *testing.T) {
	backend := &imapsess.Backend{}
	s := backend.NewSession() // never closed voluntarily

	var forced atomic.Bool
	drainSessions(backend, func() {
		forced.Store(true)
		_ = s.Close() // the real forceClose tears down conns, running Close hooks
	}, 300*time.Millisecond)

	if !forced.Load() {
		t.Fatal("forceClose did not fire on drain timeout")
	}
	if backend.ActiveSessions() != 0 {
		t.Errorf("active = %d after force close, want 0", backend.ActiveSessions())
	}
}

func TestSessionCloseIsIdempotentForCounter(t *testing.T) {
	backend := &imapsess.Backend{}
	s := backend.NewSession()
	_ = s.Close()
	_ = s.Close() // double Close must not double-decrement
	if got := backend.ActiveSessions(); got != 0 {
		t.Fatalf("active = %d after double close, want 0", got)
	}
}
