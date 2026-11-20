package proxy_test

import (
	"testing"
	"time"

	"quackmux/internal/engine/fake"
	"quackmux/internal/wire"
)

// The engine renews a connection's lease only when a message arrives, and the
// gateway answers client heartbeats itself. These tests run the fake engine's
// lease on the harness clock.

// A request that runs longer than the engine lease (1h) must not lose the
// session: the gateway keeps the lease alive while the request is in flight.
func TestLongRequestKeepsEngineLease(t *testing.T) {
	h := newHarness(t, "", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	srv.Now = h.clock.Now

	done := make(chan []byte, 1)
	go func() { done <- h.query(c, "SLEEP 3000") }()
	eventually(t, "query in flight", func() bool { return h.gw.InFlight() == 1 })
	time.Sleep(200 * time.Millisecond) // the engine has received the query

	// 61 minutes pass while the query runs; the client heartbeats every
	// minute (a DuckDB client does every 20s).
	for i := 0; i < 61; i++ {
		h.clock.Advance(time.Minute)
		mustType(t, h.heartbeat(c), wire.TypeSuccessResponse)
		h.sessions.Reap()
		h.sessions.Wait()
	}
	mustType(t, <-done, wire.TypePrepareResponse)
	if n := srv.CountType(wire.TypeHeartbeatRequest); n < 2 {
		t.Errorf("the gateway sent %d engine heartbeats during a 61-minute query, want at least 2", n)
	}
	mustType(t, h.query(c, "SELECT 1"), wire.TypePrepareResponse)
}

// The keep-alive schedule counts from when a request was sent (when the
// engine renewed the lease), not from when its answer arrived.
func TestKeepaliveCountsFromRequestStart(t *testing.T) {
	h := newHarness(t, "", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	srv.Now = h.clock.Now

	done := make(chan []byte, 1)
	go func() { done <- h.query(c, "SLEEP 3000") }()
	eventually(t, "query in flight", func() bool { return h.gw.InFlight() == 1 })
	time.Sleep(200 * time.Millisecond)
	for i := 0; i < 45; i++ {
		h.clock.Advance(time.Minute)
		mustType(t, h.heartbeat(c), wire.TypeSuccessResponse)
		h.sessions.Reap()
		h.sessions.Wait()
	}
	mustType(t, <-done, wire.TypePrepareResponse)
	// The user looks at the result for 25 minutes (client heartbeats only).
	for i := 0; i < 25; i++ {
		h.clock.Advance(time.Minute)
		mustType(t, h.heartbeat(c), wire.TypeSuccessResponse)
		h.sessions.Reap()
		h.sessions.Wait()
	}
	mustType(t, h.query(c, "SELECT 1"), wire.TypePrepareResponse)
}

// After a gateway restart, the engine lease of an adopted session is renewed
// at the next reap, even if the client only heartbeats.
func TestAdoptedSessionRenewsEngineLeaseAtOnce(t *testing.T) {
	h1 := newHarness(t, "", nil)
	c := h1.connect("tok?user=alice", nil)
	srv := h1.prov.Server(h1.sessionEngine(c))

	// A new gateway process: same key, engines handed over by the provider.
	prov2 := fake.New()
	prov2.Adopt(h1.prov)
	h1.cancel()
	h2 := newHarness(t, "", prov2)
	srv.Now = h2.clock.Now
	h2.clock.Advance(30 * time.Minute) // the gateway was away for a while
	mustType(t, h2.heartbeat(c), wire.TypeSuccessResponse)
	before := srv.CountType(wire.TypeHeartbeatRequest)
	h2.sessions.Reap()
	h2.sessions.Wait()
	if after := srv.CountType(wire.TypeHeartbeatRequest); after == before {
		t.Fatal("no engine heartbeat for an adopted session at the next reap")
	}
}
