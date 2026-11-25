package proxy_test

import (
	"testing"
	"time"

	"quackmux/internal/proxy"
	"quackmux/internal/wire"
)

// The query time limit with sessions at the bound of followed queries.

// PREPAREs that the engine answered while the others were still on their
// way may each still run. At the bound the gateway answers a further one
// itself (HTTP 200, ERROR_RESPONSE) without forwarding it. A CANCEL of the
// client ends the queries, and the session goes on.
func TestFullSessionRefusesFurtherQueries(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 1m}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	srv.PrepareDelay = 2 * time.Second // all of them reach the engine before the first answer
	answers := make(chan []byte, proxy.MaxSessionQueries)
	for i := 0; i < proxy.MaxSessionQueries; i++ {
		go func() { answers <- h.prepare(c, "SELECT 1", wire.Hugeint{Upper: 60, Lower: uint64(i + 1)}) }()
	}
	for i := 0; i < proxy.MaxSessionQueries; i++ {
		mustType(t, wait(t, "a PREPARE", answers), wire.TypePrepareResponse)
	}
	if _, q := proxy.TrackedQueries(h.gw); q != proxy.MaxSessionQueries {
		t.Fatalf("%d queries followed, want %d", q, proxy.MaxSessionQueries)
	}
	srv.PrepareDelay = 0
	mustError(t, h.prepare(c, "SELECT 2", uuidA), "it did not forward this request. Cancel those queries or reconnect.")
	if ran(srv, "SELECT 2") {
		t.Fatal("the gateway forwarded the PREPARE beyond the bound")
	}
	if n := requestCount(t, h.met, "too_many_queries"); n != 1 {
		t.Fatalf("%v requests counted as too_many_queries, want 1", n)
	}
	mustType(t, h.cancelQuery(c, wire.Hugeint{}), wire.TypeSuccessResponse)
	mustType(t, h.prepare(c, "SELECT 3", uuidB), wire.TypePrepareResponse)
	if _, q := proxy.TrackedQueries(h.gw); q != 1 {
		t.Fatalf("%d queries followed after the CANCEL, want the last one", q)
	}
}
