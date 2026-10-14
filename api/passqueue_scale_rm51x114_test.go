package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const semiJoinPassQueue = `EXISTS (SELECT 1 FROM annotation_pass_required pr WHERE pr.message_id = m.id)`

func seedLargePassQueue(t *testing.T, f *apiFixture) {
	t.Helper()
	ctx := context.Background()
	_, err := f.pool.Exec(ctx, `INSERT INTO messages(folder_id,uid,internal_date,raw_sha256,raw_size,text_body)
		SELECT folder_id,1000+g,'2026-01-01'::timestamptz+g*interval '1 second',raw_sha256,raw_size,'queue scale body'
		FROM messages m CROSS JOIN generate_series(1,200000) g WHERE m.id=$1`, f.aliceMsgIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`INSERT INTO annotation_pass_required(message_id) SELECT id FROM messages ON CONFLICT DO NOTHING`,
		`ANALYZE messages`, `ANALYZE annotation_pass_required`,
	} {
		if _, err := f.pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
}

type exportBatchTrace struct {
	mu        sync.Mutex
	durations []time.Duration
}
type exportBatchStartKey struct{}

func (*exportBatchTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "ORDER BY m.internal_date DESC, m.id DESC") {
		return context.WithValue(ctx, exportBatchStartKey{}, time.Now())
	}
	return ctx
}
func (q *exportBatchTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if start, ok := ctx.Value(exportBatchStartKey{}).(time.Time); ok {
		q.mu.Lock()
		defer q.mu.Unlock()
		q.durations = append(q.durations, time.Since(start))
	}
}

func TestLargePassQueueExport(t *testing.T) {
	f := newAPIFixture(t)
	seedLargePassQueue(t, f)
	f.srv.passQueueAvailable = true
	f.srv.cfg.Limits.ExportPageSize = 500
	q := &exportBatchTrace{}
	cfg := f.pool.Config()
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "10000"
	cfg.ConnConfig.Tracer = q
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f.srv.pool = pool
	resp := f.do(http.MethodGet, "/v1/export?pass_required=true", f.classifierToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
	dec := json.NewDecoder(resp.Body)
	count := 0
	for {
		var item struct {
			ID    int64           `json:"id"`
			Error json.RawMessage `json:"error"`
		}
		err := dec.Decode(&item)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if item.ID == 0 || len(item.Error) != 0 {
			t.Fatalf("export error row: %+v", item)
		}
		count++
	}
	var want int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM annotation_pass_required`).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if count != want || count < 200000 {
		t.Fatalf("exported %d, want %d", count, want)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.durations) < 400 {
		t.Fatalf("only %d batches", len(q.durations))
	}
	median := func(ds []time.Duration) time.Duration {
		copy := append([]time.Duration(nil), ds...)
		sort.Slice(copy, func(i, j int) bool { return copy[i] < copy[j] })
		return copy[len(copy)/2]
	}
	n := len(q.durations) / 4
	early, late := median(q.durations[:n]), median(q.durations[len(q.durations)-n:])
	t.Logf("exported %d rows in %d batches; first-quarter median %s, last-quarter median %s", count, len(q.durations), early, late)
	if late > early*5+20*time.Millisecond {
		t.Fatalf("batch cost grew with cursor depth: %s -> %s", early, late)
	}
}

func TestLargePassQueueExplain(t *testing.T) {
	f := newAPIFixture(t)
	seedLargePassQueue(t, f)
	for _, tc := range []struct{ name, filter string }{
		{"original", `m.id = ANY(ARRAY(SELECT pr.message_id FROM annotation_pass_required pr))`},
		{"semi-join", semiJoinPassQueue},
	} {
		q := fmt.Sprintf(messagePageSelect, "m.text_body", "($1::bool OR mb.id = ANY($2::bigint[])) AND "+tc.filter, 3)
		rows, err := f.pool.Query(context.Background(), "EXPLAIN (ANALYZE, BUFFERS) "+q, true, []int64{}, 500)
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
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s:\n%s", tc.name, plan.String())
		if tc.name == "semi-join" && (!strings.Contains(plan.String(), "idx_messages_date_id") || !strings.Contains(plan.String(), "annotation_pass_required_pkey")) {
			t.Fatal("semi-join did not walk date order and probe the queue primary key")
		}
	}
}
