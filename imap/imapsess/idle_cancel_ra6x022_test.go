package imapsess

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestIdleStopsWhileWaitingForAConnection is the RA6X-022 regression, run with
// the IDLE pool set to exactly one connection.
//
// The stop channel used to be converted into cancellation only AFTER the pool
// acquisition and the LISTEN, and the acquisition ran on context.Background().
// With the dedicated pool full — twenty simultaneous listeners across all
// accounts is the default, so this is ordinary — a second IDLE waited on
// Acquire forever: DONE could not reach it, closing the connection could not
// reach it, and neither could SIGTERM.
func TestIdleStopsWhileWaitingForAConnection(t *testing.T) {
	sess, holder := idlePoolFixture(t, 1)

	// Occupy the pool's single connection for the duration.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	held, err := holder.Acquire(ctx)
	if err != nil {
		t.Fatalf("hold the only IDLE connection: %v", err)
	}
	defer held.Release()

	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- sess.Idle(nil, stop) }()

	// Let it reach the blocked Acquire, then say DONE.
	time.Sleep(150 * time.Millisecond)
	close(stop)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("IDLE returned %v; want a clean exit", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("IDLE did not return after DONE while waiting for a connection")
	}
}

// TestIdleStopsOnSessionCloseWhileWaiting pins the other two cancellation
// sources: the connection going away, and daemon shutdown.
// Both arrive as the session context being cancelled.
func TestIdleStopsOnSessionCloseWhileWaiting(t *testing.T) {
	sess, holder := idlePoolFixture(t, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	held, err := holder.Acquire(ctx)
	if err != nil {
		t.Fatalf("hold the only IDLE connection: %v", err)
	}
	defer held.Release()

	// A stop channel that is never closed: only the session's own lifetime can
	// end this IDLE.
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- sess.Idle(nil, stop) }()

	time.Sleep(150 * time.Millisecond)
	_ = sess.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("IDLE returned %v; want a clean exit", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("IDLE did not return when its session closed while waiting for a connection")
	}
}

// TestIdleReleasesItsConnection pins that a finished IDLE gives its connection
// back, so the pool returns to baseline rather than leaking a slot per session.
func TestIdleReleasesItsConnection(t *testing.T) {
	sess, holder := idlePoolFixture(t, 1)

	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- sess.Idle(nil, stop) }()

	// Give it time to acquire and LISTEN, then stop it.
	time.Sleep(200 * time.Millisecond)
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("IDLE did not return on DONE")
	}

	// The pool's connection must be available again immediately.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := holder.Acquire(ctx)
	if err != nil {
		t.Fatalf("the IDLE connection was not released: %v", err)
	}
	conn.Release()
}

// idlePoolFixture returns a session whose backend has a dedicated IDLE pool of
// exactly size connections, plus that pool so a test can occupy it.
func idlePoolFixture(t *testing.T, size int32) (*Session, *pgxpool.Pool) {
	t.Helper()
	sess := mutationFixture(t)

	_, dsn := pgtest.Open(t)
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = size
	cfg.MinConns = 0

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("idle pool: %v", err)
	}
	t.Cleanup(pool.Close)

	sess.be.IdlePool = pool
	// A short heartbeat so a test that lets IDLE reach its wait does not sit
	// for 29 minutes.
	sess.be.IdleHeartbeat = 500 * time.Millisecond
	// A LONG setup timeout, deliberately: the tests assert that cancellation
	// ends a blocked acquisition, and a short bound would end it anyway and
	// hide a broken cancellation path.
	sess.be.StmtTimeout = 60 * time.Second
	return sess, pool
}
