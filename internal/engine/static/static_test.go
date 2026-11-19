package static

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quackmux/internal/config"
	"quackmux/internal/engine"
	"quackmux/internal/metrics"
)

const banner = "This is a DuckDB Quack RPC endpoint. Use ATTACH 'quack:...' to connect here.\n"

// fakeEngine answers GET / like a Quack server while healthy and with 503
// otherwise.
type fakeEngine struct {
	*httptest.Server
	healthy atomic.Bool
	probes  atomic.Int64
	delay   atomic.Int64 // nanoseconds before answering
}

func newFakeEngine(t *testing.T, healthy bool) *fakeEngine {
	t.Helper()
	f := &fakeEngine{}
	f.healthy.Store(healthy)
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		f.probes.Add(1)
		if d := time.Duration(f.delay.Load()); d > 0 {
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				return
			}
		}
		if !f.healthy.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, banner)
	}))
	t.Cleanup(f.Close)
	return f
}

func baseConfig(t *testing.T, engines ...config.StaticEngine) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(`
auth: {mode: none}
provider:
  type: static
  static:
    healthInterval: 20ms
    engines:
      - {name: placeholder, url: "http://127.0.0.1:1", token: t, shareLevel: SERVER}
profiles:
  default: {}
  large:
    pool: {size: 2}
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Provider.Static.Engines = engines
	return cfg
}

// recorder collects the events of a provider.
type recorder struct {
	mu     sync.Mutex
	events []engine.Instance
	closed bool
}

func record(p *Provider) *recorder {
	r := &recorder{}
	go func() {
		for ev := range p.Events() {
			r.mu.Lock()
			r.events = append(r.events, ev)
			r.mu.Unlock()
		}
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
	}()
	return r
}

func (r *recorder) count(id string, phase engine.Phase) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, ev := range r.events {
		if ev.ID == id && ev.Phase == phase {
			n++
		}
	}
	return n
}

func (r *recorder) last(id string, phase engine.Phase) (engine.Instance, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.events) - 1; i >= 0; i-- {
		if ev := r.events[i]; ev.ID == id && ev.Phase == phase {
			return ev, true
		}
	}
	return engine.Instance{}, false
}

func (r *recorder) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// run starts the provider and stops it at the end of the test.
func run(t *testing.T, p *Provider) (*recorder, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- p.Run(ctx) }()
	rec := record(p)
	t.Cleanup(func() {
		cancel()
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
			t.Error("Run did not return")
		}
	})
	return rec, cancel, errc
}

func find(list []engine.Instance, id string) engine.Instance {
	for _, inst := range list {
		if inst.ID == id {
			return inst
		}
	}
	return engine.Instance{}
}

func TestNewBuildsInstances(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("  file-token\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig(t,
		config.StaticEngine{Name: "shared", URL: "http://duckdb.example:9494/", Token: "inline-token", ShareLevel: "server"},
		config.StaticEngine{Name: "alice-etl", URL: " https://10.0.0.5:9494// ", TokenFile: tokenFile, ShareLevel: "User",
			Owner: "alice", Subdomain: "ETL", Profile: "large", PoolIndex: 1},
		config.StaticEngine{Name: "analysts", URL: "http://proxy.example/engines/analysts", Token: "g", ShareLevel: "GROUP", Owner: "analysts"},
	)
	before := time.Now()
	p, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "static" || p.CanLaunch() {
		t.Fatal("provider basics")
	}
	list, err := p.List(context.Background())
	if err != nil || len(list) != 3 {
		t.Fatalf("List: %+v %v", list, err)
	}
	want := []engine.Instance{
		{ID: "shared", Key: engine.Key{ShareLevel: engine.ShareServer, Subdomain: "default", Profile: "default"},
			ProfileName: "default", Endpoint: "http://duckdb.example:9494", Token: "inline-token"},
		{ID: "alice-etl", Key: engine.Key{ShareLevel: engine.ShareUser, Owner: "alice", Subdomain: "etl", Profile: "large", PoolIndex: 1},
			ProfileName: "large", Endpoint: "https://10.0.0.5:9494", Token: "file-token"},
		{ID: "analysts", Key: engine.Key{ShareLevel: engine.ShareGroup, Owner: "analysts", Subdomain: "default", Profile: "default"},
			ProfileName: "default", Endpoint: "http://proxy.example/engines/analysts", Token: "g"},
	}
	for i, w := range want {
		got := list[i]
		if got.CreatedAt.Before(before) || got.CreatedAt.After(time.Now()) {
			t.Errorf("%s: CreatedAt %v", got.ID, got.CreatedAt)
		}
		if got.Phase != engine.PhaseStarting {
			t.Errorf("%s: phase before the first probe %s", got.ID, got.Phase)
		}
		got.CreatedAt, got.Phase = time.Time{}, ""
		if got != w {
			t.Errorf("instance %d:\n got %+v\nwant %+v", i, got, w)
		}
	}
	if p.entryProbeURL("analysts") != "http://proxy.example/engines/analysts/" {
		t.Errorf("probe url %q", p.entryProbeURL("analysts"))
	}
	if p.SpecHash("default", config.Profile{}) != "" {
		t.Error("static engines have no spec hash")
	}
	if a, b := p.NewID(engine.Key{ShareLevel: engine.ShareUser}, false), p.NewID(engine.Key{}, false); a == b {
		t.Error("NewID must be unique")
	}
}

func (p *Provider) entryProbeURL(id string) string {
	for _, e := range p.engines {
		if e.inst.ID == id {
			return e.probeURL
		}
	}
	return ""
}

func TestDefaultsFollowConfiguredDefaults(t *testing.T) {
	cfg := baseConfig(t, config.StaticEngine{Name: "a", URL: "http://h:1", Token: "t", ShareLevel: "SERVER"})
	cfg.Defaults.Subdomain, cfg.Defaults.Profile = "main", "large"
	p, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	list, _ := p.List(context.Background())
	if k := list[0].Key; k.Subdomain != "main" || k.Profile != "large" || list[0].ProfileName != "large" {
		t.Fatalf("key %+v", k)
	}
}

func TestNewRejectsInvalidEngines(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok := config.StaticEngine{Name: "e", URL: "http://h:9494", Token: "t", ShareLevel: "SERVER"}
	for name, tc := range map[string]struct {
		mutate func(*config.StaticEngine)
		want   string
	}{
		"no name":            {func(e *config.StaticEngine) { e.Name = "" }, "name is required"},
		"no url":             {func(e *config.StaticEngine) { e.URL = " " }, "url is required"},
		"ftp url":            {func(e *config.StaticEngine) { e.URL = "ftp://h:21" }, "http:// or https://"},
		"quack uri":          {func(e *config.StaticEngine) { e.URL = "quack:h:9494" }, "http:// or https://"},
		"no host":            {func(e *config.StaticEngine) { e.URL = "http://:9494" }, "no host"},
		"credentials":        {func(e *config.StaticEngine) { e.URL = "http://u:p@h:9494" }, "credentials"},
		"query":              {func(e *config.StaticEngine) { e.URL = "http://h:9494/?x=1" }, "query"},
		"with /quack":        {func(e *config.StaticEngine) { e.URL = "http://h:9494/quack" }, "/quack"},
		"no token":           {func(e *config.StaticEngine) { e.Token = "  " }, "token or tokenFile is required"},
		"both tokens":        {func(e *config.StaticEngine) { e.TokenFile = empty }, "not both"},
		"missing token file": {func(e *config.StaticEngine) { e.Token, e.TokenFile = "", filepath.Join(dir, "nope") }, "read tokenFile"},
		"empty token file":   {func(e *config.StaticEngine) { e.Token, e.TokenFile = "", empty }, "is empty"},
		"bad share level":    {func(e *config.StaticEngine) { e.ShareLevel = "TEAM" }, "unknown share level"},
		"connection":         {func(e *config.StaticEngine) { e.ShareLevel = "connection" }, "CONNECTION"},
		"user without owner": {func(e *config.StaticEngine) { e.ShareLevel = "USER" }, "owner (the user) is required"},
		"group w/o owner":    {func(e *config.StaticEngine) { e.ShareLevel = "GROUP" }, "owner (the group) is required"},
		"server with owner":  {func(e *config.StaticEngine) { e.Owner = "alice" }, "owner must be empty"},
		"bad subdomain":      {func(e *config.StaticEngine) { e.Subdomain = "no_underscores" }, "invalid subdomain"},
		"unknown profile":    {func(e *config.StaticEngine) { e.Profile = "huge" }, `profile "huge" is not defined`},
		"pool index":         {func(e *config.StaticEngine) { e.PoolIndex = 1 }, "outside the pool"},
		"negative pool":      {func(e *config.StaticEngine) { e.Profile, e.PoolIndex = "large", -1 }, "outside the pool"},
	} {
		e := ok
		tc.mutate(&e)
		_, err := New(baseConfig(t, e), nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, tc.want)
		}
	}
	if _, err := New(baseConfig(t, ok, ok), nil); err == nil || !strings.Contains(err.Error(), "duplicate name") {
		t.Errorf("duplicate names: %v", err)
	}
	if _, err := New(baseConfig(t), nil); err == nil {
		t.Error("no engines must be rejected")
	}
	if _, err := New(nil, nil); err == nil {
		t.Error("New(nil) must fail")
	}
	// All problems are reported at once.
	bad := ok
	bad.Token = ""
	_, err := New(baseConfig(t, bad, config.StaticEngine{Name: "x", URL: "nope", Token: "t", ShareLevel: "SERVER"}), nil)
	if err == nil || !strings.Contains(err.Error(), "engines[0] (e)") || !strings.Contains(err.Error(), "engines[1] (x)") {
		t.Errorf("joined errors: %v", err)
	}
}

func TestProbeTransitions(t *testing.T) {
	a, b := newFakeEngine(t, true), newFakeEngine(t, false)
	cfg := baseConfig(t,
		config.StaticEngine{Name: "a", URL: a.URL, Token: "ta", ShareLevel: "SERVER"},
		config.StaticEngine{Name: "b", URL: b.URL + "/", Token: "tb", ShareLevel: "USER", Owner: "bob"},
	)
	p, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec, cancel, errc := run(t, p)
	if err := p.WaitSynced(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The first round decided both phases before WaitSynced returned.
	list, _ := p.List(context.Background())
	if ia := find(list, "a"); ia.Phase != engine.PhaseReady || ia.Message != MessageHealthy || ia.Endpoint != a.URL {
		t.Fatalf("a after the first round: %+v", ia)
	}
	if ib := find(list, "b"); ib.Phase != engine.PhaseFailed || !strings.Contains(ib.Message, "503") || ib.Endpoint != b.URL {
		t.Fatalf("b after the first round: %+v", ib)
	}
	eventually(t, "first-round events", func() bool { return rec.count("a", engine.PhaseReady) >= 1 && rec.count("b", engine.PhaseFailed) == 1 })

	// a fails: one PhaseFailed event, however many rounds it stays down.
	a.healthy.Store(false)
	eventually(t, "a failed", func() bool { return rec.count("a", engine.PhaseFailed) == 1 })
	ev, _ := rec.last("a", engine.PhaseFailed)
	if !strings.HasPrefix(ev.Message, "health check failed: ") || !strings.Contains(ev.Message, "503 Service Unavailable") {
		t.Fatalf("failure message %q", ev.Message)
	}
	seen := a.probes.Load()
	eventually(t, "more rounds", func() bool { return a.probes.Load() >= seen+5 })
	if n := rec.count("a", engine.PhaseFailed); n != 1 {
		t.Fatalf("%d PhaseFailed events for one outage", n)
	}
	if n := rec.count("b", engine.PhaseFailed); n != 1 {
		t.Fatalf("%d PhaseFailed events for b", n)
	}

	// b recovers; healthy engines are re-announced with every probe.
	b.healthy.Store(true)
	eventually(t, "b ready", func() bool { return rec.count("b", engine.PhaseReady) >= 1 })
	if ev, _ := rec.last("b", engine.PhaseReady); ev.Message != MessageHealthy || ev.Token != "tb" || ev.Key.Owner != "bob" {
		t.Fatalf("recovery event %+v", ev)
	}
	eventually(t, "re-announcements", func() bool { return rec.count("b", engine.PhaseReady) >= 3 })
	a.healthy.Store(true)
	eventually(t, "a back", func() bool {
		ia := find(must(p.List(context.Background())), "a")
		return ia.Phase == engine.PhaseReady && ia.Message == MessageHealthy
	})

	cancel()
	if err := <-errc; err != nil {
		t.Fatalf("Run: %v", err)
	}
	eventually(t, "events channel closed", rec.isClosed)
	for _, id := range []string{"a", "b"} {
		if n := rec.count(id, engine.PhaseGone); n != 0 {
			t.Fatalf("static engines must never be reported gone (%s: %d)", id, n)
		}
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestProbeTimeoutAndUnreachable(t *testing.T) {
	slow := newFakeEngine(t, true)
	slow.delay.Store(int64(time.Second))
	// A port nobody listens on.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + l.Addr().String()
	l.Close()
	cfg := baseConfig(t,
		config.StaticEngine{Name: "slow", URL: slow.URL, Token: "t", ShareLevel: "SERVER"},
		config.StaticEngine{Name: "dead", URL: dead, Token: "t", ShareLevel: "SERVER", Subdomain: "other"},
	)
	p, err := New(cfg, nil, WithProbeTimeout(100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	run(t, p)
	start := time.Now()
	if err := p.WaitSynced(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 900*time.Millisecond {
		t.Fatalf("the first round took %s despite the probe timeout", d)
	}
	list, _ := p.List(context.Background())
	if s := find(list, "slow"); s.Phase != engine.PhaseFailed || !strings.Contains(s.Message, "no answer within 100ms") {
		t.Fatalf("slow: %+v", s)
	}
	if d := find(list, "dead"); d.Phase != engine.PhaseFailed || !strings.HasPrefix(d.Message, "health check failed: ") {
		t.Fatalf("dead: %+v", d)
	}
}

func TestNotSupported(t *testing.T) {
	srv := newFakeEngine(t, true)
	p, err := New(baseConfig(t, config.StaticEngine{Name: "e", URL: srv.URL, Token: "t", ShareLevel: "SERVER"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Launch(context.Background(), engine.LaunchRequest{ID: "x"}); !errors.Is(err, engine.ErrNotSupported) {
		t.Fatalf("Launch: %v", err)
	}
	if err := p.Claim(context.Background(), "e", engine.Key{}); !errors.Is(err, engine.ErrNotSupported) {
		t.Fatalf("Claim: %v", err)
	}
	if err := p.Terminate(context.Background(), "e", "admin"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if list, _ := p.List(context.Background()); len(list) != 1 || list[0].ID != "e" {
		t.Fatalf("Terminate must keep the engine: %+v", list)
	}
}

func TestWaitSynced(t *testing.T) {
	srv := newFakeEngine(t, true)
	srv.delay.Store(int64(300 * time.Millisecond))
	newProvider := func() *Provider {
		p, err := New(baseConfig(t, config.StaticEngine{Name: "e", URL: srv.URL, Token: "t", ShareLevel: "SERVER"}), nil)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Before Run (and while the first round is running) WaitSynced blocks.
	p := newProvider()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := p.WaitSynced(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitSynced before Run: %v", err)
	}
	run(t, p)
	start := time.Now()
	if err := p.WaitSynced(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Fatalf("WaitSynced returned after %s, before the first probe answered", d)
	}
	if list, _ := p.List(context.Background()); list[0].Phase != engine.PhaseReady {
		t.Fatalf("after WaitSynced: %+v", list)
	}
	if err := p.Run(context.Background()); err == nil {
		t.Fatal("a second Run must fail")
	}

	// Run stops during the first round: WaitSynced reports it instead of
	// blocking forever.
	p2 := newProvider()
	_, stop, errc := run(t, p2)
	time.Sleep(50 * time.Millisecond)
	stop()
	if err := <-errc; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := p2.WaitSynced(context.Background()); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("WaitSynced after an early stop: %v", err)
	}
}

func TestManagerIntegration(t *testing.T) {
	shared, alice := newFakeEngine(t, true), newFakeEngine(t, true)
	cfg := baseConfig(t,
		config.StaticEngine{Name: "shared", URL: shared.URL, Token: "shared-token", ShareLevel: "SERVER"},
		config.StaticEngine{Name: "alice", URL: alice.URL, Token: "alice-token", ShareLevel: "USER", Owner: "alice"},
	)
	p, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := engine.NewManager(cfg, p, slog.New(slog.DiscardHandler), metrics.New())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	state := func(id string) engine.State {
		v, _ := m.Get(id)
		return v.State
	}
	if state("shared") != engine.StateReady || state("alice") != engine.StateReady {
		t.Fatalf("after start: %+v", m.List())
	}
	key := engine.Key{ShareLevel: engine.ShareUser, Owner: "alice", Subdomain: "default", Profile: "default"}
	lease, err := m.Acquire(ctx, key, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if lease.ID != "alice" || lease.Endpoint != alice.URL || lease.Token != "alice-token" {
		t.Fatalf("lease %+v", lease.Target)
	}
	lease.Release()
	if _, err := m.Acquire(ctx, engine.Key{ShareLevel: engine.ShareUser, Owner: "bob", Subdomain: "default", Profile: "default"}, "bob"); !errors.Is(err, engine.ErrCannotLaunch) {
		t.Fatalf("no engine for bob: %v", err)
	}

	// The gateway takes an engine out of service on its own; the next
	// successful probe brings it back.
	m.MarkFailed("shared", "engine database invalidated: test")
	if state("shared") != engine.StateFailed {
		t.Fatal("MarkFailed")
	}
	eventually(t, "shared back in service", func() bool { return state("shared") == engine.StateReady })

	// Failed probes take an engine out of service until it answers again.
	alice.healthy.Store(false)
	eventually(t, "alice failed", func() bool { return state("alice") == engine.StateFailed })
	if _, err := m.Acquire(ctx, key, "alice"); err == nil {
		t.Fatal("a failed static engine must not be used")
	}
	alice.healthy.Store(true)
	eventually(t, "alice recovered", func() bool { return state("alice") == engine.StateReady })
	if v, _ := m.Get("alice"); v.Message != MessageHealthy {
		t.Fatalf("message after recovery %q", v.Message)
	}
	lease, err = m.Acquire(ctx, key, "alice")
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	m.Reconcile() // permanent engines are never reaped
	if len(m.List()) != 2 {
		t.Fatalf("engines: %+v", m.List())
	}
}

func TestCoverageWarnings(t *testing.T) {
	var buf strings.Builder
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(writerFunc(func(b []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(b)
	}), nil))
	cfg := baseConfig(t,
		config.StaticEngine{Name: "p0", URL: "http://h:1", Token: "t", ShareLevel: "SERVER", Profile: "large"},
		config.StaticEngine{Name: "s1", URL: "http://h:2", Token: "t", ShareLevel: "SERVER"},
		config.StaticEngine{Name: "s2", URL: "http://h:3", Token: "t", ShareLevel: "SERVER"},
	)
	if _, err := New(cfg, log); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	for _, want := range []string{"missingPoolIndices=[1]", "same engine key", `engines="[s1 s2]"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }
