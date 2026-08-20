// Package observability wires Ratiba's metrics and tracing.
//
// The guiding constraint throughout is label cardinality. Every metric label in
// this package comes from a small, fixed vocabulary — HTTP method, matched
// route template, status class, domain operation, domain outcome. No label ever
// carries a UUID, a date, a patient name or a raw URL path. An unbounded label
// turns a metrics endpoint into a memory leak and makes the resulting series
// useless for alerting.
package observability

import (
	"runtime"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/JerryLegend254/ratiba/internal/platform/config"
)

// Metrics holds the application's collectors and the registry they live in.
type Metrics struct {
	registry *prometheus.Registry

	requestsTotal    *prometheus.CounterVec
	requestDuration  *prometheus.HistogramVec
	requestsInFlight prometheus.Gauge
	responseSize     *prometheus.HistogramVec
	panicsTotal      prometheus.Counter

	appointmentOps *prometheus.CounterVec
}

// NewMetrics builds a private registry and registers every collector.
//
// A private registry is used rather than the default global one so that tests
// can build an isolated instance, and so that no library can silently add
// series to Ratiba's endpoint.
func NewMetrics(cfg config.Config) *Metrics {
	registry := prometheus.NewRegistry()

	m := &Metrics{
		registry: registry,

		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total HTTP requests by method, matched route template and status class.",
		}, []string{"method", "route", "status_class"}),

		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "http_request_duration_seconds",
			Help: "HTTP request latency in seconds.",
			// Buckets are chosen around this API's expected behaviour: most
			// requests are a single indexed query and should land under 50ms,
			// with enough headroom above to see a pool-exhaustion stall.
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"method", "route", "status_class"}),

		requestsInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "http_requests_in_flight",
			Help: "HTTP requests currently being served.",
		}),

		responseSize: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_response_size_bytes",
			Help:    "HTTP response body size in bytes.",
			Buckets: prometheus.ExponentialBuckets(64, 4, 7),
		}, []string{"method", "route"}),

		panicsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "http_handler_panics_total",
			Help: "Panics recovered by the HTTP recovery middleware.",
		}),

		appointmentOps: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "appointment_operations_total",
			Help: "Appointment operations by outcome. The (book, conflict) series is the double-booking signal.",
		}, []string{"operation", "outcome"}),
	}

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ratiba_build_info",
		Help: "Build metadata of the running binary. Always 1; the information is in the labels.",
	}, []string{"version", "commit", "build_time", "go_version", "env"})
	buildInfo.WithLabelValues(
		cfg.Build.Version, cfg.Build.Commit, cfg.Build.BuildTime,
		runtime.Version(), string(cfg.Env),
	).Set(1)

	registry.MustRegister(
		m.requestsTotal,
		m.requestDuration,
		m.requestsInFlight,
		m.responseSize,
		m.panicsTotal,
		m.appointmentOps,
		buildInfo,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	return m
}

// Registry exposes the registry for the /metrics handler.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// RegisterPoolCollector publishes connection pool statistics.
//
// Pool saturation is the most common cause of a slow API that looks healthy
// from the outside, so these gauges are the first thing the runbook asks for.
func (m *Metrics) RegisterPoolCollector(pool *pgxpool.Pool) error {
	return m.registry.Register(&poolCollector{pool: pool})
}

// ObserveRequest records one completed HTTP request.
//
// route must be the matched route template, never the raw path.
func (m *Metrics) ObserveRequest(method, route string, status int, duration time.Duration, responseBytes int64) {
	class := statusClass(status)
	m.requestsTotal.WithLabelValues(method, route, class).Inc()
	m.requestDuration.WithLabelValues(method, route, class).Observe(duration.Seconds())
	m.responseSize.WithLabelValues(method, route).Observe(float64(responseBytes))
}

// IncInFlight marks a request as started.
func (m *Metrics) IncInFlight() { m.requestsInFlight.Inc() }

// DecInFlight marks a request as finished.
func (m *Metrics) DecInFlight() { m.requestsInFlight.Dec() }

// RecordPanic counts a recovered panic.
func (m *Metrics) RecordPanic() { m.panicsTotal.Inc() }

// RecordOutcome implements appointment.Metrics.
func (m *Metrics) RecordOutcome(operation, outcome string) {
	m.appointmentOps.WithLabelValues(operation, outcome).Inc()
}

// statusClass collapses a status code to its class ("2xx", "4xx"), keeping the
// label set to five values instead of dozens.
func statusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	case status >= 200:
		return "2xx"
	case status >= 100:
		return "1xx"
	default:
		return "unknown"
	}
}

// poolCollector reports pgxpool statistics on each scrape.
//
// Sampling at scrape time rather than caching means the numbers are never
// stale, which matters when diagnosing a live saturation incident.
type poolCollector struct {
	pool *pgxpool.Pool
}

var (
	poolTotalConns = prometheus.NewDesc(
		"db_pool_total_connections",
		"Connections currently in the pool, idle plus in use.", nil, nil)
	poolAcquiredConns = prometheus.NewDesc(
		"db_pool_acquired_connections",
		"Connections currently checked out.", nil, nil)
	poolIdleConns = prometheus.NewDesc(
		"db_pool_idle_connections",
		"Connections currently idle.", nil, nil)
	poolMaxConns = prometheus.NewDesc(
		"db_pool_max_connections",
		"Configured maximum pool size.", nil, nil)
	poolAcquireCount = prometheus.NewDesc(
		"db_pool_acquire_total",
		"Successful connection acquisitions since start.", nil, nil)
	poolAcquireDuration = prometheus.NewDesc(
		"db_pool_acquire_duration_seconds_total",
		"Cumulative time spent waiting to acquire a connection. A rising rate means saturation.", nil, nil)
	poolCanceledAcquire = prometheus.NewDesc(
		"db_pool_canceled_acquire_total",
		"Acquisitions abandoned because the caller's context ended first.", nil, nil)
	poolEmptyAcquire = prometheus.NewDesc(
		"db_pool_empty_acquire_total",
		"Acquisitions that had to wait for a connection to be returned.", nil, nil)
)

// Describe implements prometheus.Collector.
func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- poolTotalConns
	ch <- poolAcquiredConns
	ch <- poolIdleConns
	ch <- poolMaxConns
	ch <- poolAcquireCount
	ch <- poolAcquireDuration
	ch <- poolCanceledAcquire
	ch <- poolEmptyAcquire
}

// Collect implements prometheus.Collector.
func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	stat := c.pool.Stat()
	ch <- prometheus.MustNewConstMetric(poolTotalConns, prometheus.GaugeValue, float64(stat.TotalConns()))
	ch <- prometheus.MustNewConstMetric(poolAcquiredConns, prometheus.GaugeValue, float64(stat.AcquiredConns()))
	ch <- prometheus.MustNewConstMetric(poolIdleConns, prometheus.GaugeValue, float64(stat.IdleConns()))
	ch <- prometheus.MustNewConstMetric(poolMaxConns, prometheus.GaugeValue, float64(stat.MaxConns()))
	ch <- prometheus.MustNewConstMetric(poolAcquireCount, prometheus.CounterValue, float64(stat.AcquireCount()))
	ch <- prometheus.MustNewConstMetric(poolAcquireDuration, prometheus.CounterValue, stat.AcquireDuration().Seconds())
	ch <- prometheus.MustNewConstMetric(poolCanceledAcquire, prometheus.CounterValue, float64(stat.CanceledAcquireCount()))
	ch <- prometheus.MustNewConstMetric(poolEmptyAcquire, prometheus.CounterValue, float64(stat.EmptyAcquireCount()))
}
