package main

import (
	"context"
	"encoding/json"
	"errors"
	"expvar"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/maintenance"
	"github.com/ptudor/epistula-mail/database/storage"
)

// server carries the shared state every handler needs. All request state
// flows through parameters — the daemon is stateless per the design.
type server struct {
	cfg       *Config
	pool      *pgxpool.Pool
	store     *blob.Store
	auth      *authenticator
	exportSem chan struct{}
	limiter   *rateLimiter

	// annotationsAvailable is probed at startup: the annotation endpoint
	// returns 503 until the epistula-database migration that creates
	// message_annotations has been applied.
	annotationsAvailable bool

	// modelPriorityAvailable is probed at startup: when the annotation_models
	// registry (epistula-database migration 008) is present, a message's
	// annotations are returned ranked by model priority with the winner
	// flagged primary. Absent, they fall back to model-name order with no
	// priority/primary fields.
	modelPriorityAvailable bool

	// classificationsAvailable is probed at startup: archive categories, the
	// classification PUT and the not_classified filter need epistula-database
	// migration 020 AND its grants to this role. Without either they answer
	// 503; the rest of the API is unaffected.
	classificationsAvailable bool

	// passQueueAvailable is probed at startup: pass_required=true and the
	// prune endpoint need epistula-database migration 022 and this role's SELECT
	// and DELETE on annotation_pass_required. Without them they answer 503.
	passQueueAvailable bool
}

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to TOML config file")
	if err := fs.Parse(args); err != nil {
		return EX_USAGE
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return EX_CONFIG
	}
	setupLogging(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// SessionStatementTimeout bounds every query on every pooled
	// connection — the API issues reads outside explicit transactions, so
	// the session-level guard is the right scope (vs. the LDA's SET LOCAL).
	db, err := storage.Open(ctx, storage.Config{
		DSN:                     cfg.Postgres.DSN,
		StatementTimeout:        cfg.StatementTimeoutDuration(),
		MaxConns:                int32(cfg.Postgres.MaxOpenConns),
		MinConns:                int32(cfg.Postgres.MaxIdleConns),
		ConnMaxLifetime:         cfg.ConnMaxLifetimeDuration(),
		SessionStatementTimeout: true,
	})
	if err != nil {
		slog.Error("postgres", "err", err)
		return EX_OSERR
	}
	defer db.Close()

	lease, lerr := maintenance.Acquire(ctx, cfg.Storage.Root, db.Pool(), false, false)
	if lerr != nil {
		slog.Error("offline maintenance barrier", "err", lerr)
		return EX_OSERR
	}
	lease.HoldUntilExit()
	store := blob.NewStore(cfg.Storage.Root)
	if cfg.Production {
		if err := store.CheckPermissions(); err != nil {
			slog.Error("blob store permissions", "err", err)
			return EX_CONFIG
		}
	}

	srv := &server{
		cfg:       cfg,
		pool:      db.Pool(),
		store:     store,
		exportSem: make(chan struct{}, cfg.Limits.MaxExportStreams),
		limiter:   newRateLimiter(cfg.Limits.PerTokenRate, cfg.Limits.PerTokenBurst),
	}
	srv.auth = newAuthenticator(srv)

	// The annotation sidecar gates on its epistula-database migration; the
	// read surface ships regardless.
	probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
	var annotationsRel *string
	if err := srv.pool.QueryRow(probeCtx, `SELECT to_regclass('message_annotations')::text`).Scan(&annotationsRel); err != nil {
		probeCancel()
		slog.Error("probe message_annotations", "err", err)
		return EX_OSERR
	}
	// Probe both existence AND SELECT privilege. to_regclass returns the OID
	// even when this least-privilege role lacks SELECT, so keying availability
	// on it alone would make every ranked read fail `permission denied for
	// table annotation_models`. has_table_privilege gives the real answer, so
	// a missing GRANT degrades to unranked annotations (WARN below) instead of
	// 500ing GET /v1/messages/{id}, fields=annotation, and every export batch.
	var modelsSelectable *bool
	if err := srv.pool.QueryRow(probeCtx, `SELECT CASE
	                WHEN to_regclass('annotation_models') IS NULL THEN NULL
	                ELSE has_table_privilege('annotation_models', 'SELECT')
	            END`).Scan(&modelsSelectable); err != nil {
		probeCancel()
		slog.Error("probe annotation_models", "err", err)
		return EX_OSERR
	}
	// Archive sorting (migration 020): the tables must exist AND this role
	// must hold the grants the migration gives it, or every call would fail
	// with "permission denied" instead of a clear 503.
	var classificationsUsable *bool
	if err := srv.pool.QueryRow(probeCtx, `SELECT CASE
	                WHEN to_regclass('message_classifications') IS NULL
	                  OR to_regclass('archive_categories') IS NULL THEN NULL
	                ELSE has_table_privilege('message_classifications', 'INSERT')
	                 AND has_table_privilege('message_classifications', 'SELECT')
	                 AND has_table_privilege('archive_categories', 'SELECT')
	            END`).Scan(&classificationsUsable); err != nil {
		probeCancel()
		slog.Error("probe message_classifications", "err", err)
		return EX_OSERR
	}
	// The annotation pass queue (migration 022), on the same terms.
	var passQueueUsable *bool
	if err := srv.pool.QueryRow(probeCtx, `SELECT CASE
	                WHEN to_regclass('annotation_pass_required') IS NULL THEN NULL
	                ELSE has_table_privilege('annotation_pass_required', 'SELECT')
	                 AND has_table_privilege('annotation_pass_required', 'DELETE')
	            END`).Scan(&passQueueUsable); err != nil {
		probeCancel()
		slog.Error("probe annotation_pass_required", "err", err)
		return EX_OSERR
	}
	probeCancel()
	srv.passQueueAvailable = passQueueUsable != nil && *passQueueUsable
	srv.classificationsAvailable = classificationsUsable != nil && *classificationsUsable
	srv.annotationsAvailable = annotationsRel != nil
	srv.modelPriorityAvailable = modelsSelectable != nil && *modelsSelectable
	if !srv.annotationsAvailable {
		slog.Warn("message_annotations table absent; PUT /v1/messages/{id}/annotation will return 503 until the epistula-database migration is applied")
	}
	if !srv.classificationsAvailable {
		slog.Warn("archive classification tables absent or not usable by this role; archive-categories, PUT /v1/messages/{id}/classification and not_classified will return 503 (apply epistula-database migration 020; see deploy/README.md for grants)")
	}
	if !srv.passQueueAvailable {
		slog.Warn("annotation pass queue absent or not usable by this role; pass_required and POST /v1/pass-required/prune will return 503 (apply epistula-database migration 022; see deploy/README.md for grants)")
	}
	if srv.annotationsAvailable && !srv.modelPriorityAvailable {
		slog.Warn("annotation_models registry absent or not SELECT-able by this role; annotations are served unranked (GRANT SELECT ON annotation_models, or apply epistula-database migration 008)")
	}

	apiSrv := &http.Server{
		Addr:    cfg.Server.ListenAddr,
		Handler: srv.routes(),
		// ReadTimeout bounds slow request bodies (the annotation PUT is
		// the only body-bearing endpoint and is small). WriteTimeout is
		// configurable and defaults to unlimited because /v1/export
		// streams are legitimately long-lived; Apache fronts this
		// listener and owns client-liveness enforcement.
		ReadTimeout:       time.Duration(cfg.Server.ReadTimeoutSec) * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      time.Duration(cfg.Server.WriteTimeoutSec) * time.Second,
		IdleTimeout:       time.Duration(cfg.Server.IdleTimeoutSec) * time.Second,
	}
	adminSrv := startAdminListener(cfg, srv.pool)

	apiErrCh := make(chan error, 1)
	go func() {
		if err := apiSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			apiErrCh <- err
		}
	}()

	slog.Info("epistula-api running",
		"api_listen", cfg.Server.ListenAddr,
		"admin_listen", cfg.Admin.ListenAddr,
		"production", cfg.Production,
		"annotations", srv.annotationsAvailable,
		"classifications", srv.classificationsAvailable,
		"pass_queue", srv.passQueueAvailable,
	)

	rc := EX_OK
	select {
	case <-ctx.Done():
		slog.Info("epistula-api shutting down on signal")
	case err := <-apiErrCh:
		slog.Error("api listener error", "err", err)
		rc = EX_OSERR
	case err := <-adminSrv.errCh:
		slog.Error("admin listener error", "err", err)
		rc = EX_OSERR
	}

	// Drain: stop accepting, let in-flight requests (including export
	// streams) finish within the shutdown timeout, then close the pool
	// via the deferred db.Close.
	shutCtx, cancel := context.WithTimeout(context.Background(),
		time.Duration(cfg.Server.ShutdownTimeoutSec)*time.Second)
	if err := apiSrv.Shutdown(shutCtx); err != nil {
		slog.Warn("api shutdown", "err", err)
	}
	cancel()
	adminSrv.shutdown(cfg)
	return rc
}

// routes builds the API mux. Folder names may contain "/" (Archive/2026),
// so the messages listing matches a trailing wildcard and the handler
// strips the "/messages" suffix itself.
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /v1/mailboxes", s.api("/v1/mailboxes", s.handleMailboxes))
	mux.Handle("GET /v1/mailboxes/{mailbox}/folders", s.api("/v1/mailboxes/{mailbox}/folders", s.handleFolders))
	mux.Handle("GET /v1/mailboxes/{mailbox}/folders/{rest...}", s.api("/v1/mailboxes/{mailbox}/folders/{folder}/messages", s.handleFolderMessages))
	mux.Handle("GET /v1/messages/{id}", s.api("/v1/messages/{id}", s.handleMessage))
	mux.Handle("GET /v1/messages/{id}/text", s.api("/v1/messages/{id}/text", s.handleMessageText))
	mux.Handle("GET /v1/messages/{id}/raw", s.api("/v1/messages/{id}/raw", s.handleMessageRaw))
	mux.Handle("PUT /v1/messages/{id}/annotation", s.api("/v1/messages/{id}/annotation", s.handleAnnotationPut))
	mux.Handle("POST /v1/pass-required/prune", s.api("/v1/pass-required/prune", s.handlePassRequiredPrune))
	mux.Handle("PUT /v1/messages/{id}/classification", s.api("/v1/messages/{id}/classification", s.handleClassificationPut))
	mux.Handle("GET /v1/mailboxes/{mailbox}/archive-categories", s.api("/v1/mailboxes/{mailbox}/archive-categories", s.handleArchiveCategories))
	mux.Handle("GET /v1/search", s.api("/v1/search", s.handleSearch))
	mux.Handle("GET /v1/export", s.api("/v1/export", s.handleExport))
	// No bare "/" catch-all: it would answer unmatched paths as 404 without
	// auth or metrics (a 401-vs-404 path-enumeration oracle) and swallow method
	// mismatches as 404 instead of 405. The fallback wrapper routes both
	// through s.api (auth + metrics + panic isolation) and RFC 7807 (R-068).
	return s.fallback(mux)
}

// captureRecorder is a throwaway ResponseWriter used to inspect what the mux's
// built-in fallback handler would emit (405 + Allow vs 404) without sending it.
type captureRecorder struct {
	header http.Header
	status int
}

func (c *captureRecorder) Header() http.Header         { return c.header }
func (c *captureRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (c *captureRecorder) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}

// fallback wraps the mux so unmatched requests (unknown path, or a known path
// with the wrong method) are authenticated, metered, and answered as RFC 7807 —
// a method mismatch as 405 with an Allow header, everything else as 404 (R-068).
func (s *server) fallback(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, pattern := mux.Handler(r)
		if pattern != "" {
			// Real match. Use mux.ServeHTTP so path values are populated; the
			// registered handler is already s.api-wrapped.
			mux.ServeHTTP(w, r)
			return
		}
		// No path+method match. Probe the mux's own fallback handler to learn
		// whether this is a method mismatch (405 + Allow) or a genuine 404.
		pr := &captureRecorder{header: http.Header{}}
		h.ServeHTTP(pr, r)
		allow := pr.header.Get("Allow")
		methodMismatch := pr.status == http.StatusMethodNotAllowed && allow != ""

		s.api("fallback", func(w http.ResponseWriter, r *http.Request, _ *apiToken) {
			if methodMismatch {
				problemMethodNotAllowed(w, r, allow)
				return
			}
			problemNotFound(w, r, "No such endpoint.")
		}).ServeHTTP(w, r)
	})
}

// api wraps a handler with authentication, metrics, and panic isolation.
// Permission and scope checks are per-handler — several endpoints need
// parameter-dependent permissions (fields=text escalates to read_content).
func (s *server) api(route string, h func(http.ResponseWriter, *http.Request, *apiToken)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			p := recover()
			if p != nil && p != http.ErrAbortHandler {
				slog.Error("handler panic", "route", route, "panic", p)
				if !rec.wrote {
					problemInternal(rec, r)
				}
			}
			metricRequestsTotal.WithLabelValues(route, fmt.Sprintf("%d", rec.status)).Inc()
			metricRequestDuration.WithLabelValues(route).Observe(time.Since(start).Seconds())
			if p == http.ErrAbortHandler {
				panic(p)
			}
		}()

		tok := s.auth.authenticate(rec, r)
		if tok == nil {
			return
		}
		// Per-token request rate limit (disabled unless configured). Keyed by
		// token id so a compromised/runaway token can't hammer /raw or FTS
		// pages; the export stream semaphore is separate and unchanged. (R-017)
		if !s.limiter.allow(tok.ID) {
			problemTooMany(rec, r, "Per-token rate limit exceeded; slow down.")
			return
		}
		h(rec, r, tok)
	})
}

// statusRecorder captures the response code for metrics and supports
// Flush for the NDJSON export stream.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.wrote = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

// ResponseController prefers FlushError; preserve the underlying error instead
// of letting the legacy Flusher wrapper turn a failed stream into success.
func (r *statusRecorder) FlushError() error {
	r.wrote = true
	return http.NewResponseController(r.ResponseWriter).Flush()
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the ResponseWriter this recorder wraps, so
// http.ResponseController can reach the connection underneath it (RA6X-063).
//
// Without it, every ResponseController capability — SetWriteDeadline above
// all — answered http.ErrNotSupported for every handler in this server, since
// they all run behind s.api. The export stream's write-progress deadline was
// silently not in force: the code asked for it, the wrapper swallowed the
// request, and a stalled reader kept its semaphore slot exactly as before.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// writeJSON emits a JSON success response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("response encode", "err", err)
	}
}

// ---- admin (metrics/health) listener ----

type adminServer struct {
	srv   *http.Server
	errCh <-chan error
}

func startAdminListener(cfg *Config, pool *pgxpool.Pool) *adminServer {
	mux := http.NewServeMux()
	if cfg.Admin.ExposeMetrics {
		mux.Handle("/metrics", metricsHandler())
		mux.Handle("/debug/vars", expvar.Handler())
	}
	// /health is pure liveness; /healthz is readiness — a bounded PG ping + a
	// stat of storage_root, 503 if either fails (R-043).
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/healthz", readyHandler(pool, cfg.Storage.Root))

	srv := &http.Server{
		Addr:         cfg.Admin.ListenAddr,
		Handler:      mux,
		ReadTimeout:  time.Duration(cfg.Admin.ReadTimeoutSec) * time.Second,
		WriteTimeout: time.Duration(cfg.Admin.WriteTimeoutSec) * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	return &adminServer{srv: srv, errCh: errCh}
}

func (a *adminServer) shutdown(cfg *Config) {
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(cfg.Admin.ShutdownTimeoutSec)*time.Second)
	defer cancel()
	if err := a.srv.Shutdown(ctx); err != nil {
		slog.Warn("admin shutdown", "err", err)
	}
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

// readyHandler is the readiness probe (R-043): a bounded (~2s) Postgres ping
// plus a stat of storage_root. Returns 503 if either fails so monitoring
// catches a down backend even while /health (liveness) stays 200. Loopback-only
// and unauthenticated — the same surface as /metrics.
func readyHandler(pool *pgxpool.Pool, storageRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "not ready: postgres: %v\n", err)
			return
		}
		fi, err := os.Stat(storageRoot)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "not ready: storage_root: %v\n", err)
			return
		}
		if !fi.IsDir() {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "not ready: storage_root %q is not a directory\n", storageRoot)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ready")
	}
}
