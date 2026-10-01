package main

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

var (
	metricRunsTotal         atomic.Int64
	metricRunsFailed        atomic.Int64
	metricMessagesScanned   atomic.Int64
	metricMessagesSkipped   atomic.Int64
	metricMessagesAnnotated atomic.Int64
	metricMessagesFailed    atomic.Int64
	// Archive classifications written (ARCHIVE_SORTING.md).
	metricMessagesClassified atomic.Int64
	// Model refusals for a prompt longer than its context window, each of
	// which is retried with less of the message body.
	metricContextOverflows atomic.Int64
	// Export streams cut off mid-way and reopened within the same pass.
	metricStreamReconnects atomic.Int64
	// Complete rounds run (every other round is a fast round).
	metricCompleteRounds atomic.Int64
	// Annotation pass queue markers cleared for finished messages, and the
	// markers still queued at the last prune.
	metricPassMarkersPruned  atomic.Int64
	metricPassQueueRemaining atomic.Int64
)

func metricsHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# TYPE mail_llm_worker_runs_total counter\nmail_llm_worker_runs_total %d\n", metricRunsTotal.Load())
	fmt.Fprintf(w, "# TYPE mail_llm_worker_runs_failed_total counter\nmail_llm_worker_runs_failed_total %d\n", metricRunsFailed.Load())
	fmt.Fprintf(w, "# TYPE mail_llm_worker_messages_scanned_total counter\nmail_llm_worker_messages_scanned_total %d\n", metricMessagesScanned.Load())
	fmt.Fprintf(w, "# TYPE mail_llm_worker_messages_skipped_total counter\nmail_llm_worker_messages_skipped_total %d\n", metricMessagesSkipped.Load())
	fmt.Fprintf(w, "# TYPE mail_llm_worker_messages_annotated_total counter\nmail_llm_worker_messages_annotated_total %d\n", metricMessagesAnnotated.Load())
	fmt.Fprintf(w, "# TYPE mail_llm_worker_messages_failed_total counter\nmail_llm_worker_messages_failed_total %d\n", metricMessagesFailed.Load())
	fmt.Fprintf(w, "# TYPE mail_llm_worker_messages_classified_total counter\nmail_llm_worker_messages_classified_total %d\n", metricMessagesClassified.Load())
	fmt.Fprintf(w, "# TYPE mail_llm_worker_context_overflows_total counter\nmail_llm_worker_context_overflows_total %d\n", metricContextOverflows.Load())
	fmt.Fprintf(w, "# TYPE mail_llm_worker_stream_reconnects_total counter\nmail_llm_worker_stream_reconnects_total %d\n", metricStreamReconnects.Load())
	fmt.Fprintf(w, "# TYPE mail_llm_worker_complete_rounds_total counter\nmail_llm_worker_complete_rounds_total %d\n", metricCompleteRounds.Load())
	fmt.Fprintf(w, "# TYPE mail_llm_worker_pass_markers_pruned_total counter\nmail_llm_worker_pass_markers_pruned_total %d\n", metricPassMarkersPruned.Load())
	fmt.Fprintf(w, "# TYPE mail_llm_worker_pass_queue_remaining gauge\nmail_llm_worker_pass_queue_remaining %d\n", metricPassQueueRemaining.Load())
}
