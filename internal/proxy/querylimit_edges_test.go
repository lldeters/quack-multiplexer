package proxy_test

import (
	"context"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quackmux/internal/proxy"
	"quackmux/internal/quacktest"
	"quackmux/internal/wire"
)

// The query time limit with clients that complete, abandon, race or craft
// their requests.

// noTimeouts checks that the gateway sent no CANCEL and counted no timeout.
func noTimeouts(t *testing.T, h *harness, srv *quacktest.Server) {
	t.Helper()
	if got := cancels(srv); len(got) != 0 {
		t.Errorf("engine got CANCEL for %v", got)
	}
	if n := timeouts(t, h.met, "cancelled") + timeouts(t, h.met, "failed"); n != 0 {
		t.Errorf("%v timeouts counted", n)
	}
}

// postSlowly sends msg to the gateway: all of it but the last tail bytes at
// once, the tail after hold. With chunked set, the body has no
// Content-Length, and extra trailing bytes follow the message one by one.
func (h *harness) postSlowly(msg []byte, tail int, hold time.Duration, chunked bool, trailing int) []byte {
	h.t.Helper()
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write(msg[:len(msg)-tail])
		time.Sleep(hold)
		_, _ = pw.Write(msg[len(msg)-tail:])
		for i := 0; i < trailing; i++ {
			time.Sleep(250 * time.Millisecond)
			_, _ = pw.Write([]byte{0})
		}
		_ = pw.Close()
	}()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/quack", pr)
	if err != nil {
		h.t.Fatal(err)
	}
	req.ContentLength = int64(len(msg) + trailing)
	if chunked {
		req.ContentLength = -1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return body
}

// ran reports whether the engine received a PREPARE with sql.
func ran(srv *quacktest.Server, sql string) bool {
	for _, r := range srv.Received() {
		if r.Type == wire.TypePrepareRequest && r.SQL == sql {
			return true
		}
	}
	return false
}

// Sessions that close while the limit of their query passes: the gateway
// neither crashes nor keeps anything for them. For a query with the zero
// uuid (the client's catalog traffic) it once indexed the empty list of the
// session's queries then, in a timer goroutine, and the process died.
func TestQueryLimitWhileSessionsClose(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 30ms}\n", nil)
	var wg sync.WaitGroup
	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				c := h.connect("tok?user=alice", nil)
				done := make(chan struct{})
				go func() {
					defer close(done)
					h.post(wire.PrepareRequest{SQL: "SLEEP 3000"}.Encode(wire.Header{ConnectionID: c}), nil)
				}()
				time.Sleep(29*time.Millisecond + time.Duration(rand.IntN(2000))*time.Microsecond)
				h.disconnect(c)
				<-done
			}
		}()
	}
	wg.Wait()
	eventually(t, "no state left", func() bool { s, q := proxy.TrackedQueries(h.gw); return s == 0 && q == 0 })
}

// FETCHes for uuids the gateway does not know (the engine never had them, or
// their query is over) leave nothing behind and cause no CANCEL: every query
// of a session that this process opened passed it as a PREPARE.
func TestFetchesForUnknownQueriesLeaveNothing(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 300ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	mustType(t, h.prepare(c, "STREAM 3 60000", uuidA), wire.TypePrepareResponse)
	for i := uint64(0); i < 300; i++ {
		u := wire.Hugeint{Upper: 77, Lower: i + 1}
		body := h.post(wire.FetchRequest{QueryUUID: u, BatchIndex: i % 2}.Encode(wire.Header{ConnectionID: c}), nil)
		mustError(t, body, quacktest.MsgResultClosed)
	}
	if s, q := proxy.TrackedQueries(h.gw); s != 1 || q != 1 {
		t.Fatalf("%d sessions, %d queries followed after FETCHes for unknown queries, want the query that runs", s, q)
	}
	time.Sleep(800 * time.Millisecond)
	noTimeouts(t, h, srv)
}

// An INSERT from the client (also CTAS) as the DuckDB client runs it: the
// PREPARE, the uploads, the terminal SEND_DATA, and one FETCH for the row
// count. The statement is complete then: the session may rest past the
// limit without a CANCEL or a timeout.
func TestCompletedInsertIsNotCancelled(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 400ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	mustType(t, h.prepare(c, "STREAM 1 0", uuidA), wire.TypePrepareResponse)
	mustType(t, h.sendData(c), wire.TypeSendDataResponse)
	mustType(t, h.sendData(c), wire.TypeSendDataResponse) // the terminal one
	if f, err := wire.DecodeFetchResponseHead(h.fetch(c, uuidA, 1)); err != nil || f.Terminal() {
		t.Fatalf("FETCH 1: %+v, %v", f, err)
	}
	time.Sleep(time.Second)
	noTimeouts(t, h, srv)
	mustType(t, h.prepare(c, "COMMIT", wire.Hugeint{}), wire.TypePrepareResponse)
	if _, q := proxy.TrackedQueries(h.gw); q != 1 {
		t.Fatalf("%d queries followed, want the COMMIT only", q)
	}
}

// With read-aheads, a FETCH can reach the gateway after another one got the
// terminal answer, and a scan with LIMIT stops reading early. Neither query
// gets a CANCEL or counts as a timeout.
func TestLateAndAbandonedFetchesAreNoTimeouts(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 400ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	mustType(t, h.prepare(c, "STREAM 2 0", uuidA), wire.TypePrepareResponse)
	for _, i := range []uint64{1, 3, 2} {
		f, err := wire.DecodeFetchResponseHead(h.fetch(c, uuidA, i))
		if err != nil || f.Terminal() != (i == 3) {
			t.Fatalf("FETCH %d: %+v, %v", i, f, err)
		}
	}
	if _, q := proxy.TrackedQueries(h.gw); q != 0 {
		t.Fatalf("%d queries followed after a late FETCH", q)
	}
	mustType(t, h.prepare(c, "STREAM 50 0", uuidB), wire.TypePrepareResponse)
	for i := uint64(1); i <= 3; i++ {
		h.fetch(c, uuidB, i)
	}
	time.Sleep(time.Second)
	noTimeouts(t, h, srv)
	mustType(t, h.prepare(c, "SELECT 1", uuidC), wire.TypePrepareResponse)
	if _, q := proxy.TrackedQueries(h.gw); q != 1 {
		t.Fatalf("%d queries followed, want the last one", q)
	}
}

// A PREPARE whose body the client holds back is cut off at the limit: the
// engine never gets all of it, so it cannot run the query. Neither the zero
// uuid nor the uuid of the statement before (a CANCEL with either would
// meet that statement) nor trailing bytes after a chunked message let the
// query run past its limit.
func TestPrepareHeldBackIsCutOffAtTheLimit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cfg      string
		uuid     wire.Hugeint
		chunked  bool
		trailing int
	}{
		{"zero uuid", "server: {upstream: {maxBufferedRequestBytes: -1}}\n", wire.Hugeint{}, false, 0},
		{"uuid of the statement before", "server: {upstream: {maxBufferedRequestBytes: -1}}\n", uuidA, false, 0},
		{"chunked with trailing bytes", "", uuidA, true, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "limits: {queryTimeout: 300ms}\n"+tc.cfg, nil)
			c := h.connect("tok?user=alice", nil)
			srv := h.prov.Server(h.sessionEngine(c))
			mustType(t, h.prepare(c, "SELECT 1", uuidA), wire.TypePrepareResponse)
			sql := "SLEEP 1500 " + strings.Repeat("x", 8000)
			msg := wire.PrepareRequest{SQL: sql, QueryUUID: tc.uuid}.Encode(wire.Header{ConnectionID: c})
			tail := 2
			if tc.chunked {
				tail = 0
			}
			mustTimeout(t, h.postSlowly(msg, tail, 1200*time.Millisecond, tc.chunked, tc.trailing), "300ms", "limits.queryTimeout")
			waitTimeouts(t, h.met, "cancelled", 1)
			if ran(srv, sql) {
				t.Fatal("the engine ran the query that was cut off")
			}
			mustType(t, h.prepare(c, "SELECT 2", uuidB), wire.TypePrepareResponse)
		})
	}
}

// The engine got all of a PREPARE but makes it its statement late (a slow
// authorization function): a CANCEL with the zero uuid, or with the uuid of
// the statement before, meets that statement, and one with a new uuid is
// refused. The gateway cancels again until the query ends.
func TestPrepareInstalledLateIsCancelled(t *testing.T) {
	for _, u := range []wire.Hugeint{{}, uuidA, uuidB} {
		h := newHarness(t, "limits: {queryTimeout: 300ms}\n", nil)
		c := h.connect("tok?user=alice", nil)
		srv := h.prov.Server(h.sessionEngine(c))
		mustType(t, h.prepare(c, "SELECT 1", uuidA), wire.TypePrepareResponse)
		srv.PrepareDelay = 900 * time.Millisecond
		start := time.Now()
		mustTimeout(t, h.prepare(c, "SLEEP 10000", u), "300ms", "limits.queryTimeout")
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("uuid %v: the query ran %s", u, d)
		}
		waitTimeouts(t, h.met, "cancelled", 1)
		if n := len(cancels(srv)); n < 2 {
			t.Fatalf("uuid %v: %d CANCELs, want more than one", u, n)
		}
	}
}

// DuckDB does not interrupt a commit: a COMMIT that the gateway cancelled
// would be reported as interrupted although it committed. Statements that
// end a transaction therefore have no limit.
func TestCommitHasNoLimit(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 300ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	srv.PrepareDelay = 800 * time.Millisecond // a commit that takes long
	for _, sql := range []string{"COMMIT", "ROLLBACK"} {
		mustType(t, h.prepare(c, sql, wire.Hugeint{}), wire.TypePrepareResponse)
	}
	noTimeouts(t, h, srv)
	mustTimeout(t, h.prepare(c, "SLEEP 10000", wire.Hugeint{}), "300ms", "limits.queryTimeout")
}

// A second PREPARE with the uuid of the query that runs, which the engine
// rejects (it cannot decode it), does not stop the clock of that query.
func TestRejectedPrepareWithTheSameUUIDKeepsTheClock(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 300ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	first := make(chan []byte, 1)
	go func() { first <- h.prepare(c, "SLEEP 5000", uuidA) }()
	eventually(t, "the first PREPARE at the engine", func() bool { return h.gw.InFlight() == 1 })
	msg := wire.PrepareRequest{SQL: "SELECT 1", QueryUUID: uuidA}.Encode(wire.Header{ConnectionID: c})
	msg = append(msg[:len(msg)-2], 0x09, 0x00, 0xff, 0xff) // an unknown field instead of the end
	mustError(t, h.post(msg, nil), "Failed to deserialize")
	mustTimeout(t, wait(t, "the first PREPARE", first), "300ms", "limits.queryTimeout")
}

// A FETCH the client sends while the PREPARE of its query still waits is
// answered by the engine about whatever statement it has: the answer does
// not end the query.
func TestFetchBeforeThePrepareAnswerKeepsTheClock(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 300ms}\n", nil)
	c := h.connect("tok?user=alice", nil)
	first := make(chan []byte, 1)
	go func() { first <- h.prepare(c, "SLEEP 5000", uuidA) }()
	eventually(t, "the PREPARE at the engine", func() bool { return h.gw.InFlight() == 1 })
	f, err := wire.DecodeFetchResponseHead(h.fetch(c, uuidA, 1))
	if err != nil || !f.Terminal() {
		t.Fatalf("FETCH: %+v, %v (the fake engine answers terminal for a statement without a stream)", f, err)
	}
	mustTimeout(t, wait(t, "the PREPARE", first), "300ms", "limits.queryTimeout")
}

// A replica without a fresh lease does not cancel queries: another replica
// may serve the session by now. It looks again until it is fresh.
func TestQueryLimitWaitsForAFreshLease(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 300ms}\n", nil)
	var fresh atomic.Bool
	proxy.SetFence(h.gw, fresh.Load)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	answer := make(chan []byte, 1)
	go func() { answer <- h.prepare(c, "SLEEP 10000", uuidA) }()
	time.Sleep(900 * time.Millisecond)
	if got := cancels(srv); len(got) != 0 {
		t.Fatalf("engine got CANCEL for %v without a fresh lease", got)
	}
	fresh.Store(true)
	mustTimeout(t, wait(t, "the PREPARE", answer), "300ms", "limits.queryTimeout")
	waitCancels(t, srv, uuidA)
}

// A streamed PREPARE whose client goes away before its uuid passed never
// reached the engine whole: it is no timeout, and nothing is kept for it.
func TestPrepareOfAClientThatLeavesEarly(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 300ms}\nserver: {upstream: {maxBufferedRequestBytes: -1}}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	msg := wire.PrepareRequest{SQL: "SLEEP 10 " + strings.Repeat("x", 100000), QueryUUID: uuidA}.Encode(wire.Header{ConnectionID: c})
	pr, pw := io.Pipe()
	go func() { _, _ = pw.Write(msg[:len(msg)/2]) }() // and nothing more
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL+"/quack", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(msg))
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	eventually(t, "the PREPARE forwarded", func() bool { _, q := proxy.TrackedQueries(h.gw); return q == 1 })
	cancel() // the client goes away
	_ = pw.CloseWithError(context.Canceled)
	<-done
	eventually(t, "the query forgotten", func() bool { _, q := proxy.TrackedQueries(h.gw); return q == 0 })
	time.Sleep(600 * time.Millisecond)
	noTimeouts(t, h, srv)
}
