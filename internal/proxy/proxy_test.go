package proxy_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"quackmux/internal/engine"
	"quackmux/internal/engine/fake"
	"quackmux/internal/wire"
)

func TestUserShareLevelRoutingAndRewrite(t *testing.T) {
	h := newHarness(t, "", nil)
	a1 := h.connect("tok?user=alice", nil)
	a2 := h.connect("tok?user=alice", nil)
	b1 := h.connect("tok?user=bob", nil)

	if h.sessionEngine(a1) != h.sessionEngine(a2) {
		t.Fatal("two sessions of alice must share the USER engine")
	}
	if h.sessionEngine(a1) == h.sessionEngine(b1) {
		t.Fatal("alice and bob must not share a USER engine")
	}
	if n := h.prov.Launches(); n != 2 {
		t.Fatalf("launches = %d, want 2", n)
	}

	mustType(t, h.query(a1, "SELECT 42"), wire.TypePrepareResponse)
	srv := h.prov.Server(h.sessionEngine(a1))
	var sawPrepare bool
	for _, r := range srv.Received() {
		switch r.Type {
		case wire.TypeConnectionRequest:
			if strings.Contains(r.AuthString, "tok") {
				t.Fatalf("client token leaked to the engine: %q", r.AuthString)
			}
			if r.AuthString != srv.Token {
				t.Fatalf("engine got auth %q, want its own token", r.AuthString)
			}
			if len(r.Headers.Values("X-Quackmux-Share-Level")) != 0 {
				t.Fatal("option headers must not be forwarded")
			}
		case wire.TypePrepareRequest:
			sawPrepare = true
			if r.ConnectionID != h.backendConn(a1) {
				t.Fatalf("engine saw connection id %q, want backend id %q", r.ConnectionID, h.backendConn(a1))
			}
			if r.SQL != "SELECT 42" {
				t.Fatalf("engine saw SQL %q", r.SQL)
			}
		}
	}
	if !sawPrepare {
		t.Fatal("engine did not receive the query")
	}
	if a1 == h.backendConn(a1) || strings.Contains(a1, h.backendConn(a1)) {
		t.Fatal("gateway id must not expose the backend connection id")
	}
}

func TestConnectionShareLevelGetsDedicatedEngine(t *testing.T) {
	h := newHarness(t, "", nil)
	c1 := h.connect("tok?user=alice&share_level=connection", nil)
	c2 := h.connect("tok?user=alice&share_level=CONNECTION", nil)
	if h.sessionEngine(c1) == h.sessionEngine(c2) {
		t.Fatal("CONNECTION share level must give every session its own engine")
	}
	e1 := h.sessionEngine(c1)
	mustType(t, h.disconnect(c1), wire.TypeSuccessResponse)
	mustError(t, h.query(c1, "SELECT 1"), "client disconnect")

	h.engines.Reconcile() // idle timeout for CONNECTION engines is 0
	eventually(t, "engine termination", func() bool { _, ok := h.engines.Get(e1); return !ok })
	if _, ok := h.engines.Get(h.sessionEngine(c2)); !ok {
		t.Fatal("the other session's engine must keep running")
	}
	mustType(t, h.query(c2, "SELECT 1"), wire.TypePrepareResponse)
}

func TestServerAndGroupShareLevels(t *testing.T) {
	h := newHarness(t, "", nil)
	a := h.connect("tok?user=alice&share_level=server", nil)
	b := h.connect("tok?user=bob&share_level=server", nil)
	if h.sessionEngine(a) != h.sessionEngine(b) {
		t.Fatal("SERVER share level must share one engine")
	}
	ga := h.connect("tok?user=alice&share_level=group&group=analytics", nil)
	gb := h.connect("tok?user=bob&share_level=group", nil) // bob's first group is analytics
	if h.sessionEngine(ga) != h.sessionEngine(gb) {
		t.Fatal("GROUP share level must share the group's engine")
	}
	e := h.connectErr("tok?user=bob&share_level=group&group=admins", nil)
	if !strings.Contains(e.Message, "not a member") {
		t.Fatalf("unexpected error %q", e.Message)
	}
	e = h.connectErr("tok?user=carol&share_level=group", nil)
	if !strings.Contains(e.Message, "has none") {
		t.Fatalf("unexpected error %q", e.Message)
	}
	// Subdomains separate engines of the same owner.
	s1 := h.connect("tok?user=alice&subdomain=etl", nil)
	s2 := h.connect("tok?user=alice&subdomain=adhoc", nil)
	if h.sessionEngine(s1) == h.sessionEngine(s2) {
		t.Fatal("different subdomains must use different engines")
	}
}

func TestOptionsFromHeaders(t *testing.T) {
	h := newHarness(t, "", nil)
	a := h.connect("tok?user=alice", map[string]string{"X-Quackmux-Share-Level": "server"})
	b := h.connect("tok?user=bob", map[string]string{"X-Quackmux-Share-Level": "SERVER"})
	if h.sessionEngine(a) != h.sessionEngine(b) {
		t.Fatal("header options must be honoured")
	}
	// Token options win over headers.
	c := h.connect("tok?user=bob&share_level=user", map[string]string{"X-Quackmux-Share-Level": "server"})
	if h.sessionEngine(c) == h.sessionEngine(a) {
		t.Fatal("token options must take precedence over headers")
	}
}

func TestHandshakeErrors(t *testing.T) {
	h := newHarness(t, "", nil)
	if e := h.connectErr("bad", nil); e.Message != "Authentication failed" {
		t.Fatalf("auth error %q", e.Message)
	}
	if e := h.connectErr("tok?user=alice&share_level=bogus", nil); !strings.Contains(e.Message, "unknown share level") {
		t.Fatalf("got %q", e.Message)
	}
	if e := h.connectErr("tok?user=alice&profile=nope", nil); !strings.Contains(e.Message, "unknown profile") {
		t.Fatalf("got %q", e.Message)
	}
	if e := h.connectErr("tok?user=alice&profile=small&share_level=server", nil); !strings.Contains(e.Message, "does not allow share level") {
		t.Fatalf("got %q", e.Message)
	}
	if e := h.connectErr("tok?user=alice&colour=blue", nil); !strings.Contains(e.Message, "unknown option") {
		t.Fatalf("got %q", e.Message)
	}
	if e := h.connectErr("tok?user=alice&subdomain=Not_Valid!", nil); !strings.Contains(e.Message, "invalid subdomain") {
		t.Fatalf("got %q", e.Message)
	}
	// Protocol version mismatch.
	msg := wire.ConnectionRequest{AuthString: "tok", MinSupportedVersion: 4, MaxSupportedVersion: 5, HeartbeatTimeoutSeconds: 60}.Encode(wire.Header{})
	mustError(t, h.post(msg, nil), "Unsupported Quack version")
	// Malformed bodies.
	mustError(t, h.post([]byte{0x01, 0x00}, nil), "Failed to deserialize")
	mustError(t, h.post(wire.EncodeEmpty(wire.Header{Type: wire.TypeSuccessResponse}), nil), "Unsupported message type")
	if h.prov.Launches() != 0 {
		t.Fatal("failed handshakes must not launch engines")
	}
}

func TestInvalidAndForgedConnectionIDs(t *testing.T) {
	h := newHarness(t, "", nil)
	good := h.connect("tok?user=alice", nil)
	for _, id := range []string{"", "deadbeef", "qmx1_AAAA", good[:len(good)-2] + "xx"} {
		e := mustError(t, h.heartbeat(id), "Invalid connection id")
		if e.MustInvalidate {
			t.Fatal("unknown ids must not invalidate")
		}
	}
	mustType(t, h.heartbeat(good), wire.TypeSuccessResponse)
}

func TestIdleTimeoutClosesSessionAndFreesBackend(t *testing.T) {
	h := newHarness(t, "", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	backend := h.backendConn(c)

	// Heartbeats keep the lease alive but do not count as activity.
	for i := 0; i < 4; i++ {
		h.clock.Advance(20 * time.Minute)
		mustType(t, h.heartbeat(c), wire.TypeSuccessResponse)
		h.sessions.Reap()
	}
	h.sessions.Wait()
	e := mustError(t, h.query(c, "SELECT 1"), "idle timeout")
	if e.MustInvalidate {
		t.Fatal("an idle timeout must not invalidate the client's database")
	}
	eventually(t, "backend disconnect", func() bool {
		for _, id := range srv.Connections() {
			if id == backend {
				return false
			}
		}
		return true
	})
	if srv.CountType(wire.TypeDisconnect) != 1 {
		t.Fatal("gateway must send DISCONNECT for an idle session")
	}
}

func TestLeaseExpiryFreesBackendConnection(t *testing.T) {
	h := newHarness(t, "", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	h.clock.Advance(59 * time.Second)
	h.sessions.Reap()
	mustType(t, h.heartbeat(c), wire.TypeSuccessResponse) // still alive

	h.clock.Advance(71 * time.Second) // 60s lease + 10s grace exceeded
	h.sessions.Reap()
	h.sessions.Wait()
	mustError(t, h.heartbeat(c), "heartbeat lease expired")
	eventually(t, "backend disconnect", func() bool { return len(srv.Connections()) == 0 })
}

func TestInFlightRequestBlocksIdleClose(t *testing.T) {
	h := newHarness(t, "", nil)
	c := h.connect("tok?user=alice", nil)
	done := make(chan []byte)
	go func() { done <- h.query(c, "SLEEP 300") }()
	eventually(t, "query in flight", func() bool { return h.gw.InFlight() == 1 })
	h.clock.Advance(2 * time.Hour)
	h.sessions.Reap()
	mustType(t, <-done, wire.TypePrepareResponse)
	// After the query finished the session is active again (lastActive = now).
	h.sessions.Reap()
	mustType(t, h.query(c, "SELECT 1"), wire.TypePrepareResponse)
}

func TestEngineCrashInvalidatesSessions(t *testing.T) {
	h := newHarness(t, "", nil)
	c := h.connect("tok?user=alice", nil)
	eid := h.sessionEngine(c)
	h.prov.Crash(eid)
	eventually(t, "engine removal", func() bool { _, ok := h.engines.Get(eid); return !ok })
	e := mustError(t, h.query(c, "SELECT 1"), "session closed")
	if !e.MustInvalidate {
		t.Fatal("a lost engine must invalidate the attached database")
	}
	// A new session gets a fresh engine.
	c2 := h.connect("tok?user=alice", nil)
	if h.sessionEngine(c2) == eid {
		t.Fatal("new session must not use the crashed engine")
	}
	mustType(t, h.query(c2, "SELECT 1"), wire.TypePrepareResponse)
}

func TestEngineInvalidationMarksEngineFailed(t *testing.T) {
	h := newHarness(t, "", nil)
	c := h.connect("tok?user=alice", nil)
	c2 := h.connect("tok?user=alice", nil)
	eid := h.sessionEngine(c)
	e := mustError(t, h.query(c, "INVALIDATE"), "database invalidated")
	if !e.MustInvalidate {
		t.Fatal("engine invalidation must be relayed")
	}
	if v, _ := h.engines.Get(eid); v.State != engine.StateFailed && v.State != engine.StateTerminating {
		t.Fatalf("engine state %s, want FAILED", v.State)
	}
	e = mustError(t, h.query(c2, "SELECT 1"), "session closed")
	if !e.MustInvalidate {
		t.Fatal("other sessions of an invalidated engine must be invalidated too")
	}
}

func TestBackendForgetsConnection(t *testing.T) {
	h := newHarness(t, "", nil)
	c := h.connect("tok?user=alice", nil)
	h.prov.Server(h.sessionEngine(c)).ExpireAll()
	mustError(t, h.query(c, "SELECT 1"), "Connection heartbeat lease expired")
	mustError(t, h.query(c, "SELECT 1"), "backend closed the connection")
}

func TestGatewayRestartAdoptsSessions(t *testing.T) {
	h1 := newHarness(t, "", nil)
	c := h1.connect("tok?user=alice", nil)
	mustType(t, h1.query(c, "SELECT 1"), wire.TypePrepareResponse)
	eid := h1.sessionEngine(c)
	srv := h1.prov.Server(eid)

	// A new gateway process: same key, engines handed over by the provider.
	prov2 := fake.New()
	prov2.Adopt(h1.prov)
	h1.cancel()
	h2 := newHarness(t, "", prov2)
	if v, ok := h2.engines.Get(eid); !ok || !v.Recovered {
		t.Fatalf("engine %s not recovered: %+v", eid, v)
	}
	mustType(t, h2.heartbeat(c), wire.TypeSuccessResponse)
	mustType(t, h2.query(c, "SELECT 2"), wire.TypePrepareResponse)
	views := h2.sessions.List("alice", "")
	if len(views) != 1 || !views[0].Adopted || views[0].EngineID != eid {
		t.Fatalf("session not adopted correctly: %+v", views)
	}
	if v, _ := h2.engines.Get(eid); v.Sessions != 1 {
		t.Fatalf("adopted session not attached to engine: %+v", v)
	}
	if got := srv.CountType(wire.TypeConnectionRequest); got != 1 {
		t.Fatalf("adoption must not reconnect, saw %d handshakes", got)
	}
	// New sessions of alice reuse the recovered engine.
	c2 := h2.connect("tok?user=alice", nil)
	if h2.sessionEngine(c2) != eid {
		t.Fatal("recovered engine must be reused")
	}
	// Recovered engines without sessions survive the recovery grace period.
	b := h2.connect("tok?user=bob", nil)
	_ = b
	if prov2.Launches() != 1 {
		t.Fatalf("launches after restart = %d, want 1 (bob only)", prov2.Launches())
	}
}

func TestRecoveryGraceProtectsIdleEngines(t *testing.T) {
	h1 := newHarness(t, "", nil)
	c := h1.connect("tok?user=alice", nil)
	eid := h1.sessionEngine(c)
	prov2 := fake.New()
	prov2.Adopt(h1.prov)
	h1.cancel()
	h2 := newHarness(t, "", prov2)
	h2.clock.Advance(31 * time.Minute) // beyond USER idle timeout, but grace runs on real time? no: manager clock
	// The recovery grace is measured with the manager clock, which started at the same instant.
	// 31 minutes exceed the grace (180s), so an engine without adopted sessions is reaped...
	h2.engines.Reconcile()
	eventually(t, "idle recovered engine terminated", func() bool { _, ok := h2.engines.Get(eid); return !ok })

	// ...whereas within the grace it is kept.
	h3 := newHarness(t, "", nil)
	c3 := h3.connect("tok?user=alice", nil)
	eid3 := h3.sessionEngine(c3)
	prov4 := fake.New()
	prov4.Adopt(h3.prov)
	h3.cancel()
	h4 := newHarness(t, `
engine:
  idleTimeout: {USER: 1s}
`, prov4)
	h4.clock.Advance(time.Minute)
	h4.engines.Reconcile()
	if _, ok := h4.engines.Get(eid3); !ok {
		t.Fatal("recovered engine must survive the recovery grace period")
	}
	mustType(t, h4.heartbeat(c3), wire.TypeSuccessResponse)
}

func TestLargeBodiesBuffered(t *testing.T) { testLargeBodies(t, "") }

func TestLargeBodiesStreamed(t *testing.T) {
	testLargeBodies(t, `
server:
  upstream:
    maxBufferedRequestBytes: -1
    maxBufferedResponseBytes: -1
`)
}

func TestTinyBufferBudgetFallsBackToStreaming(t *testing.T) {
	testLargeBodies(t, `
server:
  upstream:
    bufferBudgetBytes: 1024
`)
}

func TestRequestSizeLimit(t *testing.T) {
	h := newHarness(t, `
server:
  upstream:
    maxRequestBytes: 4096
`, nil)
	c := h.connect("tok?user=alice", nil)
	hdr := wire.Header{Type: wire.TypeSendDataRequest, ConnectionID: c}.Encode()
	msg := append(append(hdr, 0xff, 0xff), make([]byte, 8192)...)
	mustError(t, h.post(msg, nil), "exceeds the limit")
}

func testLargeBodies(t *testing.T, extra string) {
	h := newHarness(t, extra, nil)
	c := h.connect("tok?user=alice", nil)
	const size = 5 << 20
	body := h.query(c, fmt.Sprintf("BLOB %d", size))
	hdr, n, err := wire.ParseHeader(body)
	if err != nil || hdr.Type != wire.TypePrepareResponse {
		t.Fatalf("unexpected response: %+v %v", hdr, err)
	}
	if len(body) < size || !bytes.Equal(body[len(body)-1024:], bytes.Repeat([]byte{0xab}, 1024)) {
		t.Fatalf("blob truncated or corrupted: %d bytes after header %d", len(body), n)
	}

	// A large SEND_DATA request reaches the engine byte for byte (apart from the header).
	blob := bytes.Repeat([]byte{0x5a}, 3<<20)
	hdrBytes := wire.Header{Type: wire.TypeSendDataRequest, ConnectionID: c}.Encode()
	msg := append(append(hdrBytes, 0xff, 0xff), blob...) // empty body object + blob
	mustType(t, h.post(msg, nil), wire.TypeSendDataResponse)
	srv := h.prov.Server(h.sessionEngine(c))
	var got int
	for _, r := range srv.Received() {
		if r.Type == wire.TypeSendDataRequest {
			got = r.BodyLen
		}
	}
	backendHdr := wire.Header{Type: wire.TypeSendDataRequest, ConnectionID: h.backendConn(c)}.Encode()
	if want := len(backendHdr) + 2 + len(blob); got != want {
		t.Fatalf("engine received %d bytes, want %d", got, want)
	}
}

func TestChunkedRequestBody(t *testing.T) {
	h := newHarness(t, "", nil)
	c := h.connect("tok?user=alice", nil)
	msg := wire.PrepareRequest{SQL: "SELECT 'chunked'", QueryUUID: wire.Hugeint{Lower: 1}}.Encode(wire.Header{ConnectionID: c})
	// io.MultiReader hides the length, so the client uses chunked encoding.
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/quack", io.MultiReader(bytes.NewReader(msg)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	mustType(t, body, wire.TypePrepareResponse)
}

func TestConcurrentRequestsOnOneSession(t *testing.T) {
	h := newHarness(t, "", nil)
	c := h.connect("tok?user=alice", nil)
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var body []byte
			if i%2 == 0 {
				body = h.heartbeat(c)
			} else {
				body = h.query(c, "SLEEP 20")
			}
			if hdr, _, err := wire.ParseHeader(body); err != nil || hdr.Type == wire.TypeErrorResponse {
				errs <- fmt.Sprintf("request %d failed", i)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	eventually(t, "in-flight counter back to zero", func() bool {
		v, _ := h.engines.Get(h.sessionEngine(c))
		return v.InFlight == 0 && h.gw.InFlight() == 0
	})
	views := h.sessions.List("alice", "")
	if len(views) != 1 || views[0].InFlight != 0 || views[0].Requests != 32 {
		t.Fatalf("session accounting wrong: %+v", views)
	}
}

func TestConcurrentConnectsShareOneLaunch(t *testing.T) {
	prov := fake.New()
	prov.StartDelay = 200 * time.Millisecond
	h := newHarness(t, "", prov)
	var wg sync.WaitGroup
	ids := make([]string, 10)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i] = h.connect("tok?user=alice", nil)
		}(i)
	}
	wg.Wait()
	if prov.Launches() != 1 {
		t.Fatalf("launches = %d, want 1", prov.Launches())
	}
	for _, id := range ids[1:] {
		if h.sessionEngine(id) != h.sessionEngine(ids[0]) {
			t.Fatal("all sessions must share the engine")
		}
	}
	if v, _ := h.engines.Get(h.sessionEngine(ids[0])); v.Sessions != 10 || v.Reserved != 0 {
		t.Fatalf("engine accounting wrong: %+v", v)
	}
}

func TestLimits(t *testing.T) {
	h := newHarness(t, `
limits:
  maxSessionsPerUser: 2
  maxEnginesPerUser: 1
`, nil)
	h.connect("tok?user=alice", nil)
	h.connect("tok?user=alice", nil)
	if e := h.connectErr("tok?user=alice", nil); !strings.Contains(e.Message, "session limit") {
		t.Fatalf("got %q", e.Message)
	}
	// Closing a session frees the slot.
	h.connect("tok?user=bob", nil)
}

func TestEngineLimitPerUser(t *testing.T) {
	h := newHarness(t, `
limits:
  maxEnginesPerUser: 1
`, nil)
	h.connect("tok?user=bob&share_level=connection", nil)
	if e := h.connectErr("tok?user=bob&share_level=connection", nil); !strings.Contains(e.Message, "maxEnginesPerUser") {
		t.Fatalf("got %q", e.Message)
	}
}

func TestStartupTimeout(t *testing.T) {
	prov := fake.New()
	prov.NeverReady = true
	h := newHarness(t, `
engine:
  startupTimeout: 1s
`, prov)
	start := time.Now()
	e := h.connectErr("tok?user=alice", nil)
	if !strings.Contains(e.Message, "did not become ready") {
		t.Fatalf("got %q", e.Message)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("startup timeout not enforced")
	}
}

func TestLaunchFailure(t *testing.T) {
	prov := fake.New()
	prov.FailLaunch = fmt.Errorf("quota exceeded")
	h := newHarness(t, "", prov)
	e := h.connectErr("tok?user=alice", nil)
	if !strings.Contains(e.Message, "quota exceeded") {
		t.Fatalf("got %q", e.Message)
	}
}

func TestUserEngineIdleTermination(t *testing.T) {
	h := newHarness(t, "", nil)
	c := h.connect("tok?user=alice", nil)
	eid := h.sessionEngine(c)
	mustType(t, h.disconnect(c), wire.TypeSuccessResponse)
	h.clock.Advance(29 * time.Minute)
	h.engines.Reconcile()
	if _, ok := h.engines.Get(eid); !ok {
		t.Fatal("engine terminated before its idle timeout")
	}
	h.clock.Advance(2 * time.Minute)
	h.engines.Reconcile()
	eventually(t, "idle engine termination", func() bool { _, ok := h.engines.Get(eid); return !ok })
}

func TestMaxLifetimeDrainsEngine(t *testing.T) {
	h := newHarness(t, `
engine:
  maxLifetime: 1h
  drainTimeout: 30m
`, nil)
	old := h.connect("tok?user=alice", nil)
	oldEngine := h.sessionEngine(old)
	h.clock.Advance(61 * time.Minute)
	mustType(t, h.heartbeat(old), wire.TypeSuccessResponse)
	h.engines.Reconcile()
	if v, _ := h.engines.Get(oldEngine); v.State != engine.StateDraining {
		t.Fatalf("state %s, want DRAINING", v.State)
	}
	// New sessions go to a new engine, the old session keeps working.
	fresh := h.connect("tok?user=alice", nil)
	if h.sessionEngine(fresh) == oldEngine {
		t.Fatal("draining engine must not take new sessions")
	}
	mustType(t, h.query(old, "SELECT 1"), wire.TypePrepareResponse)
	// After the drain timeout the old session is closed and the engine goes.
	h.clock.Advance(31 * time.Minute)
	mustType(t, h.heartbeat(fresh), wire.TypeSuccessResponse)
	h.engines.Reconcile()
	h.sessions.Wait()
	e := mustError(t, h.query(old, "SELECT 1"), "drain timeout")
	if !e.MustInvalidate {
		t.Fatal("drain-closed sessions must invalidate")
	}
	h.engines.Reconcile()
	eventually(t, "drained engine termination", func() bool { _, ok := h.engines.Get(oldEngine); return !ok })
}

func TestPoolPollingSpreadsSessions(t *testing.T) {
	h := newHarness(t, `
profiles:
  default:
    pool: {size: 2, selectPolicy: POLLING}
  small:
    allowedShareLevels: [USER]
`, nil)
	a := h.connect("tok?user=alice&share_level=server", nil)
	b := h.connect("tok?user=bob&share_level=server", nil)
	c := h.connect("tok?user=carol&share_level=server", nil)
	if h.sessionEngine(a) == h.sessionEngine(b) {
		t.Fatal("POLLING must alternate between pool engines")
	}
	if h.sessionEngine(a) != h.sessionEngine(c) {
		t.Fatal("pool of two must wrap around")
	}
}

func TestWarmPoolIsClaimedByConnectionSessions(t *testing.T) {
	h := newHarness(t, `
profiles:
  default:
    warmPool: {size: 1}
  small:
    allowedShareLevels: [USER]
`, nil)
	h.engines.Reconcile()
	eventually(t, "warm engine ready", func() bool {
		for _, v := range h.engines.List() {
			if v.Warm && v.State == engine.StateReady {
				return true
			}
		}
		return false
	})
	if h.prov.Launches() != 1 {
		t.Fatalf("launches = %d, want 1 warm engine", h.prov.Launches())
	}
	c := h.connect("tok?user=alice&share_level=connection", nil)
	if h.prov.Launches() != 1 {
		t.Fatal("CONNECTION session must claim the warm engine instead of launching")
	}
	v, _ := h.engines.Get(h.sessionEngine(c))
	if v.Warm || v.User != "alice" || v.ShareLevel != "CONNECTION" {
		t.Fatalf("claimed engine not re-keyed: %+v", v)
	}
	h.engines.Reconcile() // refill (launches run asynchronously)
	eventually(t, "warm pool refill", func() bool { return h.prov.Launches() == 2 })
	// USER sessions never use warm engines.
	u := h.connect("tok?user=bob", nil)
	if w, _ := h.engines.Get(h.sessionEngine(u)); w.Warm {
		t.Fatal("USER session got a warm engine")
	}
}

func TestPermanentEnginesAreNeverReaped(t *testing.T) {
	prov := fake.New()
	prov.Permanent = true
	key := engine.Key{ShareLevel: engine.ShareServer, Subdomain: "default", Profile: "default"}
	srv := prov.AddReady("static-shared", key, "static-token-1234")
	h := newHarness(t, "", prov)

	c := h.connect("tok?user=alice&share_level=server", nil)
	if h.sessionEngine(c) != "static-shared" {
		t.Fatal("session must use the static engine")
	}
	if srv.CountType(wire.TypeConnectionRequest) != 1 {
		t.Fatal("handshake not forwarded")
	}
	// No engine for USER share level and no way to launch one.
	if e := h.connectErr("tok?user=alice", nil); !strings.Contains(e.Message, "cannot launch") {
		t.Fatalf("got %q", e.Message)
	}
	mustType(t, h.disconnect(c), wire.TypeSuccessResponse)
	h.clock.Advance(24 * time.Hour)
	h.engines.Reconcile()
	if v, ok := h.engines.Get("static-shared"); !ok || v.State != engine.StateReady {
		t.Fatalf("static engine must stay ready: %+v", v)
	}
	if err := h.engines.Terminate("static-shared", "admin"); err == nil {
		t.Fatal("static engines must not be terminable")
	}
	// A failed probe takes it out of service, a good probe brings it back.
	c2 := h.connect("tok?user=bob&share_level=server", nil)
	prov.Emit("static-shared", engine.PhaseFailed, "probe failed")
	eventually(t, "engine failed", func() bool { v, _ := h.engines.Get("static-shared"); return v.State == engine.StateFailed })
	mustError(t, h.query(c2, "SELECT 1"), "session closed")
	prov.Emit("static-shared", engine.PhaseReady, "")
	eventually(t, "engine recovered", func() bool { v, _ := h.engines.Get("static-shared"); return v.State == engine.StateReady })
	h.clock.Advance(time.Hour)
	h.engines.Reconcile()
	c3 := h.connect("tok?user=bob&share_level=server", nil)
	mustType(t, h.query(c3, "SELECT 1"), wire.TypePrepareResponse)
}

func TestHeartbeatsAreAnsweredByTheGateway(t *testing.T) {
	h := newHarness(t, "", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	for i := 0; i < 5; i++ {
		mustType(t, h.heartbeat(c), wire.TypeSuccessResponse)
	}
	if n := srv.CountType(wire.TypeHeartbeatRequest); n != 0 {
		t.Fatalf("%d client heartbeats reached the engine", n)
	}
	// Quiet sessions get their engine lease renewed by the gateway.
	for i := 0; i < 3; i++ {
		h.clock.Advance(10 * time.Minute)
		mustType(t, h.heartbeat(c), wire.TypeSuccessResponse)
		h.sessions.Reap()
	}
	h.sessions.Wait()
	if n := srv.CountType(wire.TypeHeartbeatRequest); n == 0 {
		t.Fatal("gateway did not keep the engine lease alive")
	}
	// Heartbeats of closed sessions fail like any other request.
	mustType(t, h.disconnect(c), wire.TypeSuccessResponse)
	mustError(t, h.heartbeat(c), "client disconnect")
}

func TestDisconnectIsAcknowledgedImmediately(t *testing.T) {
	h := newHarness(t, "", nil)
	c := h.connect("tok?user=alice", nil)
	srv := h.prov.Server(h.sessionEngine(c))
	backend := h.backendConn(c)
	// A long query keeps the engine connection busy; DISCONNECT must not wait for it.
	go h.query(c, "SLEEP 500")
	eventually(t, "query in flight", func() bool { return h.gw.InFlight() == 1 })
	start := time.Now()
	mustType(t, h.disconnect(c), wire.TypeSuccessResponse)
	if time.Since(start) > 300*time.Millisecond {
		t.Fatalf("DISCONNECT took %s", time.Since(start))
	}
	h.sessions.Wait()
	eventually(t, "engine connection closed", func() bool {
		for _, id := range srv.Connections() {
			if id == backend {
				return false
			}
		}
		return true
	})
}

func TestEngineGetsGatewayLease(t *testing.T) {
	h := newHarness(t, "", nil)
	c := h.connect("tok?user=alice", nil) // client asks for 60s and gets 60s (see connect)
	if v := h.sessions.List("alice", ""); len(v) != 1 || v[0].HeartbeatTimeout != "1m0s" {
		t.Fatalf("client lease not recorded: %+v", v)
	}
	leases := h.prov.Server(h.sessionEngine(c)).HeartbeatTimeouts()
	if len(leases) != 1 || leases[0] != time.Hour {
		t.Fatalf("engine lease %v, want the gateway's 1h", leases)
	}
}

func TestBannerAndCORS(t *testing.T) {
	h := newHarness(t, "", nil)
	resp, err := http.Get(h.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "quackmux") {
		t.Fatalf("banner %q", b)
	}
	req, _ := http.NewRequest(http.MethodOptions, h.srv.URL+"/quack", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("CORS preflight: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestAdminCloseAndCancel(t *testing.T) {
	h := newHarness(t, "", nil)
	c := h.connect("tok?user=alice", nil)
	views := h.sessions.List("alice", "")
	if len(views) != 1 {
		t.Fatal("expected one session")
	}
	if err := h.sessions.Cancel(context.Background(), views[0].ID); err != nil {
		t.Fatal(err)
	}
	srv := h.prov.Server(h.sessionEngine(c))
	if srv.CountType(wire.TypeCancelRequest) != 1 {
		t.Fatal("cancel not forwarded")
	}
}
