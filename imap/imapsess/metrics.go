package imapsess

import "github.com/prometheus/client_golang/prometheus"

// Application-level Prometheus collectors. The serve entrypoint registers
// MetricsCollectors() (plus the per-backend session gauge) on its registry;
// the increments below are cheap and always-on.
var (
	metricAuthTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "imap_database_auth_total",
		Help: "Login attempts by outcome (success | failure).",
	}, []string{"outcome"})

	metricLimiterRejections = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "imap_database_limiter_rejections_total",
		Help: "Connections or commands refused by a limiter (per_ip | per_mailbox | login_throttle | command_rate | auth_budget).",
	}, []string{"limiter"})

	metricAppendsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "imap_database_appends_total",
		Help: "Successful APPENDs.",
	})

	metricAppendBytes = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "imap_database_append_bytes_total",
		Help: "Bytes ingested via APPEND.",
	})
)

// MetricsCollectors returns the package's application collectors for
// registration on the daemon's Prometheus registry.
func MetricsCollectors() []prometheus.Collector {
	return []prometheus.Collector{
		metricAuthTotal,
		metricLimiterRejections,
		metricAppendsTotal,
		metricAppendBytes,
	}
}

// SessionGauge returns a gauge tracking this backend's open sessions.
func (b *Backend) SessionGauge() prometheus.Collector {
	return prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "imap_database_active_sessions",
		Help: "Currently open IMAP sessions.",
	}, func() float64 { return float64(b.ActiveSessions()) })
}
