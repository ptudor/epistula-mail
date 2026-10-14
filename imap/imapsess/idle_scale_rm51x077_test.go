package imapsess

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type idlePollTracer struct{ counters atomic.Int64 }

func (t *idlePollTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "SELECT uidvalidity,highest_modseq FROM folders") {
		t.counters.Add(1)
	}
	return ctx
}
func (*idlePollTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestFiftyIdleSessionsBoundFallbackPolls(t *testing.T) {
	fixture := mutationFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := fixture.be.Pool.Config()
	tracer := &idlePollTracer{}
	cfg.ConnConfig.Tracer = tracer
	cfg.MaxConns = 40
	cfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	idleCfg := fixture.be.Pool.Config()
	idleCfg.MaxConns = 20
	idleCfg.MinConns = 0
	idlePool, err := pgxpool.NewWithConfig(ctx, idleCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer idlePool.Close()
	be := &Backend{Pool: pool, IdlePool: idlePool, Logger: fixture.be.Logger, StmtTimeout: time.Second, IdleHeartbeat: time.Minute, IdlePollInterval: 500 * time.Millisecond}
	var wg sync.WaitGroup
	for range 50 {
		s := be.NewSession()
		s.mailboxID = fixture.mailboxID
		s.selectedFolderID = fixture.selectedFolderID
		if err := s.ensureView(ctx); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer s.Close()
			if err := s.idleConsume(ctx, nil, make(chan struct{})); err != nil {
				t.Error(err)
			}
		}()
	}
	defer func() { cancel(); wg.Wait() }()
	deadline := time.Now().Add(3 * time.Second)
	for tracer.counters.Load() < 50 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if tracer.counters.Load() < 50 {
		t.Fatal("sessions did not start")
	}
	if got := idlePool.Stat().AcquiredConns(); got != 20 {
		t.Fatalf("listeners=%d", got)
	}
	before := tracer.counters.Load()
	time.Sleep(1100 * time.Millisecond)
	polls := tracer.counters.Load() - before
	// Thirty fallback sessions may cross two or three 500 ms tick boundaries.
	if polls < 60 || polls > 90 {
		t.Fatalf("polls=%d want 60..90", polls)
	}
	t.Logf("50 sessions, 20 listeners, %d fallback queries over 1.1s at 500ms interval", polls)
	// Main pool remains usable while every LISTEN slot is occupied.
	queryCtx, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	if err := pool.Ping(queryCtx); err != nil {
		t.Fatal(err)
	}
}
