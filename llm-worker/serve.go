package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func runOnce(args []string) int {
	fs := flag.NewFlagSet("run-once", flag.ContinueOnError)
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
	worker, err := NewWorker(cfg)
	if err != nil {
		slog.Error("worker init", "err", err)
		return EX_CONFIG
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stats, err := worker.RunOnce(ctx)
	if err != nil {
		slog.Error("run-once failed", "err", err)
		return EX_UNAVAILABLE
	}
	slog.Info("run-once complete",
		"scanned", stats.Scanned,
		"skipped", stats.Skipped,
		"annotated", stats.Annotated,
		"classified", stats.Classified,
		"failed", stats.Failed,
		"infra_failed", stats.InfraFailed)
	return EX_OK
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
	worker, err := NewWorker(cfg)
	if err != nil {
		slog.Error("worker init", "err", err)
		return EX_CONFIG
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	state := &atomicRunState{}
	adminSrv := startAdminListener(cfg, state)

	slog.Info("epistula-llm-worker running",
		"mail_api", cfg.MailAPI.BaseURL,
		"lmstudio", cfg.LMStudio.BaseURL,
		"model", cfg.LMStudio.Model,
		"annotation_model", cfg.Worker.AnnotationModel,
		"interval_seconds", cfg.Worker.IntervalSec,
		"admin_listen", cfg.Admin.ListenAddr,
		"production", cfg.Production,
		"dry_run", cfg.Worker.DryRun)

	rc := EX_OK
	loopDone := runLoop(ctx, worker, state)
	select {
	case <-ctx.Done():
		// SIGTERM/Interrupt: join the worker goroutine (bounded) so the final
		// pass's log line/stats aren't lost and an in-flight PUT isn't torn
		// down mid-request by process exit (R-055).
		waitLoop(loopDone, cfg)
	case <-loopDone:
		// interval_seconds <= 0: the loop ran its single pass and exited on its
		// own. Terminate cleanly instead of blocking on ctx.Done() forever
		// while /healthz keeps reporting healthy and nothing annotates (R-057).
		slog.Info("worker loop finished (single pass; interval_seconds <= 0), exiting")
	case err := <-adminSrv.errCh:
		slog.Error("admin listener error", "err", err)
		rc = EX_OSERR
	}
	adminSrv.shutdown(cfg)
	return rc
}

// waitLoop blocks until the worker loop goroutine has closed done, bounded by
// the admin shutdown timeout so a wedged pass cannot hang termination (R-055).
func waitLoop(done <-chan struct{}, cfg *Config) {
	timeout := time.Duration(cfg.Admin.ShutdownTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		slog.Warn("worker loop did not finish within shutdown timeout; exiting anyway")
	}
}

func runLoop(ctx context.Context, worker *Worker, state *atomicRunState) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		interval := time.Duration(worker.cfg.Worker.IntervalSec) * time.Second
		sched := newRoundSchedule(time.Duration(worker.cfg.Worker.CompleteRoundIntervalSec) * time.Second)
		for {
			state.running.Store(true)
			state.lastStarted.Store(time.Now().Unix())
			round := sched.next(time.Now())
			started := time.Now()
			stats, err := worker.runRound(ctx, round)
			if errors.Is(err, errPassQueueUnavailable) {
				if sched.queueLost() {
					slog.Warn("epistula-api has no annotation pass queue; running complete rounds until it does (apply epistula-database migration 022 and deploy its epistula-api)")
				}
				round, started = roundComplete, time.Now()
				stats, err = worker.runRound(ctx, round)
			}
			sched.finished(round, started, err)
			state.lastFinished.Store(time.Now().Unix())
			state.running.Store(false)
			state.setError(err)
			metricRunsTotal.Add(1)
			if round == roundComplete {
				metricCompleteRounds.Add(1)
			}
			if err != nil {
				metricRunsFailed.Add(1)
				slog.Error("worker pass failed", "round", round, "err", err)
			} else {
				attrs := []any{
					"round", round,
					"scanned", stats.Scanned,
					"skipped", stats.Skipped,
					"annotated", stats.Annotated,
					"classified", stats.Classified,
					"failed", stats.Failed,
					"infra_failed", stats.InfraFailed,
					"deferred", stats.Deferred,
				}
				if stats.QueueKnown {
					attrs = append(attrs, "queue_pruned", stats.QueuePruned, "queue_remaining", stats.QueueRemaining)
				}
				slog.Info("worker pass complete", attrs...)
			}
			if interval <= 0 {
				return
			}
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return done
}

type adminServer struct {
	srv   *http.Server
	errCh <-chan error
}

func startAdminListener(cfg *Config, state *atomicRunState) *adminServer {
	mux := http.NewServeMux()
	if cfg.Admin.ExposeMetrics {
		mux.HandleFunc("/metrics", metricsHandler)
	}
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"running":       state.running.Load(),
			"last_started":  state.lastStarted.Load(),
			"last_finished": state.lastFinished.Load(),
			"last_error":    state.errorString(),
		})
	})
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
