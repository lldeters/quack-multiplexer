package config

import "time"

// CloseOrphanedConnectionsEnabled reports whether the gateway closes, after
// a (re)start or a leader takeover, the connections on recovered engines that
// no session adopted (engine.closeOrphanedConnections): those of clients that
// died while no gateway ran, which the Quack server never collects. The
// default is true, except for the static provider: its engines may serve
// other gateways or clients as well.
func (c *Config) CloseOrphanedConnectionsEnabled() bool {
	if v := c.Engine.CloseOrphanedConnections; v != nil {
		return *v
	}
	return c.Provider.Type != "static"
}

// OrphanGrace is how long a connection that existed on a recovered engine
// when the gateway took over must go without a session before the gateway
// closes it: engine.recoveryGrace, but at least the longest heartbeat
// interval of a client (DuckDB heartbeats every third of its lease, with up
// to 20% jitter) plus session.heartbeatGrace, and at least
// server.upstream.handshakeTimeout, so that a handshake that was under way
// when the gateway first listed the connections has ended.
func (c *Config) OrphanGrace() time.Duration {
	ss := c.Session
	d := c.Engine.RecoveryGrace.Duration
	d = max(d, ss.MaxHeartbeatTimeout.Duration*2/5+ss.HeartbeatGrace.Duration)
	return max(d, c.Server.Upstream.HandshakeTimeout.Duration)
}
