package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"quackmux/internal/engine"
	"quackmux/internal/metrics"
	"quackmux/internal/session"
)

// ---- fakes ----------------------------------------------------------------

type closeCall struct {
	ID     string
	Reason string
	Mode   session.CloseMode
}

type fakeSessions struct {
	mu        sync.Mutex
	views     map[string]session.View
	closes    []closeCall
	cancels   []string
	cancelErr error
	// vanishOnClose makes Close fail because the session was closed
	// concurrently by somebody else.
	vanishOnClose bool
}

func (f *fakeSessions) List(user, engineID string) []session.View {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []session.View
	for _, v := range f.views {
		if (user == "" || v.User == user) && (engineID == "" || v.EngineID == engineID) {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (f *fakeSessions) Get(id string) (session.View, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.views[id]
	return v, ok
}

func (f *fakeSessions) Close(id, reason string, mode session.CloseMode) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes = append(f.closes, closeCall{ID: id, Reason: reason, Mode: mode})
	v, ok := f.views[id]
	if !ok {
		return false
	}
	if f.vanishOnClose {
		delete(f.views, id)
		return false
	}
	if v.InFlight > 0 && !mode.Force {
		return false
	}
	delete(f.views, id)
	return true
}

func (f *fakeSessions) Cancel(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, id)
	if f.cancelErr != nil {
		return f.cancelErr
	}
	if _, ok := f.views[id]; !ok {
		return session.ErrNotFound
	}
	return nil
}

func (f *fakeSessions) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.views)
}

func (f *fakeSessions) mutations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.closes) + len(f.cancels)
}

type engineCall struct {
	ID     string
	Reason string
}

type fakeEngines struct {
	mu           sync.Mutex
	views        map[string]engine.View
	drains       []engineCall
	terminates   []engineCall
	drainErr     error
	terminateErr error
	// removeOnTerminate drops the engine at once (an idle engine that is
	// gone before the response is written).
	removeOnTerminate bool
}

func (f *fakeEngines) List() []engine.View {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]engine.View, 0, len(f.views))
	for _, v := range f.views {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (f *fakeEngines) Get(id string) (engine.View, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.views[id]
	return v, ok
}

func (f *fakeEngines) Drain(id, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drains = append(f.drains, engineCall{id, reason})
	if f.drainErr != nil {
		return f.drainErr
	}
	v, ok := f.views[id]
	if !ok {
		return errors.New("engine not found")
	}
	v.State = engine.StateDraining
	v.Message = "draining: " + reason
	f.views[id] = v
	return nil
}

func (f *fakeEngines) Terminate(id, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminates = append(f.terminates, engineCall{id, reason})
	if f.terminateErr != nil {
		return f.terminateErr
	}
	v, ok := f.views[id]
	if !ok {
		return errors.New("engine not found")
	}
	if f.removeOnTerminate {
		delete(f.views, id)
		return nil
	}
	v.State = engine.StateTerminating
	v.Message = reason
	f.views[id] = v
	return nil
}

func (f *fakeEngines) mutations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.drains) + len(f.terminates)
}

// ---- fixture --------------------------------------------------------------

var t0 = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

func sampleSessions() map[string]session.View {
	mk := func(id, user, eng string, inFlight int) session.View {
		return session.View{
			ID: id, User: user, State: session.StateOpen, EngineID: eng, EngineKey: "USER/" + user + "/default/default#0",
			ShareLevel: "USER", Subdomain: "default", Profile: "default", HeartbeatTimeout: "1m0s",
			CreatedAt: t0, LastSeen: t0, LastActive: t0, InFlight: inFlight,
		}
	}
	return map[string]session.View{
		"s1":     mk("s1", "alice", "e1", 0),
		"s2":     mk("s2", "bob", "e1", 0),
		"s3":     mk("s3", "alice", "e2", 0),
		"busy":   mk("busy", "carol", "e2", 2),
		"orphan": mk("orphan", "dave", "gone", 0),
	}
}

func sampleEngines() map[string]engine.View {
	return map[string]engine.View{
		"e1": {ID: "e1", Key: "USER/alice/default/default#0", ShareLevel: "USER", Owner: "alice", Subdomain: "default",
			Profile: "default", State: engine.StateReady, Endpoint: "http://10.0.0.1:9494", CreatedAt: t0, StateSince: t0,
			LastUsed: t0, Sessions: 2, SpecCurrent: true},
		"e2": {ID: "e2", Key: "SERVER//default/default#0", ShareLevel: "SERVER", Subdomain: "default", Profile: "default",
			State: engine.StateDraining, CreatedAt: t0, StateSince: t0, LastUsed: t0, Sessions: 2},
		"w1": {ID: "w1", Key: "CONNECTION//default/default#0", ShareLevel: "CONNECTION", Profile: "default", Warm: true,
			State: engine.StateReady, CreatedAt: t0, StateSince: t0, LastUsed: t0},
	}
}

type fixture struct {
	sessions *fakeSessions
	engines  *fakeEngines
	srv      *Server
}

func newFixture(t *testing.T, mutate ...func(*Options)) *fixture {
	t.Helper()
	fx := &fixture{
		sessions: &fakeSessions{views: sampleSessions()},
		engines:  &fakeEngines{views: sampleEngines()},
	}
	o := Options{
		Sessions: fx.sessions,
		Engines:  fx.engines,
		Log:      slog.New(slog.DiscardHandler),
		Version:  "1.2.3-test",
	}
	for _, m := range mutate {
		m(&o)
	}
	fx.srv = New(o)
	return fx
}

type response struct {
	status int
	header http.Header
	body   []byte
}

// do sends a request through the handler. auth is the raw Authorization
// header ("" = none).
func do(t *testing.T, h http.Handler, method, target, auth string) response {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return response{status: res.StatusCode, header: res.Header, body: body}
}

func decode[T any](t *testing.T, body []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode %T: %v\nbody: %s", v, err, body)
	}
	return v
}

func keys(t *testing.T, body []byte) []string {
	t.Helper()
	m := decode[map[string]json.RawMessage](t, body)
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func errorMessage(t *testing.T, body []byte) string {
	t.Helper()
	e := decode[map[string]string](t, body)
	msg, ok := e["error"]
	if !ok || msg == "" {
		t.Fatalf("error response without message: %s", body)
	}
	return msg
}

func assertJSON(t *testing.T, r response) {
	t.Helper()
	if ct := r.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if !json.Valid(r.body) {
		t.Errorf("body is not valid JSON: %s", r.body)
	}
}

// ---- tests ------------------------------------------------------------------

func TestAuthentication(t *testing.T) {
	const secret = "s3cret-token"
	tests := []struct {
		name       string
		token      string // configured token
		method     string
		path       string
		auth       string // Authorization header sent
		wantStatus int
		wantChalle string // expected WWW-Authenticate ("" = must be absent)
	}{
		{"open API without configured token", "", "GET", "/api/v1/info", "", 200, ""},
		{"open API ignores credentials", "", "GET", "/api/v1/info", "Bearer whatever", 200, ""},
		{"missing token", secret, "GET", "/api/v1/info", "", 401, `Bearer realm="quackmux-admin"`},
		{"wrong token", secret, "GET", "/api/v1/sessions", "Bearer nope", 401, `Bearer realm="quackmux-admin", error="invalid_token"`},
		{"token prefix", secret, "GET", "/api/v1/sessions", "Bearer s3cret", 401, `Bearer realm="quackmux-admin", error="invalid_token"`},
		{"token with suffix", secret, "GET", "/api/v1/sessions", "Bearer " + secret + "x", 401, `Bearer realm="quackmux-admin", error="invalid_token"`},
		{"basic scheme", secret, "GET", "/api/v1/sessions", "Basic czNjcmV0LXRva2Vu", 401, `Bearer realm="quackmux-admin"`},
		{"bare token without scheme", secret, "GET", "/api/v1/sessions", secret, 401, `Bearer realm="quackmux-admin"`},
		{"empty bearer", secret, "GET", "/api/v1/sessions", "Bearer ", 401, `Bearer realm="quackmux-admin"`},
		{"correct token", secret, "GET", "/api/v1/sessions", "Bearer " + secret, 200, ""},
		{"scheme is case-insensitive", secret, "GET", "/api/v1/sessions", "bearer " + secret, 200, ""},
		{"configured token is trimmed", secret + "\n", "GET", "/api/v1/sessions", "Bearer " + secret, 200, ""},
		{"mutation needs token", secret, "DELETE", "/api/v1/sessions/s1", "", 401, `Bearer realm="quackmux-admin"`},
		{"engine mutation needs token", secret, "POST", "/api/v1/engines/e1/drain", "Bearer nope", 401, `Bearer realm="quackmux-admin", error="invalid_token"`},
		{"unknown API path needs token", secret, "GET", "/api/v2/nothing", "", 401, `Bearer realm="quackmux-admin"`},
		{"unknown API path with token", secret, "GET", "/api/v2/nothing", "Bearer " + secret, 404, ""},
		{"healthz never needs token", secret, "GET", "/healthz", "", 200, ""},
		{"readyz never needs token", secret, "GET", "/readyz", "", 200, ""},
		{"metrics never need token", secret, "GET", "/metrics", "", 200, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t, func(o *Options) {
				o.Token = tc.token
				o.Metrics = metrics.New()
			})
			r := do(t, fx.srv, tc.method, tc.path, tc.auth)
			if r.status != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", r.status, tc.wantStatus, r.body)
			}
			if got := r.header.Get("WWW-Authenticate"); got != tc.wantChalle {
				t.Errorf("WWW-Authenticate = %q, want %q", got, tc.wantChalle)
			}
			if r.status == http.StatusUnauthorized {
				assertJSON(t, r)
				errorMessage(t, r.body)
			}
			if fx.sessions.mutations()+fx.engines.mutations() != 0 && tc.wantStatus == http.StatusUnauthorized {
				t.Errorf("unauthorized request changed state")
			}
		})
	}
}

func TestProbes(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		ready      func() (bool, string)
		live       func() bool
		wantStatus int
		wantBody   string
	}{
		{"healthz default", "/healthz", nil, nil, 200, "ok"},
		{"healthz live", "/healthz", nil, func() bool { return true }, 200, "ok"},
		{"healthz not live", "/healthz", nil, func() bool { return false }, 503, "unhealthy"},
		{"healthz ignores readiness", "/healthz", func() (bool, string) { return false, "x" }, nil, 200, "ok"},
		{"readyz default", "/readyz", nil, nil, 200, "ok"},
		{"readyz ready", "/readyz", func() (bool, string) { return true, "" }, nil, 200, "ok"},
		{"readyz ready ignores reason", "/readyz", func() (bool, string) { return true, "whatever" }, nil, 200, "ok"},
		{"readyz not leader", "/readyz", func() (bool, string) { return false, "not the leader" }, nil, 503, "not the leader"},
		{"readyz without reason", "/readyz", func() (bool, string) { return false, "" }, nil, 503, "not ready"},
		{"readyz ignores liveness", "/readyz", nil, func() bool { return false }, 200, "ok"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t, func(o *Options) {
				o.Ready = tc.ready
				o.Live = tc.live
				o.Token = "secret" // probes must not need it
			})
			r := do(t, fx.srv, "GET", tc.path, "")
			if r.status != tc.wantStatus || string(r.body) != tc.wantBody {
				t.Fatalf("got %d %q, want %d %q", r.status, r.body, tc.wantStatus, tc.wantBody)
			}
			if ct := r.header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
				t.Errorf("Content-Type = %q", ct)
			}
		})
	}

	t.Run("HEAD", func(t *testing.T) {
		// (http.Server drops the body of HEAD responses; the recorder does not.)
		fx := newFixture(t)
		for _, p := range []string{"/healthz", "/readyz", "/api/v1/sessions"} {
			if r := do(t, fx.srv, "HEAD", p, ""); r.status != 200 {
				t.Errorf("HEAD %s = %d", p, r.status)
			}
		}
	})
	t.Run("POST is rejected", func(t *testing.T) {
		fx := newFixture(t)
		if r := do(t, fx.srv, "POST", "/readyz", ""); r.status != http.StatusMethodNotAllowed {
			t.Fatalf("POST /readyz = %d", r.status)
		}
	})
}

func TestMetrics(t *testing.T) {
	t.Run("exposes the registry", func(t *testing.T) {
		m := metrics.New()
		m.IsLeader.Set(1)
		m.SessionsOpen.WithLabelValues("USER", "default").Set(3)
		fx := newFixture(t, func(o *Options) { o.Metrics = m })
		r := do(t, fx.srv, "GET", "/metrics", "")
		if r.status != 200 {
			t.Fatalf("status %d: %s", r.status, r.body)
		}
		if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("Content-Type = %q", ct)
		}
		for _, want := range []string{
			"quackmux_is_leader 1",
			`quackmux_sessions{profile="default",share_level="USER"} 3`,
			"promhttp_metric_handler_requests_total",
			"go_goroutines",
		} {
			if !strings.Contains(string(r.body), want) {
				t.Errorf("metrics output lacks %q", want)
			}
		}
	})
	t.Run("several servers share one registry", func(t *testing.T) {
		m := metrics.New()
		a := newFixture(t, func(o *Options) { o.Metrics = m })
		b := newFixture(t, func(o *Options) { o.Metrics = m })
		for _, fx := range []*fixture{a, b} {
			if r := do(t, fx.srv, "GET", "/metrics", ""); r.status != 200 {
				t.Fatalf("status %d", r.status)
			}
		}
	})
	t.Run("disabled without metrics", func(t *testing.T) {
		fx := newFixture(t)
		if r := do(t, fx.srv, "GET", "/metrics", ""); r.status != http.StatusNotFound {
			t.Fatalf("status %d, want 404", r.status)
		}
	})
}

func TestSessionsAPI(t *testing.T) {
	type tcase struct {
		name       string
		method     string
		path       string
		setup      func(fx *fixture)
		wantStatus int
		check      func(t *testing.T, fx *fixture, r response)
	}
	ids := func(list []session.View) []string {
		out := make([]string, 0, len(list))
		for _, v := range list {
			out = append(out, v.ID)
		}
		return out
	}
	tests := []tcase{
		{
			name: "list all", method: "GET", path: "/api/v1/sessions", wantStatus: 200,
			check: func(t *testing.T, _ *fixture, r response) {
				if got, want := keys(t, r.body), []string{"count", "sessions"}; !reflect.DeepEqual(got, want) {
					t.Errorf("keys = %v, want %v", got, want)
				}
				l := decode[sessionList](t, r.body)
				if got, want := ids(l.Sessions), []string{"busy", "orphan", "s1", "s2", "s3"}; !reflect.DeepEqual(got, want) {
					t.Errorf("ids = %v, want %v", got, want)
				}
				if l.Count != 5 {
					t.Errorf("count = %d", l.Count)
				}
			},
		},
		{
			name: "list by user", method: "GET", path: "/api/v1/sessions?user=alice", wantStatus: 200,
			check: func(t *testing.T, _ *fixture, r response) {
				l := decode[sessionList](t, r.body)
				if got, want := ids(l.Sessions), []string{"s1", "s3"}; !reflect.DeepEqual(got, want) || l.Count != 2 {
					t.Errorf("ids = %v count %d, want %v", got, l.Count, want)
				}
			},
		},
		{
			name: "list by engine", method: "GET", path: "/api/v1/sessions?engine=e1", wantStatus: 200,
			check: func(t *testing.T, _ *fixture, r response) {
				l := decode[sessionList](t, r.body)
				if got, want := ids(l.Sessions), []string{"s1", "s2"}; !reflect.DeepEqual(got, want) {
					t.Errorf("ids = %v, want %v", got, want)
				}
			},
		},
		{
			name: "list by user and engine", method: "GET", path: "/api/v1/sessions?user=alice&engine=e2", wantStatus: 200,
			check: func(t *testing.T, _ *fixture, r response) {
				if got, want := ids(decode[sessionList](t, r.body).Sessions), []string{"s3"}; !reflect.DeepEqual(got, want) {
					t.Errorf("ids = %v, want %v", got, want)
				}
			},
		},
		{
			name: "empty list is an array", method: "GET", path: "/api/v1/sessions?user=nobody", wantStatus: 200,
			check: func(t *testing.T, _ *fixture, r response) {
				raw := decode[map[string]json.RawMessage](t, r.body)
				if string(raw["sessions"]) != "[]" || string(raw["count"]) != "0" {
					t.Errorf("body = %s", r.body)
				}
			},
		},
		{
			name: "unknown filter", method: "GET", path: "/api/v1/sessions?usr=alice", wantStatus: 400,
			check: func(t *testing.T, _ *fixture, r response) {
				if msg := errorMessage(t, r.body); !strings.Contains(msg, `"usr"`) {
					t.Errorf("error = %q", msg)
				}
			},
		},
		{name: "malformed query", method: "GET", path: "/api/v1/sessions?user=%zz", wantStatus: 400},
		{
			name: "get with engine", method: "GET", path: "/api/v1/sessions/s1", wantStatus: 200,
			check: func(t *testing.T, _ *fixture, r response) {
				if got, want := keys(t, r.body), []string{"engine", "session"}; !reflect.DeepEqual(got, want) {
					t.Errorf("keys = %v, want %v", got, want)
				}
				d := decode[sessionDetail](t, r.body)
				if d.Session.ID != "s1" || d.Session.User != "alice" || d.Session.State != session.StateOpen {
					t.Errorf("session = %+v", d.Session)
				}
				if d.Engine == nil || d.Engine.ID != "e1" || d.Engine.State != engine.StateReady {
					t.Errorf("engine = %+v", d.Engine)
				}
			},
		},
		{
			name: "get with unknown engine", method: "GET", path: "/api/v1/sessions/orphan", wantStatus: 200,
			check: func(t *testing.T, _ *fixture, r response) {
				if got, want := keys(t, r.body), []string{"session"}; !reflect.DeepEqual(got, want) {
					t.Errorf("keys = %v, want %v", got, want)
				}
			},
		},
		{
			name: "get unknown", method: "GET", path: "/api/v1/sessions/nope", wantStatus: 404,
			check: func(t *testing.T, _ *fixture, r response) {
				if msg := errorMessage(t, r.body); !strings.Contains(msg, "nope") {
					t.Errorf("error = %q", msg)
				}
			},
		},
		{name: "get with query", method: "GET", path: "/api/v1/sessions/s1?x=1", wantStatus: 400},
		{name: "trailing slash", method: "GET", path: "/api/v1/sessions/", wantStatus: 404},
		{
			name: "close idle session", method: "DELETE", path: "/api/v1/sessions/s1", wantStatus: 200,
			check: func(t *testing.T, fx *fixture, r response) {
				got := decode[sessionClosed](t, r.body)
				want := sessionClosed{ID: "s1", Closed: true, Forced: false, Reason: "admin: closed by operator"}
				if got != want {
					t.Errorf("body = %+v, want %+v", got, want)
				}
				wantCalls := []closeCall{{ID: "s1", Reason: "admin: closed by operator",
					Mode: session.CloseMode{Disconnect: true, Cancel: false, Force: false, MustInvalidate: false}}}
				if !reflect.DeepEqual(fx.sessions.closes, wantCalls) {
					t.Errorf("close calls = %+v, want %+v", fx.sessions.closes, wantCalls)
				}
				if _, ok := fx.sessions.Get("s1"); ok {
					t.Error("session still open")
				}
			},
		},
		{
			name: "close busy session without force", method: "DELETE", path: "/api/v1/sessions/busy", wantStatus: 409,
			check: func(t *testing.T, fx *fixture, r response) {
				if msg := errorMessage(t, r.body); !strings.Contains(msg, "2 request(s) in flight") || !strings.Contains(msg, "force=true") {
					t.Errorf("error = %q", msg)
				}
				if _, ok := fx.sessions.Get("busy"); !ok {
					t.Error("busy session was closed")
				}
			},
		},
		{
			name: "close busy session with force", method: "DELETE", path: "/api/v1/sessions/busy?force=true", wantStatus: 200,
			check: func(t *testing.T, fx *fixture, r response) {
				if got := decode[sessionClosed](t, r.body); !got.Closed || !got.Forced {
					t.Errorf("body = %+v", got)
				}
				want := session.CloseMode{Disconnect: true, Cancel: true, Force: true, MustInvalidate: false}
				if len(fx.sessions.closes) != 1 || fx.sessions.closes[0].Mode != want {
					t.Errorf("close calls = %+v, want mode %+v", fx.sessions.closes, want)
				}
			},
		},
		{name: "force=1 is accepted", method: "DELETE", path: "/api/v1/sessions/busy?force=1", wantStatus: 200},
		{name: "force=false keeps busy session", method: "DELETE", path: "/api/v1/sessions/busy?force=false", wantStatus: 409},
		{
			name: "invalid force", method: "DELETE", path: "/api/v1/sessions/s1?force=yes", wantStatus: 400,
			check: func(t *testing.T, fx *fixture, r response) {
				if len(fx.sessions.closes) != 0 {
					t.Error("Close called despite invalid parameter")
				}
			},
		},
		{
			name: "misspelled force", method: "DELETE", path: "/api/v1/sessions/busy?forse=true", wantStatus: 400,
			check: func(t *testing.T, fx *fixture, r response) {
				if len(fx.sessions.closes) != 0 {
					t.Error("Close called despite unknown parameter")
				}
			},
		},
		{
			name: "close unknown", method: "DELETE", path: "/api/v1/sessions/nope", wantStatus: 404,
			check: func(t *testing.T, fx *fixture, r response) {
				if len(fx.sessions.closes) != 0 {
					t.Error("Close called for unknown session")
				}
			},
		},
		{
			name: "close races with another close", method: "DELETE", path: "/api/v1/sessions/s1", wantStatus: 404,
			setup: func(fx *fixture) { fx.sessions.vanishOnClose = true },
		},
		{
			name: "cancel", method: "POST", path: "/api/v1/sessions/s1/cancel", wantStatus: 200,
			check: func(t *testing.T, fx *fixture, r response) {
				if got := decode[sessionCancelled](t, r.body); got != (sessionCancelled{ID: "s1", Cancelled: true}) {
					t.Errorf("body = %+v", got)
				}
				if !reflect.DeepEqual(fx.sessions.cancels, []string{"s1"}) {
					t.Errorf("cancel calls = %v", fx.sessions.cancels)
				}
				if _, ok := fx.sessions.Get("s1"); !ok {
					t.Error("cancel closed the session")
				}
			},
		},
		{
			name: "cancel unknown", method: "POST", path: "/api/v1/sessions/nope/cancel", wantStatus: 404,
			check: func(t *testing.T, fx *fixture, r response) {
				if len(fx.sessions.cancels) != 0 {
					t.Error("Cancel called for unknown session")
				}
			},
		},
		{
			name: "cancel races with close", method: "POST", path: "/api/v1/sessions/s1/cancel", wantStatus: 404,
			setup: func(fx *fixture) { fx.sessions.cancelErr = session.ErrNotFound },
		},
		{
			name: "cancel fails at engine", method: "POST", path: "/api/v1/sessions/s1/cancel", wantStatus: 502,
			setup: func(fx *fixture) { fx.sessions.cancelErr = errors.New("engine e1 is not available") },
			check: func(t *testing.T, _ *fixture, r response) {
				if msg := errorMessage(t, r.body); !strings.Contains(msg, "engine e1 is not available") {
					t.Errorf("error = %q", msg)
				}
			},
		},
		{
			name: "cancel times out", method: "POST", path: "/api/v1/sessions/s1/cancel", wantStatus: 504,
			setup: func(fx *fixture) { fx.sessions.cancelErr = context.DeadlineExceeded },
		},
		{name: "cancel with query", method: "POST", path: "/api/v1/sessions/s1/cancel?now=1", wantStatus: 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			if tc.setup != nil {
				tc.setup(fx)
			}
			r := do(t, fx.srv, tc.method, tc.path, "")
			if r.status != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", r.status, tc.wantStatus, r.body)
			}
			assertJSON(t, r)
			if r.status >= 400 {
				errorMessage(t, r.body)
			}
			if tc.check != nil {
				tc.check(t, fx, r)
			}
		})
	}
}

func TestEnginesAPI(t *testing.T) {
	type tcase struct {
		name       string
		method     string
		path       string
		setup      func(fx *fixture)
		wantStatus int
		check      func(t *testing.T, fx *fixture, r response)
	}
	tests := []tcase{
		{
			name: "list", method: "GET", path: "/api/v1/engines", wantStatus: 200,
			check: func(t *testing.T, _ *fixture, r response) {
				if got, want := keys(t, r.body), []string{"count", "engines"}; !reflect.DeepEqual(got, want) {
					t.Errorf("keys = %v, want %v", got, want)
				}
				l := decode[engineList](t, r.body)
				if l.Count != 3 || len(l.Engines) != 3 || l.Engines[0].ID != "e1" || l.Engines[2].ID != "w1" {
					t.Errorf("list = %+v", l)
				}
				if l.Engines[0].Endpoint != "http://10.0.0.1:9494" || l.Engines[0].Sessions != 2 {
					t.Errorf("engine e1 = %+v", l.Engines[0])
				}
			},
		},
		{name: "list with query", method: "GET", path: "/api/v1/engines?state=READY", wantStatus: 400},
		{
			name: "get with sessions", method: "GET", path: "/api/v1/engines/e1", wantStatus: 200,
			check: func(t *testing.T, _ *fixture, r response) {
				if got, want := keys(t, r.body), []string{"engine", "sessions"}; !reflect.DeepEqual(got, want) {
					t.Errorf("keys = %v, want %v", got, want)
				}
				d := decode[engineDetail](t, r.body)
				if d.Engine.ID != "e1" || d.Engine.State != engine.StateReady {
					t.Errorf("engine = %+v", d.Engine)
				}
				if len(d.Sessions) != 2 || d.Sessions[0].ID != "s1" || d.Sessions[1].ID != "s2" {
					t.Errorf("sessions = %+v", d.Sessions)
				}
			},
		},
		{
			name: "get without sessions", method: "GET", path: "/api/v1/engines/w1", wantStatus: 200,
			check: func(t *testing.T, _ *fixture, r response) {
				raw := decode[map[string]json.RawMessage](t, r.body)
				if string(raw["sessions"]) != "[]" {
					t.Errorf("sessions = %s, want []", raw["sessions"])
				}
			},
		},
		{name: "get unknown", method: "GET", path: "/api/v1/engines/nope", wantStatus: 404},
		{
			name: "drain", method: "POST", path: "/api/v1/engines/e1/drain", wantStatus: 202,
			check: func(t *testing.T, fx *fixture, r response) {
				if got, want := keys(t, r.body), []string{"engine"}; !reflect.DeepEqual(got, want) {
					t.Errorf("keys = %v, want %v", got, want)
				}
				if v := decode[engineAction](t, r.body).Engine; v.ID != "e1" || v.State != engine.StateDraining {
					t.Errorf("engine = %+v", v)
				}
				if want := []engineCall{{"e1", "admin: drained by operator"}}; !reflect.DeepEqual(fx.engines.drains, want) {
					t.Errorf("drain calls = %+v, want %+v", fx.engines.drains, want)
				}
				if len(fx.engines.terminates) != 0 {
					t.Error("drain terminated the engine")
				}
			},
		},
		{
			name: "drain unknown", method: "POST", path: "/api/v1/engines/nope/drain", wantStatus: 404,
			check: func(t *testing.T, fx *fixture, r response) {
				if len(fx.engines.drains) != 0 {
					t.Error("Drain called for unknown engine")
				}
			},
		},
		{
			name: "drain fails", method: "POST", path: "/api/v1/engines/e1/drain", wantStatus: 409,
			setup: func(fx *fixture) { fx.engines.drainErr = errors.New("not drainable") },
			check: func(t *testing.T, _ *fixture, r response) {
				if msg := errorMessage(t, r.body); !strings.Contains(msg, "not drainable") {
					t.Errorf("error = %q", msg)
				}
			},
		},
		{
			name: "drain races with removal", method: "POST", path: "/api/v1/engines/e1/drain", wantStatus: 404,
			setup: func(fx *fixture) {
				fx.engines.drainErr = errors.New(`engine "e1" not found`)
				// The engine disappears between the lookup and the drain.
				orig := fx.srv.engines
				fx.srv.engines = &vanishingEngines{Engines: orig, vanish: func() {
					fx.engines.mu.Lock()
					delete(fx.engines.views, "e1")
					fx.engines.mu.Unlock()
				}}
			},
		},
		{
			name: "terminate", method: "DELETE", path: "/api/v1/engines/e1", wantStatus: 202,
			check: func(t *testing.T, fx *fixture, r response) {
				if v := decode[engineAction](t, r.body).Engine; v.ID != "e1" || v.State != engine.StateTerminating {
					t.Errorf("engine = %+v", v)
				}
				if want := []engineCall{{"e1", "admin: terminated by operator"}}; !reflect.DeepEqual(fx.engines.terminates, want) {
					t.Errorf("terminate calls = %+v, want %+v", fx.engines.terminates, want)
				}
			},
		},
		{
			name: "terminate engine that vanishes at once", method: "DELETE", path: "/api/v1/engines/e1", wantStatus: 202,
			setup: func(fx *fixture) { fx.engines.removeOnTerminate = true },
			check: func(t *testing.T, _ *fixture, r response) {
				if v := decode[engineAction](t, r.body).Engine; v.ID != "e1" {
					t.Errorf("engine = %+v", v)
				}
			},
		},
		{
			name: "terminate unknown", method: "DELETE", path: "/api/v1/engines/nope", wantStatus: 404,
			check: func(t *testing.T, fx *fixture, r response) {
				if len(fx.engines.terminates) != 0 {
					t.Error("Terminate called for unknown engine")
				}
			},
		},
		{
			name: "terminate fails", method: "DELETE", path: "/api/v1/engines/e1", wantStatus: 409,
			setup: func(fx *fixture) { fx.engines.terminateErr = errors.New("provider refused") },
		},
		{name: "drain with query", method: "POST", path: "/api/v1/engines/e1/drain?force=true", wantStatus: 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			if tc.setup != nil {
				tc.setup(fx)
			}
			r := do(t, fx.srv, tc.method, tc.path, "")
			if r.status != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", r.status, tc.wantStatus, r.body)
			}
			assertJSON(t, r)
			if r.status >= 400 {
				errorMessage(t, r.body)
			}
			if tc.check != nil {
				tc.check(t, fx, r)
			}
		})
	}
}

// vanishingEngines runs vanish right before delegating Drain.
type vanishingEngines struct {
	Engines
	vanish func()
}

func (v *vanishingEngines) Drain(id, reason string) error {
	v.vanish()
	return v.Engines.Drain(id, reason)
}

func TestInfo(t *testing.T) {
	ready := true
	fx := newFixture(t, func(o *Options) {
		o.Ready = func() (bool, string) {
			if ready {
				return true, "ignored"
			}
			return false, "not the leader"
		}
	})
	now := t0
	fx.srv.SetClock(func() time.Time { return now })
	now = t0.Add(90 * time.Minute)

	r := do(t, fx.srv, "GET", "/api/v1/info", "")
	if r.status != 200 {
		t.Fatalf("status %d: %s", r.status, r.body)
	}
	assertJSON(t, r)
	got := decode[info](t, r.body)
	want := info{
		Version:   "1.2.3-test",
		StartedAt: t0,
		Uptime:    "1h30m0s",
		Ready:     true,
		Sessions:  sessionSummary{Total: 5, ByState: map[string]int{"OPEN": 5}},
		Engines:   engineSummary{Total: 3, ByState: map[string]int{"READY": 2, "DRAINING": 1}, Warm: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("info =\n%+v\nwant\n%+v", got, want)
	}
	if got, want := keys(t, r.body), []string{"engines", "ready", "sessions", "startedAt", "uptime", "version"}; !reflect.DeepEqual(got, want) {
		t.Errorf("keys = %v, want %v", got, want)
	}

	ready = false
	got = decode[info](t, do(t, fx.srv, "GET", "/api/v1/info", "").body)
	if got.Ready || got.ReadyReason != "not the leader" {
		t.Errorf("ready = %v %q", got.Ready, got.ReadyReason)
	}

	t.Run("default version", func(t *testing.T) {
		fx := newFixture(t, func(o *Options) { o.Version = "" })
		if got := decode[info](t, do(t, fx.srv, "GET", "/api/v1/info", "").body); got.Version != "unknown" {
			t.Errorf("version = %q", got.Version)
		}
	})
}

func TestMethodNotAllowed(t *testing.T) {
	tests := []struct {
		method, path, allow string
	}{
		{"POST", "/api/v1/info", "GET, HEAD"},
		{"POST", "/api/v1/sessions", "GET, HEAD"},
		{"DELETE", "/api/v1/sessions", "GET, HEAD"},
		{"PUT", "/api/v1/sessions/s1", "DELETE, GET, HEAD"},
		{"POST", "/api/v1/sessions/s1", "DELETE, GET, HEAD"},
		{"GET", "/api/v1/sessions/s1/cancel", "POST"},
		{"DELETE", "/api/v1/sessions/s1/cancel", "POST"},
		{"PATCH", "/api/v1/engines", "GET, HEAD"},
		{"POST", "/api/v1/engines/e1", "DELETE, GET, HEAD"},
		{"GET", "/api/v1/engines/e1/drain", "POST"},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			fx := newFixture(t)
			r := do(t, fx.srv, tc.method, tc.path, "")
			if r.status != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405", r.status)
			}
			if got := r.header.Get("Allow"); got != tc.allow {
				t.Errorf("Allow = %q, want %q", got, tc.allow)
			}
			assertJSON(t, r)
			errorMessage(t, r.body)
			if fx.sessions.mutations()+fx.engines.mutations() != 0 {
				t.Error("rejected request changed state")
			}
		})
	}
}

func TestReadOnlyGETs(t *testing.T) {
	fx := newFixture(t)
	paths := []string{
		"/api/v1/info",
		"/api/v1/sessions",
		"/api/v1/sessions?user=alice&engine=e1",
		"/api/v1/sessions/s1",
		"/api/v1/sessions/busy",
		"/api/v1/sessions/nope",
		"/api/v1/engines",
		"/api/v1/engines/e1",
		"/api/v1/engines/nope",
		"/api/v1/unknown",
		"/healthz",
		"/readyz",
	}
	for _, p := range paths {
		for _, m := range []string{"GET", "HEAD"} {
			do(t, fx.srv, m, p, "")
		}
	}
	if n := fx.sessions.mutations() + fx.engines.mutations(); n != 0 {
		t.Fatalf("GET/HEAD requests caused %d mutating calls", n)
	}
	if fx.sessions.Count() != 5 || len(fx.engines.List()) != 3 {
		t.Fatal("state changed")
	}
}

func TestNilManagers(t *testing.T) {
	srv := New(Options{})
	tests := []struct {
		method, path string
		wantStatus   int
		wantBody     string
	}{
		{"GET", "/api/v1/sessions", 200, `"sessions": []`},
		{"GET", "/api/v1/engines", 200, `"engines": []`},
		{"GET", "/api/v1/info", 200, `"total": 0`},
		{"GET", "/api/v1/sessions/x", 404, "not found"},
		{"DELETE", "/api/v1/sessions/x", 404, "not found"},
		{"POST", "/api/v1/sessions/x/cancel", 404, "not found"},
		{"GET", "/api/v1/engines/x", 404, "not found"},
		{"POST", "/api/v1/engines/x/drain", 404, "not found"},
		{"DELETE", "/api/v1/engines/x", 404, "not found"},
		{"GET", "/healthz", 200, "ok"},
		{"GET", "/readyz", 200, "ok"},
		{"GET", "/metrics", 404, ""},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			r := do(t, srv, tc.method, tc.path, "")
			if r.status != tc.wantStatus || !strings.Contains(string(r.body), tc.wantBody) {
				t.Fatalf("got %d %s, want %d containing %q", r.status, r.body, tc.wantStatus, tc.wantBody)
			}
		})
	}
}

func TestResponseHeaders(t *testing.T) {
	fx := newFixture(t)
	for _, p := range []string{"/api/v1/sessions", "/api/v1/sessions/nope", "/healthz"} {
		r := do(t, fx.srv, "GET", p, "")
		if got := r.header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q", p, got)
		}
		if got := r.header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", p, got)
		}
	}
}

func TestBearerToken(t *testing.T) {
	tests := []struct {
		header string
		want   string
		ok     bool
	}{
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true},
		{"BEARER abc", "abc", true},
		{"  Bearer   abc  ", "abc", true},
		{"Bearer", "", false},
		{"Bearer ", "", false},
		{"Basic abc", "", false},
		{"abc", "", false},
		{"", "", false},
		{"Bearer a b", "a b", true}, // compared verbatim (and rejected) later
	}
	for _, tc := range tests {
		got, ok := bearerToken(tc.header)
		if got != tc.want || ok != tc.ok {
			t.Errorf("bearerToken(%q) = %q, %v; want %q, %v", tc.header, got, ok, tc.want, tc.ok)
		}
	}
}

func TestBoolParam(t *testing.T) {
	tests := []struct {
		in      string
		want    bool
		wantErr bool
	}{
		{"", false, false},
		{"true", true, false},
		{"1", true, false},
		{"TRUE", true, false},
		{"false", false, false},
		{"0", false, false},
		{"yes", false, true},
		{"on", false, true},
	}
	for _, tc := range tests {
		got, err := boolParam(tc.in)
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("boolParam(%q) = %v, %v; want %v, err %v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}

// TestConcurrentRequests exercises the handler from many goroutines (run
// with -race to detect unsynchronized state).
func TestConcurrentRequests(t *testing.T) {
	fx := newFixture(t, func(o *Options) {
		o.Token = "tok"
		o.Metrics = metrics.New()
	})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				switch (i + j) % 5 {
				case 0:
					do(t, fx.srv, "GET", "/api/v1/sessions", "Bearer tok")
				case 1:
					do(t, fx.srv, "GET", "/api/v1/info", "Bearer tok")
				case 2:
					do(t, fx.srv, "GET", "/metrics", "")
				case 3:
					do(t, fx.srv, "POST", "/api/v1/engines/e1/drain", "Bearer tok")
				case 4:
					do(t, fx.srv, "GET", "/api/v1/engines/e1", "Bearer bad")
				}
			}
		}(i)
	}
	wg.Wait()
}
