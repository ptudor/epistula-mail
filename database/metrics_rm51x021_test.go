package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

type metricQueryTracer struct {
	mu           sync.Mutex
	queries      map[string]bool
	active, peak atomic.Int64
}

func (t *metricQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	t.queries[d.SQL] = true
	t.mu.Unlock()
	active := t.active.Add(1)
	for old := t.peak.Load(); active > old; old = t.peak.Load() {
		if t.peak.CompareAndSwap(old, active) {
			break
		}
	}
	return ctx
}
func (t *metricQueryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
	t.active.Add(-1)
}

func TestMetricEstimatesUnderScrapeLoad(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := context.Background()
	mb := gcMustMailbox(t, ctx, db, "metrics")
	folder, err := db.LookupOrCreateFolder(ctx, mb, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO messages(folder_id,uid,raw_sha256,raw_size,internal_date) SELECT $1,n,decode(md5(n::text)||md5(n::text),'hex'),1,now() FROM generate_series(1,100000)n`,
	} {
		if _, err := db.Pool().Exec(ctx, query, folder); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Pool().Exec(ctx, `INSERT INTO gc_candidates(tenant,sha256,kind,bucket,first_seen_at,generation_marked) SELECT 'gone',decode(md5(n::text)||md5(n::text),'hex'),'raw','2020/01/01',now()-interval '2 days',1 FROM generate_series(1,100000)n`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `INSERT INTO delivery_log(envelope_to,bytes,outcome,received_at) SELECT 'a@example.invalid',1,'rejected:unknown',now()-interval '2 days' FROM generate_series(1,100000)n`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `ANALYZE`); err != nil {
		t.Fatal(err)
	}
	cfg := db.Pool().Config()
	tracer := &metricQueryTracer{queries: map[string]bool{}}
	cfg.ConnConfig.Tracer = tracer
	cfg.MaxConns = 4
	cfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	previous := metricsRegistry
	metricsRegistry = prometheus.NewRegistry()
	defer func() { metricsRegistry = previous }()
	var logs bytes.Buffer
	logger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(logger)
	registerServeMetrics(storage.NewFromPool(pool, storage.Config{}))
	server := httptest.NewServer(metricsHandler())
	defer server.Close()
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(server.URL)
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Error(err)
			}
			if resp.StatusCode != 200 || !strings.Contains(string(body), "mail_database_messages_estimate") || strings.Contains(string(body), " -1\n") {
				t.Errorf("bad metrics: %s", body)
			}
		}()
		time.Sleep(100 * time.Millisecond)
	}
	wg.Wait()
	if strings.Contains(logs.String(), "metrics query failed") {
		t.Fatal(logs.String())
	}
	if tracer.peak.Load() > 4 {
		t.Fatalf("query peak=%d", tracer.peak.Load())
	}
	tracer.mu.Lock()
	queries := make([]string, 0, len(tracer.queries))
	for q := range tracer.queries {
		queries = append(queries, q)
	}
	tracer.mu.Unlock()
	if len(queries) != 8 {
		t.Fatalf("gauge queries=%d", len(queries))
	}
	for _, q := range queries {
		rows, err := db.Pool().Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+q)
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(line + "\n")
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s\n%s", q, plan.String())
		if strings.Contains(q, "pg_stat_user_tables") && (strings.Contains(plan.String(), " on messages ") || strings.Contains(plan.String(), " on gc_candidates ")) {
			t.Fatal("estimate scanned live table")
		}
	}
	t.Logf("20 scrapes at 10/s; peak active gauge queries=%d; no warnings", tracer.peak.Load())
}
