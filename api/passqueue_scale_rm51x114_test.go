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

// tokenQueueScope is the scope every list/search/export call site passes
// (searchConds): the token's mailboxes, or everything for a wildcard token.
const tokenQueueScope = `($1::bool OR pr.mailbox_id = ANY($2::bigint[]))`

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
		`INSERT INTO annotation_pass_required(message_id, mailbox_id)
		 SELECT m.id, f.mailbox_id FROM messages m JOIN folders f ON f.id = m.folder_id ON CONFLICT DO NOTHING`,
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
	ctx := context.Background()
	var bobMailbox int64
	if err := f.pool.QueryRow(ctx, `SELECT f.mailbox_id FROM messages m JOIN folders f ON f.id = m.folder_id WHERE m.id = $1`, f.bobMsgID).Scan(&bobMailbox); err != nil {
		t.Fatal(err)
	}
	// bob has messages but nothing queued: the shape of a worker whose
	// mailbox is idle while the queue holds 200K markers for someone else.
	if _, err := f.pool.Exec(ctx, `DELETE FROM annotation_pass_required WHERE mailbox_id = $1`, bobMailbox); err != nil {
		t.Fatal(err)
	}
	explain := func(filter string, all bool, scope []int64) string {
		q := fmt.Sprintf(messagePageSelect, "m.text_body", "($1::bool OR mb.id = ANY($2::bigint[])) AND "+filter, 3)
		rows, err := f.pool.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+q, all, scope, 500)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(line + "\n")
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return plan.String()
	}

	// The shape RM51X-114 replaced, for the log.
	t.Logf("original:\n%s", explain(`m.id = ANY(ARRAY(SELECT pr.message_id FROM annotation_pass_required pr))`, true, nil))

	// A wildcard token over a 200K-marker queue walks date order and probes
	// the queue primary key: cost per batch is the batch, not the queue.
	plan := explain(passQueueFilter(tokenQueueScope), true, nil)
	t.Logf("wildcard semi-join:\n%s", plan)
	if !strings.Contains(plan, "idx_messages_date_id") || !strings.Contains(plan, "annotation_pass_required_pkey") {
		t.Fatal("wildcard semi-join did not walk date order and probe the queue primary key")
	}

	// A token scoped to the idle mailbox must not read the other mailbox's
	// markers or walk its messages to find nothing: the scope's own markers
	// come from idx_annotation_pass_required_mailbox (migration 029).
	plan = explain(passQueueFilter(tokenQueueScope), false, []int64{bobMailbox})
	t.Logf("scoped, nothing queued:\n%s", plan)
	if !strings.Contains(plan, "idx_annotation_pass_required_mailbox") {
		t.Fatal("scoped semi-join with nothing queued did not read the queue through its mailbox index")
	}
	if n := sharedHitsRead(t, plan); n > 2000 {
		t.Fatalf("scoped semi-join with nothing queued touched %d buffers", n)
	}
}

// sharedHitsRead returns the top node's shared buffer hits plus reads from an
// EXPLAIN (ANALYZE, BUFFERS) plan, the cheapest honest measure of how much a
// query touched.
func sharedHitsRead(t *testing.T, plan string) int {
	t.Helper()
	for _, line := range strings.Split(plan, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Buffers: shared") {
			continue
		}
		total := 0
		for _, field := range strings.Fields(line) {
			if k, v, ok := strings.Cut(field, "="); ok && (k == "hit" || k == "read") {
				var n int
				if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
					total += n
				}
			}
		}
		return total
	}
	t.Fatalf("no Buffers line in plan:\n%s", plan)
	return 0
}
