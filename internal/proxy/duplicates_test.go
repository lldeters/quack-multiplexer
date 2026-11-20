package proxy_test

import (
	"strings"
	"testing"
	"time"

	"quackmux/internal/engine"
	"quackmux/internal/engine/fake"
)

// An engine that missed its startup timeout but became ready later is READY
// again after a gateway restart, next to the engine that replaced it. New
// sessions of the key must all reach one engine (not spread over two with
// different database state), and the duplicate is drained.
func TestRestartWithDuplicateEnginesConverges(t *testing.T) {
	const cfg = "engine: {startupTimeout: 1s, failedRetention: 10m}\n"
	prov := fake.New()
	prov.StartDelay = 1500 * time.Millisecond
	h1 := newHarness(t, cfg, prov)
	if e := h1.connectErr("tok?user=alice", nil); !strings.Contains(e.Message, "did not become ready") {
		t.Fatalf("got %q", e.Message)
	}
	h1.clock.Advance(2 * time.Second)
	h1.engines.Reconcile() // STARTING beyond the startup timeout: FAILED
	var slow string
	for _, v := range h1.engines.List() {
		slow = v.ID
	}
	time.Sleep(time.Second) // the slow engine becomes ready after all
	prov.SetStartDelay(0)
	replacement := h1.sessionEngine(h1.connect("tok?user=alice", nil))

	prov2 := fake.New()
	prov2.Adopt(h1.prov)
	h1.cancel()
	h2 := newHarness(t, cfg, prov2)
	seen := map[string]int{}
	for i := 0; i < 20; i++ {
		seen[h2.sessionEngine(h2.connect("tok?user=alice", nil))]++
	}
	if len(seen) != 1 || seen[slow] != 20 {
		t.Fatalf("new sessions landed on %v, want all on the older engine %s", seen, slow)
	}
	h2.engines.Reconcile()
	if v, _ := h2.engines.Get(replacement); v.State != engine.StateDraining {
		t.Fatalf("duplicate engine %s is %s, want DRAINING", replacement, v.State)
	}
	if v, _ := h2.engines.Get(slow); v.State != engine.StateReady {
		t.Fatalf("serving engine %s is %s", slow, v.State)
	}
}
