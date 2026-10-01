package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestReadyHandler is the R-043 regression: /healthz reports 503 when Postgres
// is unreachable or storage_root is missing, while /health stays 200 liveness.
func TestReadyHandler(t *testing.T) {
	db, _ := pgtest.Open(t)
	root := t.TempDir()

	call := func(h http.HandlerFunc) int {
		rr := httptest.NewRecorder()
		h(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		return rr.Code
	}

	if code := call(readyHandler(db.Pool(), root)); code != http.StatusOK {
		t.Errorf("healthy readiness = %d, want 200", code)
	}
	if code := call(readyHandler(db.Pool(), filepath.Join(root, "does-not-exist"))); code != http.StatusServiceUnavailable {
		t.Errorf("missing storage_root readiness = %d, want 503", code)
	}

	// Postgres unreachable: a lazy pool (pgxpool.New doesn't connect until use)
	// pointed at a dead port; the handler's bounded Ping fails → 503.
	badPool, err := pgxpool.New(context.Background(),
		"postgres://127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("open lazy pool: %v", err)
	}
	defer badPool.Close()
	if code := call(readyHandler(badPool, root)); code != http.StatusServiceUnavailable {
		t.Errorf("PG-down readiness = %d, want 503", code)
	}

	rr := httptest.NewRecorder()
	healthHandler(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("liveness = %d, want 200", rr.Code)
	}
}
