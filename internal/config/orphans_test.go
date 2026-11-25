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

// The grace is the engine lease of the gateway's connections plus the
// heartbeat grace; the client's lease, the recovery grace and the handshake
// timeout do not shorten or lengthen it.
func TestOrphanGrace(t *testing.T) {
	for name, c := range map[string]struct {
		yaml string
		want time.Duration
	}{
		"defaults":            {"", time.Hour + 15*time.Second},
		"short engine lease":  {"session: {backendHeartbeatTimeout: 30s, heartbeatGrace: 5s}\n", 35 * time.Second},
		"long client leases":  {"session: {maxHeartbeatTimeout: 18h}\nengine: {recoveryGrace: 10h}\n", time.Hour + 15*time.Second},
		"long recovery grace": {"engine: {recoveryGrace: 2h}\nserver: {upstream: {handshakeTimeout: 90m}}\n", time.Hour + 15*time.Second},
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
