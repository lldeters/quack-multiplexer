package proxy_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"quackmux/internal/proxy"
	"quackmux/internal/quacktest"
	"quackmux/internal/session"
	"quackmux/internal/wire"
)

// The query time limit with sessions that close while a query of them may
// still run on the engine. The Quack server lets the requests of a
// connection that are in flight finish after a DISCONNECT, and their
// statement runs on with them, while a CANCEL no longer reaches it (the
// fake engine does the same). The gateway answers the client's DISCONNECT
// at once, but the engine gets it only once the gateway is done with such a
// query.

// receivedAt returns the position of the first request of type typ among
// those the engine handled, in the order it finished them (-1: none).
func receivedAt(srv *quacktest.Server, typ wire.MessageType) int {
	for i, r := range srv.Received() {
		if r.Type == typ {
			return i
		}
	}
	return -1
}

// disconnectedAfterCancel waits until the engine got the session's
// DISCONNECT and checks that it came after the gateway's CANCEL.
func disconnectedAfterCancel(t *testing.T, srv *quacktest.Server) {
	t.Helper()
	eventually(t, "the DISCONNECT at the engine", func() bool { return srv.CountType(wire.TypeDisconnect) == 1 })
	if c, d := receivedAt(srv, wire.TypeCancelRequest), receivedAt(srv, wire.TypeDisconnect); c < 0 || c > d {
		t.Fatalf("the engine got the DISCONNECT (request %d) before the CANCEL (request %d)", d, c)
	}
}

// A client cannot stop the clock of its query by closing its session while
// the PREPARE runs: the query is cancelled at its limit as without the
// DISCONNECT, and the engine gets the DISCONNECT after the CANCEL. The
// gateway once forgot the session's queries at the DISCONNECT, and the
// query delivered its result long after its limit.
func TestDisconnectDoesNotStopTheClock(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(fmt.Sprintf("disconnect=%v", disconnect), func(t *testing.T) {
			h := newHarness(t, "limits: {queryTimeout: 300ms}\n", nil)
			c := h.connect("tok?user=alice", nil)
			srv := h.prov.Server(h.sessionEngine(c))
			start := time.Now()
			answer := make(chan []byte, 1)
			go func() { answer <- h.prepare(c, "SLEEP 5000", uuidA) }()
			eventually(t, "the PREPARE at the engine", func() bool { return h.gw.InFlight() == 1 })
			if disconnect {
				time.Sleep(100 * time.Millisecond)
				mustType(t, h.disconnect(c), wire.TypeSuccessResponse)
			}
			mustTimeout(t, wait(t, "the PREPARE", answer), "300ms", "limits.queryTimeout")
			if d := time.Since(start); d > 3*time.Second {
				t.Fatalf("the query ran %s with a limit of 300ms", d.Round(time.Millisecond))
			}
			waitCancels(t, srv, uuidA)
			waitTimeouts(t, h.met, "cancelled", 1)
			if disconnect {
				disconnectedAfterCancel(t, srv)
				eventually(t, "nothing left of the session", func() bool { s, q := proxy.TrackedQueries(h.gw); return s == 0 && q == 0 })
			}
		})
	}
}

// A query that ends below its limit after its session closed is not
// touched: it delivers its result, the engine gets no CANCEL, and the
// DISCONNECT follows the answer.
func TestDisconnectOfAQueryBelowItsLimit(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 2s}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	answer := make(chan []byte, 1)
	go func() { answer <- h.prepare(c, "SLEEP 400", uuidA) }()
	eventually(t, "the PREPARE at the engine", func() bool { return h.gw.InFlight() == 1 })
	mustType(t, h.disconnect(c), wire.TypeSuccessResponse)
	mustType(t, wait(t, "the PREPARE", answer), wire.TypePrepareResponse)
	eventually(t, "the DISCONNECT at the engine", func() bool { return srv.CountType(wire.TypeDisconnect) == 1 })
	if p, d := receivedAt(srv, wire.TypePrepareRequest), receivedAt(srv, wire.TypeDisconnect); p > d {
		t.Fatalf("the engine got the DISCONNECT (request %d) while the PREPARE (request %d) ran", d, p)
	}
	noTimeouts(t, h, srv)
	eventually(t, "nothing left of the session", func() bool { s, q := proxy.TrackedQueries(h.gw); return s == 0 && q == 0 })
}

// The client sends a PREPARE whole, goes away and closes its session: the
// engine runs the query anyway. The gateway cancels it at its limit, then
// lets the DISCONNECT go, and does not wait for the engine any longer.
func TestAbandonedPrepareOfAClosedSessionIsCancelled(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 300ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	start := time.Now()
	h.postAndLeave(wire.PrepareRequest{SQL: "SLEEP 5000", QueryUUID: uuidA}.Encode(wire.Header{ConnectionID: c}))
	eventually(t, "the PREPARE at the engine", func() bool { return h.gw.InFlight() == 1 })
	mustType(t, h.disconnect(c), wire.TypeSuccessResponse)
	waitCancels(t, srv, uuidA)
	eventually(t, "the PREPARE answered", func() bool { return h.gw.InFlight() == 0 })
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("the abandoned PREPARE of the closed session was held %s (limit 300ms)", d.Round(time.Millisecond))
	}
	disconnectedAfterCancel(t, srv)
	waitTimeouts(t, h.met, "cancelled", 1)
}

// The same for a FETCH that waits for its batch when the session closes.
func TestDisconnectWhileFetching(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 500ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	mustType(t, h.prepare(c, "STREAM 2 8000", uuidA), wire.TypePrepareResponse)
	fetched := make(chan []byte, 1)
	go func() { fetched <- h.fetch(c, uuidA, 1) }()
	eventually(t, "the FETCH at the engine", func() bool { return h.gw.InFlight() == 1 })
	mustType(t, h.disconnect(c), wire.TypeSuccessResponse)
	mustTimeout(t, wait(t, "the FETCH", fetched), "500ms", "limits.queryTimeout")
	waitCancels(t, srv, uuidA)
	disconnectedAfterCancel(t, srv)
}

// The engine got the whole PREPARE but makes it its statement only after
// the session closed (a slow authorization function): the gateway cancels
// again until it meets the statement, as for an open session.
func TestPrepareInstalledAfterTheCloseIsCancelled(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 300ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	srv.PrepareDelay = time.Second
	start := time.Now()
	answer := make(chan []byte, 1)
	go func() { answer <- h.prepare(c, "SLEEP 10000", uuidA) }()
	eventually(t, "the PREPARE at the engine", func() bool { return h.gw.InFlight() == 1 })
	mustType(t, h.disconnect(c), wire.TypeSuccessResponse)
	mustTimeout(t, wait(t, "the PREPARE", answer), "300ms", "limits.queryTimeout")
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the query ran %s with a limit of 300ms", d.Round(time.Millisecond))
	}
	waitTimeouts(t, h.met, "cancelled", 1)
	disconnectedAfterCancel(t, srv)
}

// A close that cancels (an engine drain, an operator) sends CANCEL with the
// zero uuid at once; it misses a PREPARE the engine has not made its
// statement yet. The limit still cancels that query, before the DISCONNECT.
func TestCancellingCloseOfAPrepareBeingAuthorized(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 600ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	srv.PrepareDelay = 300 * time.Millisecond
	answer := make(chan []byte, 1)
	go func() { answer <- h.prepare(c, "SLEEP 10000", uuidA) }()
	eventually(t, "the PREPARE at the engine", func() bool { return h.gw.InFlight() == 1 })
	if !h.sessions.Close(sessionIDOf(t, h, c), "admin: closed by operator",
		session.CloseMode{Disconnect: true, Cancel: true, Force: true, MustInvalidate: true}) {
		t.Fatal("close refused")
	}
	mustTimeout(t, wait(t, "the PREPARE", answer), "600ms", "limits.queryTimeout")
	waitCancels(t, srv, wire.Hugeint{}, uuidA)
	disconnectedAfterCancel(t, srv)
}

// Requests that reached the gateway before their session closed, and that
// it would forward only after the close, could keep a query of the session
// running on the engine past the DISCONNECT. The gateway answers them
// itself, as a request after the close.
func TestRequestsThatRaceTheCloseAreNotForwarded(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 400ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	mustType(t, h.prepare(c, "STREAM 3 60000", uuidA), wire.TypePrepareResponse)
	// Both are long enough for the gateway to read their header, and so
	// find their session, before the last bytes of their body arrive.
	fetch := wire.FetchRequest{QueryUUID: uuidA, BatchIndex: 1}.Encode(wire.Header{ConnectionID: c})
	fetch = append(fetch, make([]byte, 300)...) // the engine ignores what follows the message
	sql := "SLEEP 10000 -- " + strings.Repeat("x", 300)
	prepare := wire.PrepareRequest{SQL: sql, QueryUUID: uuidB}.Encode(wire.Header{ConnectionID: c})
	answers := make(chan []byte, 2)
	for _, msg := range [][]byte{fetch, prepare} {
		go func() { answers <- h.postSlowly(msg, 2, 600*time.Millisecond, false, 0) }()
	}
	eventually(t, "both requests at the gateway", func() bool { return h.gw.InFlight() == 2 })
	mustType(t, h.disconnect(c), wire.TypeSuccessResponse)
	for i := 0; i < 2; i++ {
		e := mustError(t, wait(t, "a request that raced the close", answers), "session closed: client disconnect")
		if e.ExceptionType != wire.ExceptionConnection {
			t.Fatalf("answer %+v: want type %s", e, wire.ExceptionConnection)
		}
	}
	if n := srv.CountType(wire.TypeFetchRequest); n != 0 {
		t.Fatalf("the engine got %d FETCHes of the closed session", n)
	}
	if ran(srv, sql) {
		t.Fatal("the engine got the PREPARE of the closed session")
	}
	if n := requestCount(t, h.met, "session_closed"); n != 2 {
		t.Fatalf("%v requests counted as session_closed, want 2", n)
	}
}

// A session closes that the engine no longer has (or whose engine is gone)
// while a PREPARE of it waits whose client went away: nothing follows the
// query any more, so the PREPARE ends with its client again instead of
// waiting for the engine.
func TestDetachedPrepareOfADroppedSessionEndsWithItsClient(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 2s}\n", nil)
	c := h.connect("tok?user=alice", nil)
	h.postAndLeave(wire.PrepareRequest{SQL: "SLEEP 6000", QueryUUID: uuidA}.Encode(wire.Header{ConnectionID: c}))
	eventually(t, "the PREPARE at the engine", func() bool { return h.gw.InFlight() == 1 })
	start := time.Now()
	if !h.sessions.Close(sessionIDOf(t, h, c), "engine terminated", session.CloseMode{Force: true, MustInvalidate: true}) {
		t.Fatal("close refused")
	}
	eventually(t, "the PREPARE ended", func() bool { return h.gw.InFlight() == 0 })
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the PREPARE of the dropped session waited %s for the engine", d.Round(time.Millisecond))
	}
}
