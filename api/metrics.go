package main

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var metricsRegistry = prometheus.NewRegistry()

var (
	metricRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mail_api_requests_total",
		Help: "API requests by route and HTTP status code.",
	}, []string{"route", "code"})

	metricRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mail_api_request_duration_seconds",
		Help:    "API request latency by route.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route"})

	metricAuthFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mail_api_auth_failures_total",
		Help: "Rejected bearer-token authentications by reason.",
	}, []string{"reason"})

	metricExportStreams = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "mail_api_export_streams_active",
		Help: "NDJSON export streams currently running.",
	})

	metricAnnotationsWritten = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "mail_api_annotations_written_total",
		Help: "Annotation upserts committed.",
	})

	metricClassificationsWritten = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "mail_api_classifications_written_total",
		Help: "Archive classification upserts committed.",
	})

	metricPassMarkersPruned = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "mail_api_pass_markers_pruned_total",
		Help: "Annotation pass markers cleared for finished messages.",
	})
)

func init() {
	metricsRegistry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		metricRequestsTotal,
		metricRequestDuration,
		metricAuthFailures,
		metricExportStreams,
		metricAnnotationsWritten,
		metricClassificationsWritten,
		metricPassMarkersPruned,
	)
}

func metricsHandler() http.Handler {
	return promhttp.HandlerFor(metricsRegistry, promhttp.HandlerOpts{Registry: metricsRegistry})
}
