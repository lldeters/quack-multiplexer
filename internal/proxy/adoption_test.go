package proxy_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"quackmux/internal/engine"
	"quackmux/internal/engine/fake"
	"quackmux/internal/session"
	"quackmux/internal/wire"
)

// A session the gateway closed must stay closed when its tombstone expires,
// although the DuckDB client keeps heartbeating while attached.
func TestClosedSessionStaysClosedAfterTombstoneTTL(t *testing.T) {
	h := newHarness(t, "limits: {maxSessionsPerUser: 1}\n", nil)
	c := h.connect("tok?user=alice", nil)
	eid := h.sessionEngine(c)
	views := h.sessions.List("alice", "")
	if !h.sessions.Close(views[0].ID, "admin: closed by operator", session.CloseMode{Disconnect: true}) {
		t.Fatal("close refused")
	}
	h.sessions.Wait()
	mustError(t, h.heartbeat(c), "closed by operator")
	for i := 0; i < 62; i++ { // beyond the tombstone TTL (1h)
		h.clock.Advance(time.Minute)
		_ = h.heartbeat(c)
		h.sessions.Reap()
	}
	if body := h.heartbeat(c); respType(t, body) == wire.TypeSuccessResponse {
		t.Fatal("the closed session is open again")
	}
	if n := h.sessions.Count(); n != 0 {
		t.Fatalf("%d open sessions, want 0", n)
	}
	if v, _ := h.engines.Get(eid); v.Sessions != 0 {
		t.Fatalf("engine counts %d sessions for a closed session", v.Sessions)
	}
	h.connect("tok?user=alice", nil) // her only slot is free
}

// After a restart (tombstones are lost) a session that was closed and
// disconnected before is not adopted; a live one is.
func TestRestartAdoptsOnlyLiveSessions(t *testing.T) {
	h1 := newHarness(t, "", nil)
	closed := h1.connect("tok?user=alice", nil)
	live := h1.connect("tok?user=alice", nil)
	if !h1.sessions.Close(sessionIDOf(t, h1, closed), "idle timeout (6h0m0s)", session.CloseMode{Disconnect: true}) {
		t.Fatal("close refused")
	}
	h1.sessions.Wait()

	prov2 := fake.New()
	prov2.Adopt(h1.prov)
	h1.cancel()
	h2 := newHarness(t, "", prov2)
	mustError(t, h2.heartbeat(closed), "no longer knows this connection")
	mustType(t, h2.heartbeat(live), wire.TypeSuccessResponse)
	if n := h2.sessions.Count(); n != 1 {
		t.Fatalf("%d sessions after the restart, want only the live one", n)
	}
}

func sessionIDOf(t *testing.T, h *harness, connID string) string {
	t.Helper()
	r, err := h.sealer.Open(connID)
	if err != nil {
		t.Fatal(err)
	}
	return r.SessionID
}

// A query error whose text merely contains a server message about unknown
// connections must not end the session.
func TestQueryErrorWithConnectionTextKeepsSession(t *testing.T) {
	h := newHarness(t, "", nil)
	c := h.connect("tok?user=alice", nil)
	mustError(t, h.query(c, "FAIL Conversion Error: Could not convert string 'Invalid connection id' to INT32"), "Conversion Error")
	h.sessions.Wait()
	mustType(t, h.query(c, "SELECT 1"), wire.TypePrepareResponse)
}

// listEndpoint rewrites the endpoints the fake provider lists (engines found
// at startup).
type listEndpoint struct {
	*fake.Provider
	endpoint string
}

func (p *listEndpoint) List(ctx context.Context) ([]engine.Instance, error) {
	out, err := p.Provider.List(ctx)
	for i := range out {
		out[i].Endpoint = p.endpoint
	}
	return out, err
}

// A handshake with an engine that accepts connections but never answers ends
// after server.upstream.handshakeTimeout with an error, and gives the engine
// reservation back.
func TestHandshakeWithHungEngineTimesOut(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, conn) }() // read, never answer
		}
	}()
	inner := fake.New()
	key := engine.Key{ShareLevel: engine.ShareUser, Owner: "alice", Subdomain: "default", Profile: "default"}
	inner.AddReady("hung", key, "tok-hung")
	h := newHarnessWith(t, "server: {upstream: {handshakeTimeout: 1s}}\n", inner,
		&listEndpoint{Provider: inner, endpoint: "http://" + ln.Addr().String()})

	start := time.Now()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/quack", bytes.NewReader(connectMsg("tok?user=alice")))
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(mustError(t, body, "unreachable").Message, "hung") {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the handshake took %s", d)
	}
	if v, _ := h.engines.Get("hung"); v.Reserved != 0 {
		t.Fatalf("engine still reserved %d times", v.Reserved)
	}
}
