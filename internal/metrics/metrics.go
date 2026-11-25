// Package metrics defines the gateway's Prometheus metrics in one place.
package metrics

import (
	"sync"

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

	// LeaderLabel and LeaderLabelUpdates follow the label quackmux.io/leader
	// on the gateway's own pod. Only leader election (ha.enabled) uses them:
	// they are registered by RegisterLeaderLabel.
	LeaderLabel        prometheus.Gauge
	LeaderLabelUpdates *prometheus.CounterVec
	leaderLabelOnce    sync.Once

	// OrphanSweeps and OrphansClosed follow the sweep of the connections
	// that no session adopted after a takeover (package session, orphans.go).
	OrphanSweeps  *prometheus.CounterVec
	OrphansClosed prometheus.Counter
}

// New creates all collectors and registers them on a fresh registry (those
// of RegisterLeaderLabel only when that is called).
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
		LeaderLabel: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "quackmux_leader_label", Help: "1 if this replica's pod carries the label quackmux.io/leader (as confirmed by the API server).",
		}),
		LeaderLabelUpdates: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "quackmux_leader_label_updates_total", Help: "Patches of the label quackmux.io/leader, by action (set, remove, remove_stale) and result.",
		}, []string{"action", "result"}),
		OrphanSweeps: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "quackmux_orphan_sweeps_total",
			Help: "Recovered engines checked for connections that no session adopted, by result (ok; failed: a listing or a probe failed; skipped: the engine was gone, its authorization function refused the listing or the leader lease was not fresh).",
		}, []string{"result"}),
		OrphansClosed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "quackmux_orphaned_connections_closed_total",
			Help: "Connections on recovered engines that no session adopted and that the engine removed at the gateway's probe, their lease having run out.",
		}),
	}
	r.MustRegister(m.OrphanSweeps, m.OrphansClosed)
	r.MustRegister(m.SessionsOpen, m.SessionsOpened, m.SessionsClosed, m.ConnectFailures, m.AuthFailures,
		m.Engines, m.EngineStarts, m.EngineStartTime, m.EngineTerminated, m.Requests, m.RequestSeconds,
		m.Bytes, m.InFlight, m.IsLeader)
	return m
}

// RegisterLeaderLabel registers LeaderLabel and LeaderLabelUpdates, so that
// gateways without leader election do not export them. It may be called
// more than once.
func (m *Metrics) RegisterLeaderLabel() {
	m.leaderLabelOnce.Do(func() { m.Registry.MustRegister(m.LeaderLabel, m.LeaderLabelUpdates) })
}
