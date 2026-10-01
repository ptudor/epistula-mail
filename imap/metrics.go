package main

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ptudor/epistula-mail/imap/imapsess"
)

var metricsRegistry = prometheus.NewRegistry()

func init() {
	metricsRegistry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	// Application metrics: auth outcomes, limiter rejections, APPENDs.
	metricsRegistry.MustRegister(imapsess.MetricsCollectors()...)
}

// registerBackendMetrics adds the per-backend collectors (active session
// gauge) once the backend exists; called from runServe.
func registerBackendMetrics(backend *imapsess.Backend) {
	metricsRegistry.MustRegister(backend.SessionGauge())
}

func metricsHandler() http.Handler {
	return promhttp.HandlerFor(metricsRegistry, promhttp.HandlerOpts{Registry: metricsRegistry})
}
