// Package metrics defines the Prometheus counters, gauges, and
// histograms OpenWatch emits and exposes them through a package-owned
// registry. Using a dedicated registry (rather than the default global
// one) keeps the daemon's metrics separate from anything third-party
// dependencies might scatter into prometheus.DefaultRegisterer — the
// /metrics endpoint only ever shows what OpenWatch itself cares about.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Update status labels. Defined as constants so recording call sites
// don't pass arbitrary strings and so the cardinality of the status
// label is bounded at compile time.
const (
	StatusSuccess    = "success"
	StatusFailed     = "failed"
	StatusSkipped    = "skipped"
	StatusNotifyOnly = "notify_only"
)

// Rollback status labels. Same reasoning as the update status constants.
const (
	RollbackSuccess = "success"
	RollbackFailed  = "failed"
)

// Metrics bundles every counter/gauge/histogram the daemon emits
// together with the registry that owns them. The watcher holds one
// instance for the lifetime of the process; the HTTP API exposes the
// same instance via its Handler.
type Metrics struct {
	registry *prometheus.Registry

	UpdatesTotal        *prometheus.CounterVec
	RollbacksTotal      *prometheus.CounterVec
	ContainersMonitored prometheus.Gauge
	UpdateDuration      *prometheus.HistogramVec
}

// New allocates the metric set, registers it against a fresh
// prometheus.Registry, and returns the bundle. Registration failures
// are surfaced as an error so the caller (main.go) can fatal-exit
// cleanly if, say, a collision is introduced in the future.
func New() (*Metrics, error) {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		registry: reg,
		UpdatesTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "openwatch_updates_total",
				Help: "Count of update attempts, labelled by container name and outcome.",
			},
			[]string{"container", "status"},
		),
		RollbacksTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "openwatch_rollbacks_total",
				Help: "Count of rollback attempts, labelled by container name and outcome.",
			},
			[]string{"container", "status"},
		),
		ContainersMonitored: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "openwatch_containers_monitored",
				Help: "Number of containers inspected during the most recent tick.",
			},
		),
		UpdateDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "openwatch_update_duration_seconds",
				Help:    "Wall-clock duration of a successful update, measured from pull start to container start.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"container"},
		),
	}

	for _, c := range []prometheus.Collector{
		m.UpdatesTotal,
		m.RollbacksTotal,
		m.ContainersMonitored,
		m.UpdateDuration,
	} {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}

	return m, nil
}

// Handler returns an http.Handler that serves the metric registry in
// Prometheus exposition format. The handler is bound to the
// package-owned registry, not prometheus.DefaultGatherer, so it
// reflects only OpenWatch's own metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	})
}

// RecordUpdate increments the update counter for the given container
// and status. A no-op when the receiver is nil so call sites in the
// watcher can stay branch-free; main.go always supplies a real
// Metrics instance, so nil only matters in tests.
func (m *Metrics) RecordUpdate(container, status string) {
	if m == nil {
		return
	}
	m.UpdatesTotal.WithLabelValues(container, status).Inc()
}

// RecordRollback increments the rollback counter for the given
// container and status. Same nil-safety as RecordUpdate.
func (m *Metrics) RecordRollback(container, status string) {
	if m == nil {
		return
	}
	m.RollbacksTotal.WithLabelValues(container, status).Inc()
}

// SetContainersMonitored updates the gauge with the number of
// containers inspected in the most recent tick. Nil-safe for the same
// reason as the recorders above.
func (m *Metrics) SetContainersMonitored(n int) {
	if m == nil {
		return
	}
	m.ContainersMonitored.Set(float64(n))
}

// ObserveUpdateDuration records an entry in the update-duration
// histogram. Nil-safe.
func (m *Metrics) ObserveUpdateDuration(container string, seconds float64) {
	if m == nil {
		return
	}
	m.UpdateDuration.WithLabelValues(container).Observe(seconds)
}
