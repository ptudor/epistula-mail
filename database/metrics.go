package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ptudor/epistula-mail/database/storage"
)

var metricsRegistry = prometheus.NewRegistry()

func init() {
	metricsRegistry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}

func metricsHandler() http.Handler {
	return promhttp.HandlerFor(metricsRegistry, promhttp.HandlerOpts{Registry: metricsRegistry})
}

// registerServeMetrics adds the application gauges for the long-lived `serve`
// process (RO5X-024).
//
// The registry previously held only NewGoCollector + NewProcessCollector —
// not a single mail_database_* series — while epistula-imap and epistula-api export five app
// collectors plus /debug/vars, and the root CLAUDE.md promises "Prometheus +
// expvar endpoints".
//
// The reason no per-delivery counters exist is structural and correct
// (hooks.go): `deliver` is a short-lived process whose counters would never be
// scraped, and R-049's reasoning stands — nothing here adds metrics to that
// path. But `serve` IS long-lived and can export state that matters, so these
// are gauge-funcs evaluated at scrape time, bounded by the session
// statement_timeout the serve pool already sets.
func registerServeMetrics(db *storage.DB) {
	scalar := func(name, help, query string) {
		metricsRegistry.MustRegister(prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{Name: name, Help: help},
			func() float64 {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				var v float64
				if err := db.Pool().QueryRow(ctx, query).Scan(&v); err != nil {
					// A scrape must never fail the process; report -1 so a
					// broken query is visible on the dashboard rather than
					// silently reading as zero.
					slog.Warn("metrics query failed", "metric", name, "err", err)
					return -1
				}
				return v
			}))
	}

	scalar("mail_database_gc_candidates",
		"Blobs currently queued for GC sweep.",
		`SELECT count(*)::float8 FROM gc_candidates`)
	scalar("mail_database_gc_candidate_oldest_age_seconds",
		"Age of the oldest gc_candidates row, in seconds. Grows without bound if sweep is not running.",
		`SELECT COALESCE(EXTRACT(epoch FROM now() - min(first_seen_at)), 0)::float8 FROM gc_candidates`)
	scalar("mail_database_mailboxes",
		"Number of mailboxes.",
		`SELECT count(*)::float8 FROM mailboxes`)
	scalar("mail_database_messages",
		"Number of stored messages.",
		`SELECT count(*)::float8 FROM messages`)
	scalar("mail_database_used_bytes_total",
		"Sum of mailboxes.used_bytes across all mailboxes.",
		`SELECT COALESCE(sum(used_bytes), 0)::float8 FROM mailboxes`)
	scalar("mail_database_mailboxes_over_quota",
		"Mailboxes whose used_bytes exceeds their quota_bytes.",
		`SELECT count(*)::float8 FROM mailboxes WHERE quota_bytes IS NOT NULL AND used_bytes > quota_bytes`)

	// Delivery outcomes over a rolling window, so a spike in rejections is
	// visible without shipping per-delivery counters from the LDA.
	metricsRegistry.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "mail_database_delivery_log_recent_total",
			Help: "delivery_log rows in the last hour.",
		},
		func() float64 {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var v float64
			if err := db.Pool().QueryRow(ctx,
				`SELECT count(*)::float8 FROM delivery_log WHERE received_at > now() - interval '1 hour'`,
			).Scan(&v); err != nil {
				slog.Warn("metrics query failed", "metric", "delivery_log_recent_total", "err", err)
				return -1
			}
			return v
		}))
	metricsRegistry.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "mail_database_delivery_log_recent_rejected",
			Help: "delivery_log rows in the last hour whose outcome is a rejection.",
		},
		func() float64 {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var v float64
			if err := db.Pool().QueryRow(ctx,
				`SELECT count(*)::float8 FROM delivery_log
				  WHERE received_at > now() - interval '1 hour'
				    AND outcome LIKE 'rejected:%'`,
			).Scan(&v); err != nil {
				slog.Warn("metrics query failed", "metric", "delivery_log_recent_rejected", "err", err)
				return -1
			}
			return v
		}))
}
