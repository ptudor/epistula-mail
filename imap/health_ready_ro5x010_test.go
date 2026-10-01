package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestReadyHandlerRO5X010 mirrors epistula-api's R-043 test: /healthz is a real
// readiness probe (503 when PG is unreachable or storage_root is missing)
// while /health stays pure liveness. Both used to share the always-200
// handler, so monitoring could not tell "process alive" from "backend
// usable".
func TestReadyHandlerRO5X010(t *testing.T) {
	db, _ := pgtest.Open(t)
	root := t.TempDir()

	call := func(h http.HandlerFunc) (int, string) {
		rr := httptest.NewRecorder()
		h(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		return rr.Code, rr.Body.String()
	}

	if code, body := call(readyHandler(db.Pool(), root)); code != http.StatusOK {
		t.Errorf("healthy readiness = %d (%s), want 200", code, body)
	}

	// storage_root missing.
	code, body := call(readyHandler(db.Pool(), filepath.Join(root, "nope")))
	if code != http.StatusServiceUnavailable {
		t.Errorf("missing storage_root readiness = %d, want 503", code)
	}
	if !strings.Contains(body, "storage_root") {
		t.Errorf("503 body should name the failing half; got %q", body)
	}

	// storage_root is a file, not a directory.
	f := filepath.Join(root, "afile")
	if err := os.WriteFile(f, []byte("x"), 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}
	if code, _ := call(readyHandler(db.Pool(), f)); code != http.StatusServiceUnavailable {
		t.Errorf("file-as-storage_root readiness = %d, want 503", code)
	}

	// Postgres unreachable: a lazy pool pointed at a dead port.
	badPool, err := pgxpool.New(context.Background(),
		"postgres://127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("open lazy pool: %v", err)
	}
	defer badPool.Close()
	code, body = call(readyHandler(badPool, root))
	if code != http.StatusServiceUnavailable {
		t.Errorf("PG-down readiness = %d, want 503", code)
	}
	if !strings.Contains(body, "postgres") {
		t.Errorf("503 body should name postgres; got %q", body)
	}

	// Liveness is unconditional.
	rr := httptest.NewRecorder()
	healthHandler(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("liveness = %d, want 200", rr.Code)
	}
}

// TestOpenPoolSurvivesUnreachablePostgres is the startup half of RO5X-010:
// the daemon must come up with a lazy pool rather than exiting EX_OSERR when
// Postgres happens to be down, which under rc.d (no restart supervisor) meant
// staying dead until an operator noticed.
func TestOpenPoolSurvivesUnreachablePostgres(t *testing.T) {
	cfg := &Config{}
	cfg.Postgres.DSN = "postgres://127.0.0.1:1/none?sslmode=disable&connect_timeout=1"
	cfg.Postgres.MaxOpenConns = 4
	cfg.Postgres.MaxIdleConns = 0

	pool, err := openPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("openPool returned an error for an unreachable server; "+
			"it must return a lazy pool and let the daemon start (RO5X-010): %v", err)
	}
	if pool == nil {
		t.Fatal("openPool returned a nil pool without an error")
	}
	defer pool.Close()

	// Readiness must report the truth while the backend is down.
	rr := httptest.NewRecorder()
	readyHandler(pool, t.TempDir())(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("readiness with PG down = %d, want 503", rr.Code)
	}
}

// TestOpenPoolStillRejectsUnparseableDSN keeps the one fatal case fatal: a
// malformed DSN is a config error, not a transient backend.
func TestOpenPoolStillRejectsUnparseableDSN(t *testing.T) {
	cfg := &Config{}
	cfg.Postgres.DSN = "://not a dsn at all"

	pool, err := openPool(context.Background(), cfg)
	if err == nil {
		if pool != nil {
			pool.Close()
		}
		t.Fatal("openPool accepted an unparseable DSN; that must stay fatal")
	}
}
