package proxy_test

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"quackmux/internal/wire"
)

// The query time limit with FETCHes and uploads that no query of the
// session claims when the gateway forwards them. The gateway reads the uuid
// of a FETCH from the start of its body and forwards a body it does not
// buffer while it streams in: the engine gets such a request only once all
// of it arrived, and it serves the statement the connection has then.

// A FETCH for a uuid that has no query yet, whose body comes slowly: the
// engine gets it after the PREPARE of that uuid became its statement, and
// the FETCH waits for a batch of that query. The gateway once did not count
// it as a request of the query: at its limit the query looked left alone,
// and the FETCH got its batch long after.
func TestFetchSentBeforeItsQueryKeepsTheClock(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 600ms}\nserver: {upstream: {maxBufferedRequestBytes: -1}}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	fetch := wire.FetchRequest{QueryUUID: uuidA, BatchIndex: 1}.Encode(wire.Header{ConnectionID: c})
	fetch = append(fetch, make([]byte, 400)...) // the engine ignores what follows the message
	fetched := make(chan []byte, 1)
	go func() { fetched <- h.postSlowly(fetch, 8, 600*time.Millisecond, false, 0) }()
	eventually(t, "the FETCH at the gateway", func() bool { return h.gw.InFlight() == 1 })
	time.Sleep(100 * time.Millisecond) // forwarded, its body on the way
	mustType(t, h.prepare(c, "STREAM 2 3000", uuidA), wire.TypePrepareResponse)
	mustTimeout(t, wait(t, "the FETCH", fetched), "600ms", "limits.queryTimeout")
	waitCancels(t, srv, uuidA)
}

// An upload whose body comes slowly serves the statement the engine has
// when all of it arrived. Whether the gateway saw no query when it
// forwarded the upload, or only the one before the upload's statement,
// which the next PREPARE replaced, the upload counts as a request of the
// statement that comes: at its limit the gateway cancels it, and the upload
// gets the gateway's error. The gateway once left the statement alone at
// its limit, and the upload kept it busy.
func TestUploadSentBeforeItsStatementKeepsTheClock(t *testing.T) {
	for _, before := range []string{"", "SELECT 1"} {
		t.Run("before="+before, func(t *testing.T) {
			h := newHarness(t, "limits: {queryTimeout: 600ms}\nserver: {upstream: {maxBufferedRequestBytes: -1}}\n", nil)
			c := h.connect("tok?user=alice", nil)
			srv := h.prov.Server(h.sessionEngine(c))
			if before != "" {
				mustType(t, h.prepare(c, before, uuidB), wire.TypePrepareResponse)
			}
			upload := wire.EncodeEmpty(wire.Header{Type: wire.TypeSendDataRequest, ConnectionID: c})
			upload = append(upload, make([]byte, 400)...)
			sent := make(chan []byte, 1)
			go func() { sent <- h.postSlowly(upload, 8, 600*time.Millisecond, false, 0) }()
			eventually(t, "the upload at the gateway", func() bool { return h.gw.InFlight() == 1 })
			time.Sleep(100 * time.Millisecond) // forwarded, its body on the way
			mustType(t, h.prepare(c, "UPLOAD 3000", uuidA), wire.TypePrepareResponse)
			mustTimeout(t, wait(t, "the upload", sent), "600ms", "limits.queryTimeout")
			waitCancels(t, srv, uuidA)
		})
	}
}

// holdBack sends msg, but for its last bytes, over a connection of its own
// (with a Content-Length for all of it): the gateway forwards the request,
// and the engine never gets all of it. Closing the connection abandons the
// request; the test closes it at its end in any case.
func (h *harness) holdBack(msg []byte) net.Conn {
	h.t.Helper()
	msg = append(msg, make([]byte, 400)...) // the engine ignores what follows the message
	addr := strings.TrimPrefix(h.srv.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = conn.Close() })
	head := fmt.Sprintf("POST /quack HTTP/1.1\r\nHost: %s\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n",
		addr, wire.ContentType, len(msg))
	if _, err := conn.Write(append([]byte(head), msg[:len(msg)-8]...)); err != nil {
		h.t.Fatal(err)
	}
	return conn
}

// A FETCH whose body comes slowly is in flight for a query when a PREPARE
// with the same uuid replaces that query: the engine serves the FETCH for
// the new query, and it counts as a request of that query. The gateway once
// forgot the uuid of such a FETCH when its query ended, so the new query did
// not take it: the query looked left alone at its limit, and the FETCH got
// its batch long after.
func TestFetchOfAReplacedQueryKeepsTheClock(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 600ms}\nserver: {upstream: {maxBufferedRequestBytes: -1}}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	mustType(t, h.prepare(c, "STREAM 3 60000", uuidA), wire.TypePrepareResponse)
	fetch := wire.FetchRequest{QueryUUID: uuidA, BatchIndex: 1}.Encode(wire.Header{ConnectionID: c})
	fetch = append(fetch, make([]byte, 400)...)
	fetched := make(chan []byte, 1)
	go func() { fetched <- h.postSlowly(fetch, 8, time.Second, false, 0) }()
	eventually(t, "the FETCH at the gateway", func() bool { return h.gw.InFlight() == 1 })
	time.Sleep(100 * time.Millisecond) // forwarded, its body on the way
	// This query replaces the first one.
	mustType(t, h.prepare(c, "STREAM 2 3000", uuidA), wire.TypePrepareResponse)
	mustTimeout(t, wait(t, "the FETCH", fetched), "600ms", "limits.queryTimeout")
	waitCancels(t, srv, uuidA)
}

// A statement without a limit (COMMIT, ROLLBACK) claims no FETCH and no
// upload, also no FETCH with its uuid: the engine may serve the request for
// the next query, whose clock then counts it. The gateway once counted such
// a request for a COMMIT whose body the client held back, and the query
// that the engine served the request for passed its limit unattended.
func TestStatementWithoutALimitClaimsNoRequest(t *testing.T) {
	for _, upload := range []bool{false, true} {
		t.Run(fmt.Sprintf("upload=%v", upload), func(t *testing.T) {
			h := newHarness(t, "limits: {queryTimeout: 600ms}\nserver: {upstream: {maxBufferedRequestBytes: -1}}\n", nil)
			c := h.connect("tok?user=alice", nil)
			srv := h.prov.Server(h.sessionEngine(c))
			h.holdBack(wire.PrepareRequest{SQL: "COMMIT", QueryUUID: uuidA}.Encode(wire.Header{ConnectionID: c}))
			eventually(t, "the COMMIT at the gateway", func() bool { return h.gw.InFlight() == 1 })
			req := wire.FetchRequest{QueryUUID: uuidA, BatchIndex: 1}.Encode(wire.Header{ConnectionID: c})
			sql := "STREAM 2 3000"
			if upload {
				req = wire.EncodeEmpty(wire.Header{Type: wire.TypeSendDataRequest, ConnectionID: c})
				sql = "UPLOAD 3000"
			}
			req = append(req, make([]byte, 400)...)
			answered := make(chan []byte, 1)
			go func() { answered <- h.postSlowly(req, 8, time.Second, false, 0) }()
			eventually(t, "the request at the gateway", func() bool { return h.gw.InFlight() == 2 })
			time.Sleep(100 * time.Millisecond) // forwarded, its body on the way
			mustType(t, h.prepare(c, sql, uuidA), wire.TypePrepareResponse)
			mustTimeout(t, wait(t, "the request", answered), "600ms", "limits.queryTimeout")
			waitCancels(t, srv, uuidA)
		})
	}
}

// The DISCONNECT of a closed session waits only while the limit may still
// have to cancel a query of it. An upload that a held-back COMMIT once
// claimed moved, when the COMMIT ended, to the statement that the engine
// served it for, which had passed its limit unattended: nothing cancelled
// the statement, and the DISCONNECT waited until the engine answered the
// upload (about 7 s after the client's with a limit of 300 ms).
func TestClosedSessionIsNotHeldPastTheLimit(t *testing.T) {
	h := newHarness(t, "limits: {queryTimeout: 600ms}\nserver: {upstream: {maxBufferedRequestBytes: -1}}\n", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	commit := h.holdBack(wire.PrepareRequest{SQL: "COMMIT", QueryUUID: uuidB}.Encode(wire.Header{ConnectionID: c}))
	eventually(t, "the COMMIT at the gateway", func() bool { return h.gw.InFlight() == 1 })
	upload := wire.EncodeEmpty(wire.Header{Type: wire.TypeSendDataRequest, ConnectionID: c})
	upload = append(upload, make([]byte, 400)...)
	sent := make(chan []byte, 1)
	go func() { sent <- h.postSlowly(upload, 8, 500*time.Millisecond, false, 0) }()
	eventually(t, "the upload at the gateway", func() bool { return h.gw.InFlight() == 2 })
	time.Sleep(100 * time.Millisecond) // forwarded, its body on the way
	mustType(t, h.prepare(c, "UPLOAD 8000", uuidA), wire.TypePrepareResponse)
	time.Sleep(900 * time.Millisecond) // past the limit of the UPLOAD statement
	_ = commit.Close()                 // the COMMIT ends
	mustType(t, h.disconnect(c), wire.TypeSuccessResponse)
	closed := time.Now()
	eventually(t, "the DISCONNECT at the engine", func() bool { return srv.CountType(wire.TypeDisconnect) == 1 })
	if d := time.Since(closed); d > 2*time.Second {
		t.Errorf("the engine got the DISCONNECT %s after the client's (limit 600ms)", d.Round(time.Millisecond))
	}
	mustTimeout(t, wait(t, "the upload", sent), "600ms", "limits.queryTimeout")
	waitCancels(t, srv, uuidA)
}
