// Package metrics defines the gateway's Prometheus metrics in one place.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics holds all collectors. Every component gets the same instance;
// tests simply create their own with New.
type Metrics struct {
	Registry *prometheus.Registry

	SessionsOpen     *prometheus.GaugeVec
	SessionsOpened   *prometheus.CounterVec
	SessionsClosed   *prometheus.CounterVec
	ConnectFailures  *prometheus.CounterVec
	AuthFailures     *prometheus.CounterVec
	Engines          *prometheus.GaugeVec
	EngineStarts     *prometheus.CounterVec
	EngineStartTime  *prometheus.HistogramVec
	EngineTerminated *prometheus.CounterVec
	Requests         *prometheus.CounterVec
	RequestSeconds   *prometheus.HistogramVec
	Bytes            *prometheus.CounterVec
	InFlight         prometheus.Gauge
	IsLeader         prometheus.Gauge
}

// New creates and registers all collectors on a fresh registry.
func New() *Metrics {
	r := prometheus.NewRegistry()
	r.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	m := &Metrics{
		Registry: r,
		SessionsOpen: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "quackmux_sessions", Help: "Open sessions.",
		}, []string{"share_level", "profile"}),
		SessionsOpened: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "quackmux_sessions_opened_total", Help: "Sessions opened (adopted=true for sessions recovered after a restart).",
		}, []string{"share_level", "profile", "adopted"}),
		SessionsClosed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "quackmux_sessions_closed_total", Help: "Sessions closed, by reason.",
		}, []string{"reason"}),
		ConnectFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "quackmux_connect_failures_total", Help: "Failed connection handshakes, by reason.",
		}, []string{"reason"}),
		AuthFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "quackmux_auth_failures_total", Help: "Authentication failures.",
		}, []string{"method"}),
		Engines: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "quackmux_engines", Help: "Engines by state.",
		}, []string{"state", "share_level", "profile"}),
		EngineStarts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "quackmux_engine_starts_total", Help: "Engine launches by result.",
		}, []string{"profile", "result"}),
		EngineStartTime: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "quackmux_engine_start_seconds", Help: "Time from launch to ready.",
			Buckets: []float64{0.5, 1, 2, 3, 5, 8, 13, 21, 34, 55, 89, 144, 233},
		}, []string{"profile"}),
		EngineTerminated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "quackmux_engines_terminated_total", Help: "Engines terminated, by reason.",
		}, []string{"reason"}),
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "quackmux_requests_total", Help: "Quack requests by message type and outcome.",
		}, []string{"type", "outcome"}),
		RequestSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "quackmux_request_seconds", Help: "Quack request latency by message type.",
			Buckets: prometheus.ExponentialBuckets(0.001, 4, 10),
		}, []string{"type"}),
		Bytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "quackmux_bytes_total", Help: "Proxied body bytes by direction (in=client->engine, out=engine->client).",
		}, []string{"direction"}),
		InFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "quackmux_requests_in_flight", Help: "Requests currently being proxied.",
		}),
		IsLeader: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "quackmux_is_leader", Help: "1 if this replica serves traffic.",
		}),
	}
	r.MustRegister(m.SessionsOpen, m.SessionsOpened, m.SessionsClosed, m.ConnectFailures, m.AuthFailures,
		m.Engines, m.EngineStarts, m.EngineStartTime, m.EngineTerminated, m.Requests, m.RequestSeconds,
		m.Bytes, m.InFlight, m.IsLeader)
	return m
}
