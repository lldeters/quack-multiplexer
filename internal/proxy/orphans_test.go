package proxy_test

import (
	"context"
	"slices"
	"testing"

	"quackmux/internal/engine/fake"
	"quackmux/internal/wire"
)

// restartedHarness starts a gateway with yamlExtra on the engines of h1, as
// a restarted gateway process finds them, and stops h1.
func restartedHarness(t *testing.T, h1 *harness, yamlExtra string) *harness {
	t.Helper()
	prov := fake.New()
	prov.Adopt(h1.prov)
	h1.cancel()
	return newHarness(t, yamlExtra, prov)
}

func listings(h *harness, engineID string) int {
	n := 0
	for _, r := range h.prov.Server(engineID).Received() {
		if r.Type == wire.TypePrepareRequest && r.SQL == wire.ActiveConnectionsSQL {
			n++
		}
	}
	return n
}

// A client that died while no gateway ran leaves its connection on a shared
// engine. After the restart, the sweep closes it; the client that is alive
// keeps its session, and its connection, on the same engine.
func TestRestartClosesOrphanedConnections(t *testing.T) {
	h1 := newHarness(t, "", nil)
	live := h1.connect("tok?user=alice", nil)
	dead := h1.connect("tok?user=alice", nil)
	eid := h1.sessionEngine(live)
	if h1.sessionEngine(dead) != eid {
		t.Fatal("the sessions of alice landed on different engines")
	}
	liveConn, deadConn := h1.backendConn(live), h1.backendConn(dead)

	h2 := restartedHarness(t, h1, "")
	ctx := context.Background()
	sweep := h2.sessions.BeginOrphanSweep(ctx, h2.engines.RecoveredTargets())
	mustType(t, h2.heartbeat(live), wire.TypeSuccessResponse) // adopted
	if n := sweep.Finish(ctx); n != 1 {
		t.Fatalf("the sweep closed %d connections, want 1", n)
	}
	conns := h2.prov.Server(eid).Connections()
	if !slices.Contains(conns, liveConn) || slices.Contains(conns, deadConn) {
		t.Fatalf("the engine holds %v; want %s and not %s", conns, liveConn, deadConn)
	}
	mustType(t, h2.query(live, "SELECT 1"), wire.TypePrepareResponse)
	if e := mustError(t, h2.heartbeat(dead), "closed the engine connection as orphaned"); e.MustInvalidate {
		t.Fatal("the late client must only reconnect, its database is fine")
	}
	if n := h2.sessions.Count(); n != 1 {
		t.Fatalf("%d sessions after the sweep, want the live one", n)
	}
}

// SweepOrphans closes the orphans once the grace has passed, on its own.
func TestSweepOrphansAfterGrace(t *testing.T) {
	h1 := newHarness(t, "", nil)
	live := h1.connect("tok?user=alice", nil)
	dead := h1.connect("tok?user=alice", nil)
	eid := h1.sessionEngine(live)
	deadConn := h1.backendConn(dead)

	// The grace is the longest of engine.recoveryGrace, 0.4 *
	// session.maxHeartbeatTimeout + session.heartbeatGrace and
	// server.upstream.handshakeTimeout: one second here.
	h2 := restartedHarness(t, h1, `
server: {upstream: {handshakeTimeout: 1s}}
session: {minHeartbeatTimeout: 1s, maxHeartbeatTimeout: 1s, heartbeatGrace: 100ms, checkInterval: 50ms}
engine: {recoveryGrace: 1s}
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h2.sessions.SweepOrphans(ctx, h2.engines.RecoveredTargets())
	mustType(t, h2.heartbeat(live), wire.TypeSuccessResponse)
	srv := h2.prov.Server(eid)
	eventually(t, "the sweep to close the orphaned connection", func() bool {
		return !slices.Contains(srv.Connections(), deadConn)
	})
	if n := listings(h2, eid); n != 2 {
		t.Fatalf("%d listings of the engine's connections, want 2", n)
	}
	mustType(t, h2.query(live, "SELECT 1"), wire.TypePrepareResponse)
}

// engine.closeOrphanedConnections: false keeps the gateway from even
// listing the connections.
func TestSweepOrphansSwitchedOff(t *testing.T) {
	h1 := newHarness(t, "", nil)
	c := h1.connect("tok?user=alice", nil)
	eid := h1.sessionEngine(c)
	h2 := restartedHarness(t, h1, "engine: {closeOrphanedConnections: false}\n")
	h2.sessions.SweepOrphans(context.Background(), h2.engines.RecoveredTargets())
	if n := listings(h2, eid); n != 0 {
		t.Fatalf("%d listings with the sweep switched off", n)
	}
}
