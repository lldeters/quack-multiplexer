package config

import "time"

// CloseOrphanedConnectionsEnabled reports whether the gateway closes, after
// a (re)start or a leader takeover, the connections on recovered engines that
// no session adopted (engine.closeOrphanedConnections): those of clients that
// died while no gateway ran, which the Quack server never collects. The
// default is true, except for the static provider: its engines are not the
// gateway's own, other gateways or clients may use them as well.
func (c *Config) CloseOrphanedConnectionsEnabled() bool {
	if v := c.Engine.CloseOrphanedConnections; v != nil {
		return *v
	}
	return c.Provider.Type != "static"
}

// OrphanGrace is how long after a takeover the gateway waits before it
// probes the connections that a recovered engine held at the takeover and
// that no session adopted: session.backendHeartbeatTimeout plus
// session.heartbeatGrace. The gateway asks for that lease at every handshake
// with an engine and renews it only while a session is open, so by then the
// lease of every connection that only the previous process kept alive has
// run out, and the engine removes it at the probe. A connection whose lease
// still runs, the engine renews instead.
func (c *Config) OrphanGrace() time.Duration {
	return c.Session.BackendHeartbeatTimeout.Duration + c.Session.HeartbeatGrace.Duration
}
