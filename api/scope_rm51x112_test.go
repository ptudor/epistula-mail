package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type scopeQueryTrace struct {
	mu      sync.Mutex
	queries []string
}

func (q *scopeQueryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.queries = append(q.queries, d.SQL)
	return ctx
}

func (*scopeQueryTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func traceScopeQueries(t *testing.T, f *apiFixture) *scopeQueryTrace {
	t.Helper()
	q := &scopeQueryTrace{}
	cfg := f.pool.Config()
	cfg.ConnConfig.Tracer = q
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.pool = pool
	t.Cleanup(pool.Close)
	return q
}

func TestOutOfScopeBodyQueriesNeverRun(t *testing.T) {
	f := newAPIFixture(t)
	if _, err := f.pool.Exec(context.Background(), `UPDATE messages SET text_body=repeat('x',20*1024*1024), html_body=repeat('y',20*1024*1024) WHERE id=$1`, f.bobMsgID); err != nil {
		t.Fatal(err)
	}
	q := traceScopeQueries(t, f)
	for _, suffix := range []string{"", "/text"} {
		wantProblem(t, f.do(http.MethodGet, fmt.Sprintf("/v1/messages/%d%s", f.bobMsgID, suffix), f.aliceContentToken, nil), http.StatusForbidden)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.queries) == 0 {
		t.Fatal("no queries traced")
	}
	for _, query := range q.queries {
		if strings.Contains(query, "text_body") || strings.Contains(query, "html_body") {
			t.Fatalf("out-of-scope request selected body: %s", query)
		}
	}
}

func TestFolderAndCategorySQLScopePredicates(t *testing.T) {
	f := newArchiveFixture(t)
	q := traceScopeQueries(t, f.apiFixture)
	for _, suffix := range []string{"folders", "archive-categories"} {
		wantProblem(t, f.do(http.MethodGet, "/v1/mailboxes/alice/"+suffix, f.bobToken, nil), http.StatusForbidden)
		for _, token := range []string{f.aliceContentToken, f.classifierToken} {
			resp := f.do(http.MethodGet, "/v1/mailboxes/alice/"+suffix, token, nil)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatal(resp.Status)
			}
		}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	checked := 0
	for _, sql := range q.queries {
		if strings.Contains(sql, "FROM folders f") || strings.Contains(sql, "FROM archive_categories ac") {
			if !strings.Contains(sql, "$2::bool OR") || !strings.Contains(sql, "mailbox_id = ANY($3::bigint[])") {
				t.Fatalf("scope missing: %s", sql)
			}
			checked++
		}
	}
	if checked != 6 {
		t.Fatalf("checked %d queries, want 6", checked)
	}
}
