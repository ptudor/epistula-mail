package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type exportAnnotationTracer struct{ queries atomic.Int64 }

func (t *exportAnnotationTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "FROM message_annotations") && strings.Contains(d.SQL, "ORDER BY") {
		t.queries.Add(1)
	}
	return ctx
}
func (*exportAnnotationTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestExportLoadsAnnotationsOncePerBatch(t *testing.T) {
	f := newAPIFixture(t)
	cfg := f.srv.pool.Config()
	tracer := &exportAnnotationTracer{}
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	f.srv.pool = pool
	f.srv.cfg.Limits.ExportPageSize = 500
	resp := f.do(http.MethodGet, "/v1/export", f.classifierToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	decoder := json.NewDecoder(resp.Body)
	n := 0
	for {
		var row map[string]any
		err := decoder.Decode(&row)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if row["error"] != nil {
			t.Fatal(row)
		}
		n++
	}
	if n != 8 || tracer.queries.Load() != 1 {
		t.Fatalf("rows=%d annotation queries=%d", n, tracer.queries.Load())
	}
}

func TestPageBudgetChargesAllStages(t *testing.T) {
	b := newPageBudget(100)
	for range 3 {
		if !b.admit(25) {
			t.Fatal("base item refused")
		}
	}
	if n := b.grow(0, 20); n != 3 || b.used != 95 {
		t.Fatalf("attachments: rows=%d used=%d", n, b.used)
	}
	if n := b.grow(0, 20); n != 2 || b.used != 90 {
		t.Fatalf("annotations: rows=%d used=%d", n, b.used)
	}
	if n := b.grow(0, 100); n != 1 || b.used != 165 {
		t.Fatalf("first complete row: rows=%d used=%d", n, b.used)
	}
	if n := b.grow(2, 999); n != 1 || b.used != 165 {
		t.Fatal("deferred sidecar retained")
	}
}
