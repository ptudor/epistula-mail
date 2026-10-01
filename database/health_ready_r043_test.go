package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
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

	if code := call(readyHandler(db, root)); code != http.StatusOK {
		t.Errorf("healthy readiness = %d, want 200", code)
	}
	if code := call(readyHandler(db, filepath.Join(root, "does-not-exist"))); code != http.StatusServiceUnavailable {
		t.Errorf("missing storage_root readiness = %d, want 503", code)
	}

	// Postgres unreachable: a lazy pool (SkipPing) pointed at a dead port; the
	// bounded Ping in the handler fails → 503.
	badDB, err := storage.Open(context.Background(), storage.Config{
		DSN:      "postgres://127.0.0.1:1/none?sslmode=disable&connect_timeout=1",
		SkipPing: true,
	})
	if err != nil {
		t.Fatalf("open lazy pool: %v", err)
	}
	defer badDB.Close()
	if code := call(readyHandler(badDB, root)); code != http.StatusServiceUnavailable {
		t.Errorf("PG-down readiness = %d, want 503", code)
	}

	// Liveness is unconditional.
	rr := httptest.NewRecorder()
	healthHandler(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("liveness = %d, want 200", rr.Code)
	}
}
