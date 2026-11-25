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

// received counts the messages of a type that an engine got for a
// connection.
func received(h *harness, engineID, conn string, t wire.MessageType) int {
	n := 0
	for _, r := range h.prov.Server(engineID).Received() {
		if r.Type == t && r.ConnectionID == conn {
			n++
		}
	}
	return n
}

// A client that died while no gateway ran leaves its connection on a shared
// engine. Once its engine lease ran out, the sweep's heartbeat makes the
// engine remove it. The client that is alive keeps its session, and a
// client that was cut off for longer than any heartbeat interval, but not
// for longer than its engine lease, comes back after the sweep and keeps
// its connection too. The sweep sends no DISCONNECT.
func TestRestartClosesOrphanedConnections(t *testing.T) {
	h1 := newHarness(t, "", nil)
	live := h1.connect("tok?user=alice", nil)
	dead := h1.connect("tok?user=alice", nil)
	cut := h1.connect("tok?user=alice", nil)
	eid := h1.sessionEngine(live)
	if h1.sessionEngine(dead) != eid || h1.sessionEngine(cut) != eid {
		t.Fatal("the sessions of alice landed on different engines")
	}
	liveConn, deadConn, cutConn := h1.backendConn(live), h1.backendConn(dead), h1.backendConn(cut)

	h2 := restartedHarness(t, h1, "")
	srv := h2.prov.Server(eid)
	ctx := context.Background()
	sweep := h2.sessions.BeginOrphanSweep(ctx, h2.engines.RecoveredTargets())
	mustType(t, h2.heartbeat(live), wire.TypeSuccessResponse) // adopted
	srv.Expire(deadConn)                                      // its engine lease ran out
	if n := sweep.Finish(ctx); n != 1 {
		t.Fatalf("the sweep closed %d connections, want 1", n)
	}
	conns := srv.Connections()
	if !slices.Contains(conns, liveConn) || !slices.Contains(conns, cutConn) || slices.Contains(conns, deadConn) {
		t.Fatalf("the engine holds %v; want %s and %s, not %s", conns, liveConn, cutConn, deadConn)
	}
	if received(h2, eid, deadConn, wire.TypeHeartbeatRequest) != 1 || received(h2, eid, cutConn, wire.TypeHeartbeatRequest) != 1 {
		t.Fatal("the sweep did not probe each connection without a session once")
	}
	if n := srv.CountType(wire.TypeDisconnect); n != 1 {
		t.Fatalf("%d DISCONNECTs: want only the one of the listing's control connection", n)
	}
	mustType(t, h2.query(live, "SELECT 1"), wire.TypePrepareResponse)
	mustType(t, h2.heartbeat(cut), wire.TypeSuccessResponse) // back after the sweep
	mustType(t, h2.query(cut, "SELECT 1"), wire.TypePrepareResponse)
	if e := mustError(t, h2.heartbeat(dead), "the engine no longer knows this connection"); e.MustInvalidate {
		t.Fatal("the late client must only reconnect, its database is fine")
	}
	if n := h2.sessions.Count(); n != 2 {
		t.Fatalf("%d sessions after the sweep, want the live one and the one that was cut off", n)
	}
	if n := listings(h2, eid); n != 1 {
		t.Fatalf("%d listings of the engine's connections, want 1", n)
	}
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
