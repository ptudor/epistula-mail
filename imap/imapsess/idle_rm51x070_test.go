package imapsess

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type changingIdleGate struct{ current atomic.Pointer[pollGate] }

func (g *changingIdleGate) TraceQueryStart(ctx context.Context, c *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if gate := g.current.Load(); gate != nil {
		return gate.TraceQueryStart(ctx, c, d)
	}
	return ctx
}
func (*changingIdleGate) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func withIdleGate(t *testing.T, s *Session, tracer *changingIdleGate) *pgxpool.Pool {
	t.Helper()
	plain := s.be.Pool
	cfg := plain.Config()
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s.be.Pool = pool
	return plain
}

func TestPollWaitsForIdleReconcile(t *testing.T) {
	s, listener := idlePoolFixture(t, 1)
	defer s.Close()
	listener.Close()
	if _, err := s.Select("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	tracer := new(changingIdleGate)
	withIdleGate(t, s, tracer)
	release := make(chan struct{})
	gate := newPollGate(func(sql string) bool { return strings.HasPrefix(sql, "SELECT uidvalidity,highest_modseq") }, func(ctx context.Context) context.Context { <-release; return ctx })
	tracer.current.Store(gate)
	gate.armed.Store(true)
	stop := make(chan struct{})
	idleDone := make(chan error, 1)
	go func() { idleDone <- s.Idle(nil, stop) }()
	gate.waitHit(t)
	pollDone := make(chan error, 1)
	go func() { pollDone <- s.Poll(nil, true) }()
	select {
	case err := <-pollDone:
		t.Fatalf("Poll passed in-flight IDLE: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(stop)
	close(release)
	for _, done := range []chan error{idleDone, pollDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("IDLE/Poll did not finish")
		}
	}
}

func TestIdleDisconnectDuringReconcileRace(t *testing.T) {
	s, listener := idlePoolFixture(t, 1)
	defer s.Close()
	listener.Close()
	tracer := new(changingIdleGate)
	plain := withIdleGate(t, s, tracer)
	for i := 0; i < 200; i++ {
		c := fetchWire(t, s)
		changeFlags(t, plain, s.selectedFolderID, fmt.Sprintf("flag%d", i))
		release := make(chan struct{})
		gate := newPollGate(isCommit, func(ctx context.Context) context.Context { <-release; return ctx })
		tracer.current.Store(gate)
		gate.armed.Store(true)
		c.send("i IDLE")
		c.until(func(line string) bool { return strings.HasPrefix(line, "+") })
		gate.waitHit(t)
		c.conn.Close()
		close(release)
		waitFetchSessions(t, s.be, 1)
	}
}
