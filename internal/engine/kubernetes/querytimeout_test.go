package kubernetes

import (
	"testing"
	"time"

	"quackmux/internal/config"
)

// The query time limit is the gateway's policy, not part of the pod:
// setting it on a profile must not replace the profile's running engines.
func TestSpecHashIgnoresQueryTimeout(t *testing.T) {
	e := newTestEnv(t, testConfig(t, ""))
	prof := e.p.cfg.Profiles["default"]
	before := e.p.SpecHash("default", prof)
	limit := config.D(30 * time.Minute)
	prof.QueryTimeout = &limit
	if after := e.p.SpecHash("default", prof); after != before {
		t.Fatal("profiles.<name>.queryTimeout changes the spec hash")
	}
}
