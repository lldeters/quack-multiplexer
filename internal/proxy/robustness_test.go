package proxy_test

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"quackmux/internal/wire"
)

// Unauthenticated clients cannot park requests: a request whose message does
// not arrive within server.readHeaderTimeout is answered with an error (or
// dropped) and its handler ends.
func TestSlowUnauthenticatedRequestsDoNotPark(t *testing.T) {
	h := newHarness(t, "server: {readHeaderTimeout: 1s}\n", nil)
	srv := httptest.NewUnstartedServer(h.gw)
	srv.Config.ReadHeaderTimeout = h.cfg.Server.ReadHeaderTimeout.Duration
	srv.Start()
	defer srv.Close()

	const n = 50
	conns := make([]net.Conn, 0, n)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < n; i++ {
		c, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		// Headers complete, a body announced, but only 3 bytes of it sent.
		fmt.Fprintf(c, "POST /quack HTTP/1.1\r\nHost: x\r\nContent-Length: 65536\r\n\r\n\x01\x00\x01")
	}
	eventually(t, "requests parked", func() bool { return h.gw.InFlight() == n })
	eventually(t, "parked requests ended", func() bool { return h.gw.InFlight() == 0 })

	// A handshake whose body stalls gets an answer, not a dropped connection.
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	msg := connectMsg("tok?user=alice")
	fmt.Fprintf(c, "POST /quack HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n", len(msg)+10)
	_, _ = c.Write(msg) // 10 bytes short
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("no answer to a stalled handshake: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// A slow handshake is not cut off once it has arrived: waiting for an engine
// may take longer than server.readHeaderTimeout.
func TestHandshakeWaitsForSlowEngineBeyondReadTimeout(t *testing.T) {
	h := newHarness(t, "server: {readHeaderTimeout: 1s}\nengine: {startupTimeout: 10s}\n", nil)
	h.prov.SetStartDelay(2 * time.Second)
	srv := httptest.NewUnstartedServer(h.gw)
	srv.Config.ReadHeaderTimeout = h.cfg.Server.ReadHeaderTimeout.Duration
	srv.Start()
	defer srv.Close()
	h.srv = srv
	h.connect("tok?user=alice", nil)
}

// Engine errors too large to inspect reach the client whole.
func TestLargeEngineErrorPassesWhole(t *testing.T) {
	h := newHarness(t, "", nil)
	c := h.connect("tok?user=alice", nil)
	body := h.query(c, "FAIL "+strings.Repeat("x", 17<<20))
	e, err := wire.DecodeErrorResponse(body)
	if err != nil {
		t.Fatalf("undecodable %d-byte error: %v", len(body), err)
	}
	if len(e.Message) < 17<<20 {
		t.Fatalf("error message cut to %d bytes", len(e.Message))
	}
	mustType(t, h.query(c, "SELECT 1"), wire.TypePrepareResponse)
}

// At shutdown, a query that still waits for its engine is answered with an
// error and cancelled on the engine: a request dropped unanswered can be sent
// again by libcurl, and the query would run twice.
func TestAbortQueriesAnswersWaitingQueries(t *testing.T) {
	h := newHarness(t, "", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	done := make(chan []byte, 1)
	go func() { done <- h.query(c, "SLEEP 5000") }()
	eventually(t, "query in flight", func() bool { return h.gw.InFlight() == 1 })
	time.Sleep(200 * time.Millisecond)
	h.gw.AbortQueries()
	select {
	case body := <-done:
		mustError(t, body, "shutting down")
	case <-time.After(3 * time.Second):
		t.Fatal("the waiting query was not answered")
	}
	eventually(t, "query cancelled on the engine", func() bool { return srv.CountType(wire.TypeCancelRequest) == 1 })
	// The session itself survives (the next gateway adopts it).
	mustType(t, h.heartbeat(c), wire.TypeSuccessResponse)
}
