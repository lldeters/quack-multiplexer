package proxy_test

import (
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
