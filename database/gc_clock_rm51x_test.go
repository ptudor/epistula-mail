package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

type sweepMaintenanceTracer struct{ count atomic.Int64 }

func (t *sweepMaintenanceTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "SELECT EXISTS (SELECT 1 FROM mailboxes WHERE maintenance_at IS NOT NULL)") {
		t.count.Add(1)
	}
	return ctx
}
func (*sweepMaintenanceTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func traceSweepMaintenance(t *testing.T, db *storage.DB) (*storage.DB, *atomic.Int64) {
	t.Helper()
	cfg := db.Pool().Config()
	tracer := &sweepMaintenanceTracer{}
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return storage.NewFromPool(pool, storage.Config{}), &tracer.count
}

func TestSweepGraceUsesDatabaseClock(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := context.Background()
	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `INSERT INTO gc_candidates(tenant,sha256,kind,bucket,first_seen_at,generation_marked)
 SELECT 'gone',decode(lpad(to_hex(n),64,'0'),'hex'),'raw','2020/01/01',now()-make_interval(hours=>n),floor(extract(epoch FROM now()))::bigint FROM unnest(ARRAY[23,25])n`); err != nil {
		t.Fatal(err)
	}
	if code := gcSweep(ctx, db, store, 24*time.Hour); code != EX_OK {
		t.Fatalf("sweep=%d", code)
	}
	var count int
	var hours float64
	if err := db.Pool().QueryRow(ctx, `SELECT count(*),max(extract(epoch FROM now()-first_seen_at)/3600) FROM gc_candidates`).Scan(&count, &hours); err != nil {
		t.Fatal(err)
	}
	if count != 1 || hours < 23 || hours > 24 {
		t.Fatalf("remaining=%d age=%f", count, hours)
	}
}
