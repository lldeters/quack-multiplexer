package config

import (
	"testing"
	"time"
)

func TestCloseOrphanedConnectionsDefault(t *testing.T) {
	static := "provider: {type: static, static: {engines: [{name: s, url: 'http://e:9494', token: t0123456789abcdef, shareLevel: SERVER}]}}\n"
	for name, c := range map[string]struct {
		yaml string
		want bool
	}{
		"kubernetes":                      {"", true},
		"process":                         {"provider: {type: process}\n", true},
		"static":                          {static, false},
		"static, switched on":             {static + "engine: {closeOrphanedConnections: true}\n", true},
		"kubernetes, switched off":        {"engine: {closeOrphanedConnections: false}\n", false},
		"kubernetes, explicitly on":       {"engine: {closeOrphanedConnections: true}\n", true},
		"static, explicitly switched off": {static + "engine: {closeOrphanedConnections: false}\n", false},
	} {
		cfg, err := Parse([]byte("auth: {mode: none}\n" + c.yaml))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := cfg.CloseOrphanedConnectionsEnabled(); got != c.want {
			t.Errorf("%s: CloseOrphanedConnectionsEnabled() = %v, want %v", name, got, c.want)
		}
	}
}

// The grace covers the longest heartbeat interval of a client plus the
// heartbeat grace, and a handshake that was under way.
func TestOrphanGrace(t *testing.T) {
	for name, c := range map[string]struct {
		yaml string
		want time.Duration
	}{
		// recoveryGrace defaults to maxHeartbeatTimeout/2 + 30s.
		"defaults":                     {"", 180 * time.Second},
		"recoveryGrace at its minimum": {"engine: {recoveryGrace: 120s}\n", 135 * time.Second},
		"longer recoveryGrace":         {"engine: {recoveryGrace: 10m}\n", 10 * time.Minute},
		"short leases": {"session: {minHeartbeatTimeout: 1s, maxHeartbeatTimeout: 5s, heartbeatGrace: 1s}\nengine: {recoveryGrace: 2s}\n",
			30 * time.Second}, // server.upstream.handshakeTimeout
	} {
		cfg, err := Parse([]byte("auth: {mode: none}\n" + c.yaml))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := cfg.OrphanGrace(); got != c.want {
			t.Errorf("%s: OrphanGrace() = %s, want %s", name, got, c.want)
		}
	}
}
