package proxy_test

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"

	"quackmux/internal/metrics"
	"quackmux/internal/proxy"
	"quackmux/internal/quacktest"
	"quackmux/internal/wire"
)

// The tests of the query time limit run with real timers and short limits;
// waits are generous, so that a loaded machine does not make them flaky.

var (
	uuidA = wire.Hugeint{Upper: 11, Lower: 1}
	uuidB = wire.Hugeint{Upper: 11, Lower: 2}
	uuidC = wire.Hugeint{Upper: 11, Lower: 3}
)

func (h *harness) prepare(connID, sql string, u wire.Hugeint) []byte {
	h.t.Helper()
	return h.post(wire.PrepareRequest{SQL: sql, QueryUUID: u}.Encode(wire.Header{ConnectionID: connID, ClientQueryID: wire.SomeIdx(5)}), nil)
}

func (h *harness) fetch(connID string, u wire.Hugeint, batch uint64) []byte {
	h.t.Helper()
	return h.post(wire.FetchRequest{QueryUUID: u, BatchIndex: batch, AckIndex: batch - 1}.Encode(wire.Header{ConnectionID: connID}), nil)
}

func (h *harness) cancelQuery(connID string, u wire.Hugeint) []byte {
	h.t.Helper()
	return h.post(wire.EncodeCancel(connID, u), nil)
}

func (h *harness) sendData(connID string) []byte {
	h.t.Helper()
	return h.post(wire.EncodeEmpty(wire.Header{Type: wire.TypeSendDataRequest, ConnectionID: connID}), nil)
}

// mustTimeout checks that body is the gateway's error for a query that ran
// out of time.
func mustTimeout(t *testing.T, body []byte, limit, source string) {
	t.Helper()
	e := mustError(t, body, "ran longer than its time limit of "+limit+" ("+source+") and was cancelled")
	if e.ExceptionType != wire.ExceptionInterrupt || e.MustInvalidate {
		t.Fatalf("timeout error %+v: want type %s without must_invalidate", e, wire.ExceptionInterrupt)
	}
}

// cancels returns the uuids of the CANCELs an engine received.
func cancels(srv *quacktest.Server) []wire.Hugeint {
	var out []wire.Hugeint
	for _, r := range srv.Received() {
		if r.Type == wire.TypeCancelRequest {
			out = append(out, r.QueryUUID)
		}
	}
	return out
}

// timeouts returns quackmux_query_timeouts_total for a result.
func timeouts(t *testing.T, met *metrics.Metrics, result string) float64 {
	t.Helper()
	mfs, err := met.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var n float64
	for _, mf := range mfs {
		if mf.GetName() != "quackmux_query_timeouts_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "result" && lp.GetValue() == result {
					n += m.GetCounter().GetValue()
				}
			}
		}
	}
	return n
}

// wait receives an answer that must come within 15s.
func wait(t *testing.T, what string, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case b := <-ch:
		return b
	case <-time.After(15 * time.Second):
		t.Fatalf("no answer to %s", what)
		return nil
	}
}

// A query that runs into its limit while the client waits for its PREPARE is
// cancelled on the engine; the client gets the gateway's error, and the
// session stays usable.
func TestQueryTimeoutCancelsRunningQuery(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 300ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	start := time.Now()
	mustTimeout(t, h.prepare(c, "SLEEP 30000", uuidA), "300ms", "limits.queryTimeout")
	if d := time.Since(start); d < 300*time.Millisecond || d > 10*time.Second {
		t.Fatalf("the query was cancelled after %s", d)
	}
	if got := cancels(srv); len(got) != 1 || got[0] != uuidA {
		t.Fatalf("engine got CANCEL for %v, want [%v]", got, uuidA)
	}
	mustType(t, h.prepare(c, "SELECT 1", uuidB), wire.TypePrepareResponse)
	if n := timeouts(t, h.met, "cancelled"); n != 1 {
		t.Fatalf("quackmux_query_timeouts_total{result=cancelled} = %v, want 1", n)
	}
	if n := requestCount(t, h.met, "query_timeout"); n != 1 {
		t.Fatalf("requests with outcome query_timeout: %v, want 1", n)
	}
	// Timed-out queries leave nothing behind: only the last query rests.
	for i := 0; i < 3; i++ {
		mustTimeout(t, h.prepare(c, "SLEEP 30000", uuidC), "300ms", "limits.queryTimeout")
	}
	mustType(t, h.prepare(c, "SELECT 1", uuidB), wire.TypePrepareResponse)
	if s, q := proxy.TrackedQueries(h.gw); s != 1 || q != 1 {
		t.Fatalf("%d sessions, %d queries followed, want the last query only", s, q)
	}
}

// Queries that end below the limit are not touched, also when the limit
// passes after their end: the engine gets no CANCEL, and the gateway forgets
// fetched results.
func TestQueryBelowLimitIsUnaffected(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 400ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	mustType(t, h.prepare(c, "SLEEP 50", uuidA), wire.TypePrepareResponse)
	mustType(t, h.prepare(c, "STREAM 3 10", uuidB), wire.TypePrepareResponse)
	for i := uint64(1); i <= 4; i++ {
		f, err := wire.DecodeFetchResponseHead(h.fetch(c, uuidB, i))
		if err != nil || f.Terminal() != (i == 4) {
			t.Fatalf("FETCH %d: %+v, %v", i, f, err)
		}
	}
	if _, q := proxy.TrackedQueries(h.gw); q != 0 {
		t.Fatalf("%d queries followed after the result was fetched", q)
	}
	// A query whose PREPARE_RESPONSE may hold the whole result rests until
	// the next one.
	mustType(t, h.prepare(c, "SELECT 1", uuidC), wire.TypePrepareResponse)
	time.Sleep(time.Second)
	if got := cancels(srv); len(got) != 0 {
		t.Fatalf("engine got CANCEL for %v", got)
	}
	mustType(t, h.prepare(c, "SELECT 2", uuidA), wire.TypePrepareResponse)
	if _, q := proxy.TrackedQueries(h.gw); q != 1 {
		t.Fatalf("%d queries followed, want the last one", q)
	}
}

// FETCHes that wait for a batch when the limit passes get the gateway's
// error; so does every later FETCH of the query, without reaching the engine.
func TestQueryTimeoutWhileFetching(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 300ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	mustType(t, h.prepare(c, "STREAM 3 60000", uuidA), wire.TypePrepareResponse)
	answers := make(chan []byte, 2)
	for i := uint64(1); i <= 2; i++ {
		go func() { answers <- h.fetch(c, uuidA, i) }()
	}
	for i := 0; i < 2; i++ {
		mustTimeout(t, wait(t, "a waiting FETCH", answers), "300ms", "limits.queryTimeout")
	}
	fetches := srv.CountType(wire.TypeFetchRequest)
	mustTimeout(t, h.fetch(c, uuidA, 3), "300ms", "limits.queryTimeout")
	if n := srv.CountType(wire.TypeFetchRequest); n != fetches {
		t.Fatalf("the FETCH after the timeout reached the engine (%d FETCHes, before %d)", n, fetches)
	}
	if got := cancels(srv); len(got) != 1 || got[0] != uuidA {
		t.Fatalf("engine got CANCEL for %v, want [%v]", got, uuidA)
	}
	mustType(t, h.prepare(c, "SELECT 1", uuidB), wire.TypePrepareResponse)
	if s, q := proxy.TrackedQueries(h.gw); s != 1 || q != 1 {
		t.Fatalf("%d sessions, %d queries followed, want the new query only", s, q)
	}
}

// Time the client spends between FETCHes counts: the gateway cancels the
// query while the client rests, and its next FETCH gets the error.
func TestQueryTimeoutBetweenFetches(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 400ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	mustType(t, h.prepare(c, "STREAM 5 10", uuidA), wire.TypePrepareResponse)
	for i := uint64(1); i <= 2; i++ {
		if f, err := wire.DecodeFetchResponseHead(h.fetch(c, uuidA, i)); err != nil || f.Terminal() {
			t.Fatalf("FETCH %d: %+v, %v", i, f, err)
		}
	}
	eventually(t, "CANCEL at the limit", func() bool { return len(cancels(srv)) == 1 })
	mustTimeout(t, h.fetch(c, uuidA, 3), "400ms", "limits.queryTimeout")
}

// The gateway cannot tell from a PREPARE_RESPONSE whether the result is
// complete. A query that rests after it is not cancelled at its limit, but as
// soon as the client fetches for it.
func TestRestingQueryIsCancelledWhenTheClientReturns(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 300ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	mustType(t, h.prepare(c, "STREAM 2 10", uuidA), wire.TypePrepareResponse)
	time.Sleep(800 * time.Millisecond)
	if got := cancels(srv); len(got) != 0 {
		t.Fatalf("engine got CANCEL for %v while the query rested", got)
	}
	mustTimeout(t, h.fetch(c, uuidA, 1), "300ms", "limits.queryTimeout")
	if got := cancels(srv); len(got) != 1 || got[0] != uuidA {
		t.Fatalf("engine got CANCEL for %v, want [%v]", got, uuidA)
	}
}

// Uploads count like FETCHes: an upload that runs past the limit is cancelled,
// and the next part of it gets the error.
func TestQueryTimeoutDuringUpload(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 400ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	mustType(t, h.prepare(c, "STREAM 1 60000", uuidA), wire.TypePrepareResponse) // INSERT ... FROM the client
	mustType(t, h.sendData(c), wire.TypeSendDataResponse)
	eventually(t, "CANCEL at the limit", func() bool { return len(cancels(srv)) == 1 })
	uploads := srv.CountType(wire.TypeSendDataRequest)
	mustTimeout(t, h.sendData(c), "400ms", "limits.queryTimeout")
	if n := srv.CountType(wire.TypeSendDataRequest); n != uploads {
		t.Fatal("the upload after the timeout reached the engine")
	}
}

// If the engine does not confirm the CANCEL, the gateway answers the waiting
// request itself.
func TestQueryTimeoutWhenTheCancelFails(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 300ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	srv.CancelError = "boom"
	start := time.Now()
	mustTimeout(t, h.prepare(c, "SLEEP 3000", uuidA), "300ms", "limits.queryTimeout")
	if d := time.Since(start); d > 2500*time.Millisecond {
		t.Fatalf("the client waited %s for the query that the engine did not cancel", d)
	}
	if n := timeouts(t, h.met, "failed"); n != 1 {
		t.Fatalf("quackmux_query_timeouts_total{result=failed} = %v, want 1", n)
	}
}

// A CANCEL of the client ends the query: the gateway does not cancel it again
// at the limit, and the engine's answers pass unchanged.
func TestClientCancelEndsQuery(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 500ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	mustType(t, h.prepare(c, "STREAM 3 60000", uuidA), wire.TypePrepareResponse)
	answer := make(chan []byte, 1)
	go func() { answer <- h.fetch(c, uuidA, 1) }()
	eventually(t, "FETCH in flight", func() bool { return h.gw.InFlight() == 1 })
	mustType(t, h.cancelQuery(c, uuidA), wire.TypeSuccessResponse)
	mustError(t, wait(t, "the FETCH", answer), quacktest.MsgCancelled)
	if _, q := proxy.TrackedQueries(h.gw); q != 0 {
		t.Fatalf("%d queries followed after the client cancelled", q)
	}
	time.Sleep(time.Second)
	if got := cancels(srv); len(got) != 1 {
		t.Fatalf("engine got CANCEL for %v, want only the client's", got)
	}
}

// A PREPARE that the engine answers with a result replaces the queries
// answered before it; one that fails replaces nothing the gateway can be sure
// of. A CANCEL at the limit of a query the engine no longer runs is refused
// by the engine; that is no timeout.
func TestSupersededQueries(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 400ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	mustType(t, h.prepare(c, "STREAM 3 10", uuidA), wire.TypePrepareResponse)
	h.fetch(c, uuidA, 1) // the client reads, the query is not complete
	mustType(t, h.prepare(c, "SELECT 1", uuidB), wire.TypePrepareResponse)
	if _, q := proxy.TrackedQueries(h.gw); q != 1 {
		t.Fatalf("%d queries followed, want only the new one", q)
	}

	mustType(t, h.prepare(c, "STREAM 3 10", uuidC), wire.TypePrepareResponse)
	h.fetch(c, uuidC, 1)
	mustError(t, h.prepare(c, "FAIL boom", uuidA), "boom")
	// The engine's statement is the failed one now: the CANCEL for uuidC at
	// its limit is refused.
	eventually(t, "CANCEL at the limit", func() bool { return len(cancels(srv)) == 1 })
	eventually(t, "the query forgotten", func() bool { _, q := proxy.TrackedQueries(h.gw); return q == 0 })
	if n := timeouts(t, h.met, "cancelled") + timeouts(t, h.met, "failed"); n != 0 {
		t.Fatalf("%v timeouts counted for a query that was over", n)
	}
	mustError(t, h.fetch(c, uuidC, 2), quacktest.MsgResultClosed)
}

// A FETCH for a query the gateway has not seen (it began before a gateway
// restart) starts the limit.
func TestQueryTimeoutForQueryFromBeforeARestart(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 300ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	// The previous gateway forwarded the PREPARE.
	msg := wire.PrepareRequest{SQL: "STREAM 3 60000", QueryUUID: uuidA}.Encode(wire.Header{ConnectionID: h.backendConn(c)})
	resp, err := http.Post(srv.URL()+"/quack", wire.ContentType, bytes.NewReader(msg))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	mustTimeout(t, h.fetch(c, uuidA, 1), "300ms", "limits.queryTimeout")
	if got := cancels(srv); len(got) != 1 || got[0] != uuidA {
		t.Fatalf("engine got CANCEL for %v, want [%v]", got, uuidA)
	}
}

// A PREPARE waits until the engine has answered the CANCEL for the previous
// query: the CANCEL must not meet the next query.
func TestPrepareWaitsForTheTimeoutCancel(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 200ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	srv.CancelDelay = 700 * time.Millisecond
	first := make(chan []byte, 1)
	go func() { first <- h.prepare(c, "SLEEP 30000", uuidA) }()
	eventually(t, "CANCEL sent", func() bool { _, q := proxy.TrackedQueries(h.gw); return q == 1 && h.gw.InFlight() == 1 })
	time.Sleep(350 * time.Millisecond) // the limit has passed, the CANCEL waits at the engine
	mustType(t, h.prepare(c, "SELECT 1", uuidB), wire.TypePrepareResponse)
	mustTimeout(t, wait(t, "the first query", first), "200ms", "limits.queryTimeout")
	var order []wire.MessageType
	for _, r := range srv.Received() {
		if r.Type == wire.TypeCancelRequest || (r.Type == wire.TypePrepareRequest && r.QueryUUID == uuidB) {
			order = append(order, r.Type)
		}
	}
	if len(order) != 2 || order[0] != wire.TypeCancelRequest {
		t.Fatalf("engine handled %v, want the CANCEL before the next PREPARE", order)
	}
	if n := timeouts(t, h.met, "cancelled"); n != 1 {
		t.Fatalf("quackmux_query_timeouts_total{result=cancelled} = %v, want 1", n)
	}
}

// A PREPARE that waits for such a CANCEL when the gateway shuts down is
// answered, and does not run.
func TestShutdownAnswersAPrepareWaitingForACancel(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 200ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	srv.CancelDelay = 3 * time.Second
	go h.prepare(c, "SLEEP 30000", uuidA)
	time.Sleep(500 * time.Millisecond)
	second := make(chan []byte, 1)
	go func() { second <- h.prepare(c, "SELECT 1", uuidB) }()
	time.Sleep(300 * time.Millisecond)
	h.gw.AbortQueries()
	mustError(t, wait(t, "the waiting PREPARE", second), "the query did not run")
	for _, r := range srv.Received() {
		if r.Type == wire.TypePrepareRequest && r.QueryUUID == uuidB {
			t.Fatal("the PREPARE reached the engine")
		}
	}
}

// The limit comes from the first matching override, else from the profile,
// else from limits.queryTimeout; 0 disables it.
func TestQueryTimeoutPerProfileAndOverride(t *testing.T) {
	h := newHarness(t, `
limits: {queryTimeout: 0s}
profiles:
  small: {queryTimeout: 300ms}
overrides:
  - groups: [admins]
    queryTimeout: 0s
`, nil)
	bob := h.connect("tok?user=bob&profile=small", nil) // analytics only
	mustTimeout(t, h.prepare(bob, "SLEEP 30000", uuidA), "300ms", "profiles.small.queryTimeout")
	alice := h.connect("tok?user=alice&profile=small", nil) // admins
	mustType(t, h.prepare(alice, "SLEEP 800", uuidA), wire.TypePrepareResponse)
	carol := h.connect("tok?user=carol", nil) // default profile: no limit
	mustType(t, h.prepare(carol, "SLEEP 800", uuidA), wire.TypePrepareResponse)
	// Bob's query ended with the error to its PREPARE; the others have no
	// limit.
	if s, q := proxy.TrackedQueries(h.gw); s != 0 || q != 0 {
		t.Fatalf("%d sessions, %d queries followed, want none", s, q)
	}
}

// When the session closes, the gateway forgets its queries at once.
func TestQueryStateEndsWithTheSession(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 300ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	mustType(t, h.prepare(c, "STREAM 3 10", uuidA), wire.TypePrepareResponse)
	h.fetch(c, uuidA, 1)
	if s, q := proxy.TrackedQueries(h.gw); s != 1 || q != 1 {
		t.Fatalf("%d sessions, %d queries followed, want 1 and 1", s, q)
	}
	mustType(t, h.disconnect(c), wire.TypeSuccessResponse)
	if s, q := proxy.TrackedQueries(h.gw); s != 0 || q != 0 {
		t.Fatalf("%d sessions, %d queries followed after DISCONNECT", s, q)
	}
	time.Sleep(700 * time.Millisecond)
	if got := cancels(srv); len(got) != 0 {
		t.Fatalf("engine got CANCEL for %v after the session closed", got)
	}
}

// Without any limit in the configuration the gateway does not follow queries.
func TestNoQueryTimeoutByDefault(t *testing.T) {
	h := newHarness(t, "", nil)
	c := h.connect("tok?user=alice", nil)
	mustType(t, h.prepare(c, "STREAM 2 10", uuidA), wire.TypePrepareResponse)
	h.fetch(c, uuidA, 1)
	if s, q := proxy.TrackedQueries(h.gw); s != 0 || q != 0 {
		t.Fatalf("%d sessions, %d queries followed without a limit", s, q)
	}
}
