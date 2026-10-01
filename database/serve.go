package main

import (
	"context"
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

	"github.com/ptudor/epistula-mail/database/storage"
)

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

	// Open the pool LAZILY (SkipPing) so serve starts even when Postgres is
	// briefly down; /healthz reports NOT-ready until it recovers (R-043). Only
	// a missing/malformed DSN fails here, which is a genuine config error.
	db, err := storage.Open(ctx, storage.Config{
		DSN:                     cfg.Postgres.DSN,
		StatementTimeout:        cfg.StatementTimeoutDuration(),
		SessionStatementTimeout: true,
		SkipPing:                true,
		// Honour the documented pool knobs. Every path except the two
		// importers used to omit these, leaving pgxpool's defaults
		// (MaxConns = max(4, NumCPU)) — so on a 32-core box the metrics/health
		// listener, which needs at most one connection, opened 32 (RO5X-021).
		MaxConns:        int32(cfg.Postgres.MaxOpenConns),
		MinConns:        int32(cfg.Postgres.MaxIdleConns),
		ConnMaxLifetime: cfg.ConnMaxLifetimeDuration(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres: %v\n", err)
		return EX_CONFIG
	}
	defer db.Close()

	mux := http.NewServeMux()
	if cfg.HTTP.ExposeMetrics {
		registerServeMetrics(db)
		mux.Handle("/metrics", metricsHandler())
		// expvar mirrors the "Prometheus + expvar" pattern the root CLAUDE.md
		// promises and epistula-imap and epistula-api already ship; same loopback-only exposure
		// rules as /metrics (RO5X-024).
		mux.Handle("/debug/vars", expvar.Handler())
	}
	// /health is pure liveness (the process is up); /healthz is readiness — a
	// bounded PG ping + a stat of storage_root, 503 if either fails (R-043).
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/healthz", readyHandler(db, cfg.Storage.Root))

	srv := &http.Server{
		Addr:         cfg.HTTP.ListenAddr,
		Handler:      mux,
		ReadTimeout:  time.Duration(cfg.HTTP.ReadTimeoutSec) * time.Second,
		WriteTimeout: time.Duration(cfg.HTTP.WriteTimeoutSec) * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	slog.Info("serve listening", "addr", cfg.HTTP.ListenAddr, "production", cfg.Production)

	select {
	case <-ctx.Done():
		slog.Info("serve shutting down on signal")
	case err := <-errCh:
		slog.Error("server error", "err", err)
		return EX_OSERR
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(),
		time.Duration(cfg.HTTP.ShutdownTimeoutSec)*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("serve shutdown error", "err", err)
	}
	return EX_OK
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

// readyHandler is the readiness probe (R-043): a bounded (~2s) Postgres ping
// plus a stat of storage_root. Returns 503 if either fails, so monitoring
// catches a down backend even while /health (liveness) stays 200. Loopback-only
// and unauthenticated — the same surface as /metrics.
func readyHandler(db *storage.DB, storageRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Pool().Ping(ctx); err != nil {
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
