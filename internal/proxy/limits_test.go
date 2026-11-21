package proxy_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"quackmux/internal/engine"
	"quackmux/internal/engine/fake"
	"quackmux/internal/wire"
)

// limits.maxEnginesPerUser counts every engine a user caused, whatever its
// share level: otherwise one user could start SERVER engines for arbitrary
// subdomains until limits.maxEngines locks everybody else out.
func TestEngineLimitPerUserCoversSharedEngines(t *testing.T) {
	h := newHarness(t, "limits: {maxEnginesPerUser: 2, maxEngines: 4}\n", nil)
	h.connect("x?user=mallory", nil)
	h.connect("x?user=mallory&share_level=server&subdomain=x0", nil)
	for i := 1; i < 4; i++ {
		e := h.connectErr(fmt.Sprintf("x?user=mallory&share_level=server&subdomain=x%d", i), nil)
		if !strings.Contains(e.Message, "maxEnginesPerUser") {
			t.Fatalf("engine %d of mallory: %q", i+2, e.Message)
		}
	}
	h.connect("x?user=bob", nil) // room is left for others
	// Using an engine somebody else started does not count.
	h.connect("x?user=bob&share_level=server&subdomain=x0", nil)
}

// A warm engine claim counts against limits.maxEnginesPerUser as well.
func TestWarmClaimRespectsEngineLimitPerUser(t *testing.T) {
	h := newHarness(t, `
limits:
  maxEnginesPerUser: 1
profiles:
  default:
    warmPool: {size: 2}
`, nil)
	warmReady := func() int {
		n := 0
		for _, v := range h.engines.List() {
			if v.Warm && v.State == engine.StateReady {
				n++
			}
		}
		return n
	}
	h.engines.Reconcile()
	eventually(t, "warm pool filled", func() bool { return warmReady() == 2 })
	h.connect("tok?user=alice&share_level=connection", nil)
	if e := h.connectErr("tok?user=alice&share_level=connection", nil); !strings.Contains(e.Message, "maxEnginesPerUser") {
		t.Fatalf("second CONNECTION engine of alice: %q", e.Message)
	}
}

// A warm engine that reached engine.maxLifetime is replaced, not handed to
// a new session that would then be drained at once.
func TestWarmEngineBeyondMaxLifetimeIsRecycled(t *testing.T) {
	h := newHarness(t, `
engine:
  maxLifetime: 1h
  drainTimeout: 30m
profiles:
  default:
    warmPool: {size: 1}
`, nil)
	h.engines.Reconcile()
	warmReady := func() string {
		for _, v := range h.engines.List() {
			if v.Warm && v.State == engine.StateReady {
				return v.ID
			}
		}
		return ""
	}
	eventually(t, "warm engine ready", func() bool { return warmReady() != "" })
	first := warmReady()
	for i := 0; i < 24; i++ { // two idle hours, the pool maintained all along
		h.clock.Advance(5 * time.Minute)
		h.engines.Reconcile()
		eventually(t, "warm engine ready", func() bool { return warmReady() != "" })
	}
	if warmReady() == first {
		t.Fatal("the warm engine was not recycled after its max lifetime")
	}
	c := h.connect("tok?user=alice&share_level=connection", nil)
	h.engines.Reconcile()
	if v, _ := h.engines.Get(h.sessionEngine(c)); v.State != engine.StateReady {
		t.Fatalf("the claimed engine is %s (%s) right after the connect", v.State, v.Message)
	}
	for i := 0; i < 7; i++ { // 35 minutes of work
		h.clock.Advance(5 * time.Minute)
		mustType(t, h.heartbeat(c), wire.TypeSuccessResponse)
		h.engines.Reconcile()
		h.sessions.Wait()
	}
	mustType(t, h.query(c, "SELECT 1"), wire.TypePrepareResponse)
}

// A claimed warm engine still counts for its user after a gateway restart:
// the claim records the user like a launch does.
func TestClaimedWarmEngineCountsForItsUserAfterRestart(t *testing.T) {
	const cfg = `
limits:
  maxEnginesPerUser: 1
profiles:
  default:
    warmPool: {size: 1}
`
	h1 := newHarness(t, cfg, nil)
	h1.engines.Reconcile()
	var warm string
	eventually(t, "warm engine ready", func() bool {
		for _, v := range h1.engines.List() {
			if v.Warm && v.State == engine.StateReady {
				warm = v.ID
				return true
			}
		}
		return false
	})
	c := h1.connect("tok?user=alice&share_level=connection", nil)
	if got := h1.sessionEngine(c); got != warm {
		t.Fatalf("the session got engine %s, not the warm engine %s", got, warm)
	}

	prov2 := fake.New()
	prov2.Adopt(h1.prov)
	h1.cancel()
	h2 := newHarness(t, cfg, prov2)
	if v, ok := h2.engines.Get(warm); !ok || v.Warm || v.User != "alice" {
		t.Fatalf("recovered claimed engine: %+v (found %v)", v, ok)
	}
	if e := h2.connectErr("tok?user=alice&share_level=connection", nil); !strings.Contains(e.Message, "maxEnginesPerUser") {
		t.Fatalf("second CONNECTION engine of alice after the restart: %q", e.Message)
	}
}
