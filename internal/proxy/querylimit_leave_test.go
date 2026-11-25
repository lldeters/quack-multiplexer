package proxy_test

import (
	"bytes"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"quackmux/internal/metrics"
	"quackmux/internal/proxy"
	"quackmux/internal/wire"
)

// The query time limit with clients that send a PREPARE whole and go away
// before the answer, with sessions at the bound of followed queries, and
// with sessions that end while the gateway cancels.

// postAndLeave sends msg as a chunked POST over a connection of its own and
// closes the connection right after the last chunk, without reading the
// answer: the gateway reads all of the message.
func (h *harness) postAndLeave(msg []byte) {
	h.t.Helper()
	addr := strings.TrimPrefix(h.srv.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		h.t.Fatal(err)
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "POST /quack HTTP/1.1\r\nHost: %s\r\nContent-Type: %s\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n",
		addr, wire.ContentType, len(msg))
	b.Write(msg)
	b.WriteString("\r\n0\r\n\r\n")
	if _, err := conn.Write(b.Bytes()); err != nil {
		h.t.Fatal(err)
	}
	_ = conn.Close()
}

// requestsOfType sums quackmux_requests_total for one message type.
func requestsOfType(t *testing.T, met *metrics.Metrics, typ wire.MessageType) float64 {
	t.Helper()
	mfs, err := met.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var n float64
	for _, mf := range mfs {
		if mf.GetName() != "quackmux_requests_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "type" && lp.GetValue() == typ.String() {
					n += m.GetCounter().GetValue()
				}
			}
		}
	}
	return n
}

// A client cannot stop the clock of the query that runs by piling up
// PREPAREs that the engine refuses (an unknown field after the uuid) and
// leaving before the answer, which with the default configuration streams
// chunked bodies. The gateway once lost their answers and followed them
// until their limit; the 64th made it forget the oldest answered query,
// the one that ran.
func TestAbandonedPreparesDoNotStopTheClock(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 2s}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	mustType(t, h.prepare(c, "STREAM 1 8000", uuidA), wire.TypePrepareResponse)
	fetched := make(chan []byte, 1)
	go func() { fetched <- h.fetch(c, uuidA, 1) }()
	eventually(t, "the FETCH in flight", func() bool { return h.gw.InFlight() == 1 })
	base := requestsOfType(t, h.met, wire.TypePrepareRequest)
	sql := "SELECT 1 -- " + strings.Repeat("x", 10000)
	for i := 0; i < 2*proxy.MaxSessionQueries; i++ {
		msg := wire.PrepareRequest{SQL: sql, QueryUUID: wire.Hugeint{Upper: 50, Lower: uint64(i + 1)}}.Encode(wire.Header{ConnectionID: c})
		h.postAndLeave(append(msg[:len(msg)-2], 0x09, 0x00, 0xff, 0xff))
		eventually(t, "the abandoned PREPARE handled", func() bool {
			return requestsOfType(t, h.met, wire.TypePrepareRequest) == base+float64(i+1)
		})
	}
	if _, q := proxy.TrackedQueries(h.gw); q != 1 {
		t.Fatalf("%d queries followed after the abandoned PREPAREs, want the one that runs", q)
	}
	mustTimeout(t, wait(t, "the FETCH", fetched), "2s", "limits.queryTimeout")
	waitCancels(t, srv, uuidA)
}

// The same with PREPAREs that the engine got whole and holds (a slow
// quack_authorization_function): the gateway follows them up to the bound,
// answers a further PREPARE itself with an error, and cancels the query
// that runs at its limit.
func TestHeldPreparesDoNotStopTheClock(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 3s}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	mustType(t, h.prepare(c, "STREAM 1 8000", uuidA), wire.TypePrepareResponse)
	fetched := make(chan []byte, 1)
	go func() { fetched <- h.fetch(c, uuidA, 1) }()
	eventually(t, "the FETCH in flight", func() bool { return h.gw.InFlight() == 1 })
	srv.PrepareDelay = 4 * time.Second
	for i := 1; i < proxy.MaxSessionQueries; i++ {
		h.postAndLeave(wire.PrepareRequest{SQL: "SELECT 1", QueryUUID: wire.Hugeint{Upper: 50, Lower: uint64(i)}}.Encode(wire.Header{ConnectionID: c}))
	}
	eventually(t, "the bound reached", func() bool { _, q := proxy.TrackedQueries(h.gw); return q == proxy.MaxSessionQueries })
	e := mustError(t, h.prepare(c, "SELECT 2", uuidC), "the session has 64 queries that are not over, the most the gateway follows")
	if e.ExceptionType != wire.ExceptionInvalidInput || e.MustInvalidate {
		t.Fatalf("refusal %+v: want type %s without must_invalidate", e, wire.ExceptionInvalidInput)
	}
	if ran(srv, "SELECT 2") {
		t.Fatal("the gateway forwarded the PREPARE beyond the bound")
	}
	mustTimeout(t, wait(t, "the FETCH", fetched), "3s", "limits.queryTimeout")
	waitCancels(t, srv, uuidA)
}

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

// A client that sends a PREPARE whole and goes away before the answer: the
// engine runs the query anyway, and the gateway waits for its answer. A
// query that ends before its limit gets no CANCEL and is no timeout; the
// gateway once lost the answer and cancelled and counted the query at its
// limit.
func TestAbandonedPrepareThatEndsIsNoTimeout(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 300ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	h.postAndLeave(wire.PrepareRequest{SQL: "SLEEP 100", QueryUUID: uuidB}.Encode(wire.Header{ConnectionID: c}))
	eventually(t, "the engine's answer", func() bool { return requestsOfType(t, h.met, wire.TypePrepareRequest) == 1 })
	if n := requestCount(t, h.met, "client_gone"); n != 1 {
		t.Errorf("%v requests counted as client_gone, want the abandoned PREPARE", n)
	}
	time.Sleep(600 * time.Millisecond) // past the limit
	noTimeouts(t, h, srv)
	mustType(t, h.prepare(c, "SELECT 1", uuidC), wire.TypePrepareResponse)
	if _, q := proxy.TrackedQueries(h.gw); q != 1 {
		t.Fatalf("%d queries followed, want the last one", q)
	}
}

// ... and a query that still runs at its limit is cancelled on the engine
// then, like any other.
func TestAbandonedPrepareIsCancelledAtItsLimit(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 300ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	h.postAndLeave(wire.PrepareRequest{SQL: "SLEEP 10000", QueryUUID: uuidB}.Encode(wire.Header{ConnectionID: c}))
	waitCancels(t, srv, uuidB)
	waitTimeouts(t, h.met, "cancelled", 1)
	eventually(t, "the PREPARE answered", func() bool { return requestsOfType(t, h.met, wire.TypePrepareRequest) == 1 })
	mustType(t, h.prepare(c, "SELECT 1", uuidC), wire.TypePrepareResponse)
}
