package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ptudor/epistula-mail/database/archive"
	"github.com/ptudor/epistula-mail/database/storage"
)

// The archive loop runs the server-side jobs of archive sorting
// (ARCHIVE_SORTING.md): the live sorter, which files what users archive; the
// Trash purge, which destroys what they throw away; and, with delete_archives,
// the archiving of mail a client left marked \Deleted in INBOX. The rest of
// delete_archives, what EXPUNGE does, lives in imapsess. It lives in this daemon
// because this daemon owns IMAP message state; the classifier that decides
// categories runs elsewhere and can only write a label.

var (
	metricArchiveFiled = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "imap_database_archive_filed_total",
		Help: "Messages the live sorter filed out of an \\Archive folder into a category folder.",
	})
	metricTrashPurged = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "imap_database_trash_purged_total",
		Help: "Messages the Trash purge destroyed, including other copies of their content.",
	})
	metricTrashPurgedBytes = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "imap_database_trash_purged_bytes_total",
		Help: "Raw message bytes the Trash purge destroyed.",
	})
	metricDeletedArchived = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "imap_database_deleted_archived_total",
		Help: "Messages left marked \\Deleted in INBOX that were archived instead (delete_archives).",
	})
	metricArchivePassErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "imap_database_archive_pass_errors_total",
		Help: "Archive sorter and Trash purge passes that ended in an error, by job.",
	}, []string{"job"})
	metricArchiveLastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "imap_database_archive_last_success_timestamp_seconds",
		Help: "Unix time of the last archive sorter or Trash purge pass that completed without error, by job.",
	}, []string{"job"})
)

func init() {
	metricsRegistry.MustRegister(metricArchiveFiled, metricTrashPurged, metricTrashPurgedBytes,
		metricDeletedArchived, metricArchivePassErrors, metricArchiveLastSuccess)
}

// startArchiveLoop runs the enabled archive jobs every archive.interval until
// ctx ends, and returns a channel closed once the loop has stopped, so the
// caller can wait for an in-flight pass before closing the pool. With neither
// job enabled it starts nothing and returns a closed channel.
func startArchiveLoop(ctx context.Context, pool *pgxpool.Pool, cfg *Config, logger *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	a := cfg.Archive
	if !a.SortEnabled && !a.PurgeEnabled && !a.DeleteArchives {
		close(done)
		return done
	}
	interval, settle, retention := a.Durations()
	if a.PurgeEnabled && retention < time.Hour {
		logger.Warn("archive.trash_retention is under an hour: a mistaken delete can barely be recovered",
			"trash_retention", retention)
	}
	db := storage.NewFromPool(pool, storage.Config{
		StatementTimeout: mustParseDuration(cfg.Postgres.StatementTimeout, 10*time.Second),
	})
	logger.Info("archive jobs enabled",
		"sort", a.SortEnabled, "purge", a.PurgeEnabled, "delete_archives", a.DeleteArchives, "interval", interval,
		"settle_delay", settle, "min_confidence", a.MinConfidence,
		"trash_retention", retention, "purge_all_copies", a.PurgeAllCopies)

	deleted := archive.NewDeletedTracker()
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			runArchivePass(ctx, db, a, deleted, settle, retention, logger)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}

// runArchivePass runs one pass of each enabled job. Errors are logged and
// counted; the next tick tries again.
func runArchivePass(ctx context.Context, db *storage.DB, a ArchiveConfig, deleted *archive.DeletedTracker, settle, retention time.Duration, logger *slog.Logger) {
	if a.DeleteArchives && ctx.Err() == nil {
		n, err := deleted.ArchiveOnce(ctx, db, archive.DeletedOptions{
			SettleDelay: settle,
			BatchSize:   a.BatchSize,
			Logger:      logger,
		})
		metricDeletedArchived.Add(float64(n))
		switch {
		case err != nil && ctx.Err() == nil:
			metricArchivePassErrors.WithLabelValues("delete_archives").Inc()
			logger.Error("archive deleted inbox mail", "err", err, "archived", n)
		case err == nil:
			metricArchiveLastSuccess.WithLabelValues("delete_archives").SetToCurrentTime()
		}
	}
	if a.SortEnabled && ctx.Err() == nil {
		stats, err := archive.SortOnce(ctx, db, archive.SortOptions{
			SettleDelay:   settle,
			MinConfidence: a.MinConfidence,
			BatchSize:     a.BatchSize,
			Logger:        logger,
		})
		metricArchiveFiled.Add(float64(stats.Filed))
		switch {
		case err != nil && ctx.Err() == nil:
			metricArchivePassErrors.WithLabelValues("sort").Inc()
			logger.Error("archive sorter pass", "err", err, "filed", stats.Filed)
		case err == nil:
			metricArchiveLastSuccess.WithLabelValues("sort").SetToCurrentTime()
		}
	}
	if a.PurgeEnabled && ctx.Err() == nil {
		stats, err := archive.PurgeOnce(ctx, db, archive.PurgeOptions{
			Retention: retention,
			AllCopies: a.PurgeAllCopies,
			BatchSize: a.BatchSize,
			Logger:    logger,
		})
		metricTrashPurged.Add(float64(stats.Messages))
		metricTrashPurgedBytes.Add(float64(stats.Bytes))
		switch {
		case err != nil && ctx.Err() == nil:
			metricArchivePassErrors.WithLabelValues("purge").Inc()
			logger.Error("trash purge pass", "err", err, "destroyed", stats.Messages)
		case err == nil:
			metricArchiveLastSuccess.WithLabelValues("purge").SetToCurrentTime()
		}
	}
}
