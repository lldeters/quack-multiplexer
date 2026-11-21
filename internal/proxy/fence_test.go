package proxy_test

import (
	"sync/atomic"
	"testing"
	"time"

	"quackmux/internal/engine"
	"quackmux/internal/wire"
)

// A leader that resumes after a freeze finds the heartbeats of its sessions
// missing (the new leader received them) and every timeout long passed.
// While its lease is not fresh it must neither close the sessions (the
// DISCONNECT would end them on the engine, for the new leader as well) nor
// stop engines. Found in the kind HA test (test/e2e/kind/ha.sh, h3).
func TestStaleLeaderLeavesSessionsAndEnginesAlone(t *testing.T) {
	h := newHarness(t, "", nil)
	var fresh atomic.Bool
	fresh.Store(true)
	h.sessions.SetFence(fresh.Load)
	h.engines.SetFence(fresh.Load)
	c := h.connect("tok?user=alice&share_level=connection", nil)
	eid := h.sessionEngine(c)
	srv := h.prov.Server(eid)

	fresh.Store(false)
	h.clock.Advance(2 * time.Hour)
	h.sessions.Reap()
	h.engines.Reconcile()
	h.sessions.Wait()
	if n := srv.CountType(wire.TypeDisconnect); n != 0 {
		t.Fatalf("the stale leader sent %d DISCONNECT(s)", n)
	}
	if n := len(h.sessions.List("alice", "")); n != 1 {
		t.Fatalf("the stale leader closed the session (%d left)", n)
	}
	if v, ok := h.engines.Get(eid); !ok || v.State != engine.StateReady {
		t.Fatalf("the stale leader changed the engine: %+v (found %v)", v, ok)
	}

	// With a fresh lease the timeouts apply again.
	fresh.Store(true)
	h.sessions.Reap()
	eventually(t, "the DISCONNECT of the expired session", func() bool { return srv.CountType(wire.TypeDisconnect) == 1 })
}
