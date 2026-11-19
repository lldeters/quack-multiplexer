package proxy_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"quackmux/internal/auth"
	"quackmux/internal/config"
	"quackmux/internal/engine"
	"quackmux/internal/engine/fake"
	"quackmux/internal/metrics"
	"quackmux/internal/proxy"
	"quackmux/internal/session"
	"quackmux/internal/upstream"
	"quackmux/internal/wire"

	"sigs.k8s.io/yaml"
)

// clock is a manually advanced clock shared by the managers under test.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// noneAuth accepts every token; the user comes from the "user" option.
type noneAuth struct{ groups map[string][]string }

func (a noneAuth) Authenticate(_ context.Context, r auth.Request) (auth.Identity, error) {
	if r.Credential == "bad" {
		return auth.Identity{}, auth.ErrUnauthenticated
	}
	u := r.Options.Get("user")
	if u == "" {
		u = "anonymous"
	}
	return auth.Identity{User: u, Groups: a.groups[u], Method: "test"}, nil
}

func (noneAuth) Close() error { return nil }

type harness struct {
	t        *testing.T
	cfg      *config.Config
	prov     *fake.Provider
	engines  *engine.Manager
	sessions *session.Manager
	gw       *proxy.Gateway
	srv      *httptest.Server
	clock    *clock
	cancel   context.CancelFunc
	sealer   *session.Sealer
}

const baseConfig = `
auth:
  mode: none
provider:
  type: kubernetes
session:
  idleTimeout: 1h
  heartbeatGrace: 10s
engine:
  startupTimeout: 5s
  failedRetention: 0s
profiles:
  default: {}
  small:
    allowedShareLevels: [USER]
`

var testKey = []byte("0123456789abcdef0123456789abcdef")

// mergeYAML deep-merges extra over base (maps merge, everything else is replaced).
func mergeYAML(t *testing.T, base, extra string) []byte {
	t.Helper()
	var b, e map[string]any
	if err := yaml.Unmarshal([]byte(base), &b); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(extra), &e); err != nil {
		t.Fatal(err)
	}
	var merge func(dst, src map[string]any)
	merge = func(dst, src map[string]any) {
		for k, v := range src {
			if sm, ok := v.(map[string]any); ok {
				if dm, ok := dst[k].(map[string]any); ok {
					merge(dm, sm)
					continue
				}
			}
			dst[k] = v
		}
	}
	if b == nil {
		b = map[string]any{}
	}
	merge(b, e)
	out, err := yaml.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func newHarness(t *testing.T, yamlExtra string, prov *fake.Provider) *harness {
	t.Helper()
	cfg, err := config.Parse(mergeYAML(t, baseConfig, yamlExtra))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if prov == nil {
		prov = fake.New()
	}
	h := &harness{t: t, cfg: cfg, prov: prov, clock: newClock()}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	met := metrics.New()
	up := upstream.New(cfg.Server.Upstream)
	h.sealer, err = session.NewSealer(testKey)
	if err != nil {
		t.Fatal(err)
	}
	h.engines = engine.NewManager(cfg, prov, log, met)
	h.engines.SetClock(h.clock.Now)
	h.sessions = session.NewManager(cfg, h.engines, up, h.sealer, log, met)
	h.sessions.SetClock(h.clock.Now)
	h.engines.SetSessionCloser(h.sessions)
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	if err := h.engines.Start(ctx); err != nil {
		t.Fatal(err)
	}
	h.gw = proxy.New(proxy.Options{
		Config: cfg, Auth: noneAuth{groups: map[string][]string{"alice": {"analytics", "admins"}, "bob": {"analytics"}}},
		Engines: h.engines, Sessions: h.sessions, Upstream: up, Log: log, Metrics: met,
	})
	h.gw.SetClock(h.clock.Now)
	h.srv = httptest.NewServer(h.gw)
	t.Cleanup(func() {
		h.srv.Close()
		cancel()
		h.sessions.Wait()
		for _, id := range prov.IDs() {
			_ = prov.Terminate(context.Background(), id, "test cleanup")
		}
	})
	return h
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// post sends a raw Quack message to the gateway and returns the response body.
func (h *harness) post(msg []byte, headers map[string]string) []byte {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/quack", bytes.NewReader(msg))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", wire.ContentType)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return body
}

func connectMsg(token string) []byte {
	return wire.ConnectionRequest{
		AuthString: token, ClientDuckDBVersion: "v2.0.0", ClientPlatform: "test_platform",
		MinSupportedVersion: 3, MaxSupportedVersion: 3, ClientID: "client-1234", HeartbeatTimeoutSeconds: 60,
	}.Encode(wire.Header{})
}

// connect performs the handshake and returns the gateway connection id.
func (h *harness) connect(token string, headers map[string]string) string {
	h.t.Helper()
	body := h.post(connectMsg(token), headers)
	hdr, resp, err := wire.DecodeConnectionResponse(body)
	if err != nil {
		if e, derr := wire.DecodeErrorResponse(body); derr == nil {
			h.t.Fatalf("connect failed: %s", e.Message)
		}
		h.t.Fatalf("decode connection response: %v", err)
	}
	if resp.QuackVersion != wire.ProtocolVersion || resp.HeartbeatTimeoutSeconds != 60 {
		h.t.Fatalf("unexpected handshake answer %+v", resp)
	}
	if !strings.HasPrefix(hdr.ConnectionID, "qm") || len(hdr.ConnectionID) > session.MaxIDLength {
		h.t.Fatalf("connection id %q is not a gateway id (or too long: %d)", hdr.ConnectionID, len(hdr.ConnectionID))
	}
	return hdr.ConnectionID
}

// connectErr performs a handshake that must fail and returns the error.
func (h *harness) connectErr(token string, headers map[string]string) wire.ErrorResponse {
	h.t.Helper()
	body := h.post(connectMsg(token), headers)
	e, err := wire.DecodeErrorResponse(body)
	if err != nil {
		h.t.Fatalf("expected an error response, got %x (%v)", body[:min(len(body), 32)], err)
	}
	return e
}

func (h *harness) query(connID, sql string) []byte {
	h.t.Helper()
	return h.post(wire.PrepareRequest{SQL: sql, QueryUUID: wire.Hugeint{Upper: 7, Lower: 9}}.Encode(wire.Header{ConnectionID: connID}), nil)
}

func (h *harness) heartbeat(connID string) []byte {
	h.t.Helper()
	return h.post(wire.EncodeEmpty(wire.Header{Type: wire.TypeHeartbeatRequest, ConnectionID: connID}), nil)
}

func (h *harness) disconnect(connID string) []byte {
	h.t.Helper()
	return h.post(wire.EncodeEmpty(wire.Header{Type: wire.TypeDisconnect, ConnectionID: connID}), nil)
}

func respType(t *testing.T, body []byte) wire.MessageType {
	t.Helper()
	hdr, _, err := wire.ParseHeader(body)
	if err != nil {
		t.Fatalf("undecodable response: %v", err)
	}
	return hdr.Type
}

func mustType(t *testing.T, body []byte, want wire.MessageType) {
	t.Helper()
	if got := respType(t, body); got != want {
		msg := ""
		if got == wire.TypeErrorResponse {
			e, _ := wire.DecodeErrorResponse(body)
			msg = e.Message
		}
		t.Fatalf("response type %s, want %s %s", got, want, msg)
	}
}

func mustError(t *testing.T, body []byte, contains string) wire.ErrorResponse {
	t.Helper()
	e, err := wire.DecodeErrorResponse(body)
	if err != nil {
		t.Fatalf("expected an error response: %v", err)
	}
	if !strings.Contains(e.Message, contains) {
		t.Fatalf("error %q does not contain %q", e.Message, contains)
	}
	return e
}

// sessionEngine returns the engine id behind a gateway connection id.
func (h *harness) sessionEngine(connID string) string {
	h.t.Helper()
	r, err := h.sealer.Open(connID)
	if err != nil {
		h.t.Fatal(err)
	}
	id, ok := h.engines.IDByUID(r.EngineUID)
	if !ok {
		for _, v := range h.engines.List() {
			if engine.UIDOf(v.ID) == r.EngineUID {
				return v.ID
			}
		}
		// The engine is gone: identify it through the provider's history.
		for _, id := range h.prov.IDs() {
			if engine.UIDOf(id) == r.EngineUID {
				return id
			}
		}
		h.t.Fatalf("no engine for connection id")
	}
	return id
}

func (h *harness) backendConn(connID string) string {
	h.t.Helper()
	r, err := h.sealer.Open(connID)
	if err != nil {
		h.t.Fatal(err)
	}
	return r.BackendConnID
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
