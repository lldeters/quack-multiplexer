package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"quackmux/internal/config"
	"quackmux/internal/engine"
	"quackmux/internal/metrics"
	"quackmux/internal/upstream"
	"quackmux/internal/wire"
)

const sweepEngine = "quack-user-alice-x1"

// sweepEngines is one READY USER engine.
type sweepEngines struct {
	mu   sync.Mutex
	gone bool
}

func (e *sweepEngines) target() engine.Target {
	return engine.Target{ID: sweepEngine, Endpoint: "http://engine", Token: "tok", State: engine.StateReady,
		Key: engine.Key{ShareLevel: engine.ShareUser, Owner: "alice", Subdomain: "default", Profile: "default"}}
}

func (e *sweepEngines) IDByUID(uid engine.UID) (string, bool) {
	return sweepEngine, uid == engine.UIDOf(sweepEngine)
}

func (e *sweepEngines) Lookup(id string) (engine.Target, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.target(), id == sweepEngine && !e.gone
}

func (e *sweepEngines) Exists(id string) (engine.State, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.gone {
		return engine.StateStarting, id == sweepEngine // momentarily not ready
	}
	return engine.StateReady, id == sweepEngine
}
func (e *sweepEngines) AttachAdopted(string, string, string) {}
func (e *sweepEngines) DetachSession(string)                 {}

// sweepBackend is an engine's connection table behind the Backend interface.
type sweepBackend struct {
	mu        sync.Mutex
	conns     map[string]bool
	cancelled []string
	listErr   error
	closeErr  error
	// confirm, if set, blocks the confirming heartbeat of an adoption until
	// it is closed; confirming is closed when one is waiting.
	confirm    chan struct{}
	confirming chan struct{}
}

func newSweepBackend(conns ...string) *sweepBackend {
	b := &sweepBackend{conns: map[string]bool{}}
	for _, c := range conns {
		b.conns[c] = true
	}
	return b
}

func (b *sweepBackend) has(conn string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.conns[conn]
}

func (b *sweepBackend) add(conn string) {
	b.mu.Lock()
	b.conns[conn] = true
	b.mu.Unlock()
}

func gone() error {
	return &upstream.EngineError{Response: wire.ErrorResponse{Message: upstream.MsgInvalidConnection}}
}

func (b *sweepBackend) Disconnect(_ context.Context, _, conn string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closeErr != nil {
		return b.closeErr
	}
	if !b.conns[conn] {
		return gone()
	}
	delete(b.conns, conn)
	return nil
}

func (b *sweepBackend) CancelAll(_ context.Context, _, conn string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cancelled = append(b.cancelled, conn)
	return nil
}

func (b *sweepBackend) Heartbeat(_ context.Context, _, conn string) error {
	if b.confirm != nil {
		close(b.confirming)
		<-b.confirm
	}
	if !b.has(conn) {
		return gone()
	}
	return nil
}

func (b *sweepBackend) Connections(context.Context, string, string) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.listErr != nil {
		return nil, b.listErr
	}
	var out []string
	for c := range b.conns {
		out = append(out, c)
	}
	return out, nil
}

type sweepRig struct {
	t       *testing.T
	m       *Manager
	engines *sweepEngines
	backend *sweepBackend
	met     *metrics.Metrics
	sealer  *Sealer
}

func newSweepRig(t *testing.T, conns ...string) *sweepRig {
	t.Helper()
	cfg, err := config.Parse([]byte("auth: {mode: none}\n"))
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := NewSealer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	r := &sweepRig{t: t, engines: &sweepEngines{}, backend: newSweepBackend(conns...), met: metrics.New(), sealer: sealer}
	r.m = NewManager(cfg, r.engines, r.backend, sealer, slog.New(slog.NewTextHandler(io.Discard, nil)), r.met)
	return r
}

// id mints the client-facing id of a session of an earlier gateway process
// on conn.
func (r *sweepRig) id(conn string) string {
	r.t.Helper()
	id, err := r.sealer.Seal(Route{SessionID: NewSessionID(), EngineUID: engine.UIDOf(sweepEngine), BackendConnID: conn,
		User: "alice", CreatedUnix: time.Now().Unix(), HeartbeatTimeout: 60})
	if err != nil {
		r.t.Fatal(err)
	}
	return id
}

func (r *sweepRig) begin() *OrphanSweep {
	return r.m.BeginOrphanSweep(context.Background(), []engine.Target{r.engines.target()})
}

// counter returns the value of a counter, of the series with label
// result=result if result is not empty.
func (r *sweepRig) counter(name, result string) float64 {
	r.t.Helper()
	fams, err := r.met.Registry.Gather()
	if err != nil {
		r.t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "result" && l.GetValue() != result {
					goto next
				}
			}
			return m.GetCounter().GetValue()
		next:
		}
	}
	return 0
}

func (r *sweepRig) sweeps(result string) float64 {
	return r.counter("quackmux_orphan_sweeps_total", result)
}

const (
	connLive   = "A1A1A1A1A1A1A1A1A1A1A1A1A1A1A1A1"
	connDead   = "D0D0D0D0D0D0D0D0D0D0D0D0D0D0D0D0"
	connLate   = "B2B2B2B2B2B2B2B2B2B2B2B2B2B2B2B2"
	connRacing = "C3C3C3C3C3C3C3C3C3C3C3C3C3C3C3C3"
)

// The sweep closes what no client asked for, keeps the adopted connection
// and leaves connections created after the first listing alone; the client
// of the closed connection learns why.
func TestOrphanSweepClosesWhatNoClientAskedFor(t *testing.T) {
	r := newSweepRig(t, connLive, connDead)
	live, dead := r.id(connLive), r.id(connDead)
	s := r.begin()
	if s, c, err := r.m.Resolve(live); s == nil || c != nil || err != nil {
		t.Fatalf("adoption of the live session failed: %v %v", c, err)
	}
	r.backend.add(connLate) // a session of this process (handshake under way)

	if n := s.Finish(context.Background()); n != 1 {
		t.Fatalf("closed %d connections, want 1", n)
	}
	if r.backend.has(connDead) || !r.backend.has(connLive) || !r.backend.has(connLate) {
		t.Fatalf("engine holds %v after the sweep", r.backend.conns)
	}
	if !slices.Equal(r.backend.cancelled, []string{connDead}) {
		t.Fatalf("cancelled %v, want the orphan only (a query of a dead client may still run)", r.backend.cancelled)
	}
	_, c, err := r.m.Resolve(dead)
	if err != nil || c == nil || c.Reason != orphanReason || c.Transient || c.MustInvalidate {
		t.Fatalf("the late client got %+v, %v; want the orphan reason", c, err)
	}
	if _, c, _ = r.m.Resolve(dead); c == nil || c.Reason != orphanReason {
		t.Fatalf("the second request got %+v", c)
	}
	if got := r.counter("quackmux_orphaned_connections_closed_total", ""); got != 1 || r.sweeps("ok") != 1 {
		t.Fatalf("metrics: %v closed, %v ok sweeps", got, r.sweeps("ok"))
	}
	if r.m.seen != nil {
		t.Fatal("the sweep still notes connections after it finished")
	}
}

// A client whose first request arrives while the sweep decides is noted
// before its adoption asks the engine, and keeps its connection.
func TestOrphanSweepKeepsAdoptionUnderWay(t *testing.T) {
	r := newSweepRig(t, connRacing)
	racing := r.id(connRacing)
	s := r.begin()
	r.backend.confirm, r.backend.confirming = make(chan struct{}), make(chan struct{})
	done := make(chan *Closed, 1)
	go func() {
		_, c, _ := r.m.Resolve(racing)
		done <- c
	}()
	<-r.backend.confirming // the adoption waits for the engine
	if n := s.Finish(context.Background()); n != 0 {
		t.Fatalf("the sweep closed %d connections during an adoption", n)
	}
	close(r.backend.confirm)
	if c := <-done; c != nil {
		t.Fatalf("the adoption failed: %+v", c)
	}
	if !r.backend.has(connRacing) || r.m.Count() != 1 {
		t.Fatal("the adopted connection is gone")
	}
}

// A client whose adoption failed for a passing reason (the engine was not
// reachable) asked for its connection all the same.
func TestOrphanSweepKeepsConnectionOfFailedAdoption(t *testing.T) {
	r := newSweepRig(t, connLive)
	live := r.id(connLive)
	s := r.begin()
	r.engines.mu.Lock()
	r.engines.gone = true // Lookup fails: "engine is not ready"
	r.engines.mu.Unlock()
	if _, c, _ := r.m.Resolve(live); c == nil {
		t.Fatal("adoption succeeded without the engine")
	}
	r.engines.mu.Lock()
	r.engines.gone = false
	r.engines.mu.Unlock()
	if n := s.Finish(context.Background()); n != 0 || !r.backend.has(connLive) {
		t.Fatalf("closed %d connections of a client that came back", n)
	}
	if _, c, _ := r.m.Resolve(live); c != nil {
		t.Fatalf("the retry got %+v", c)
	}
}

// Without a fresh leader lease nothing is closed; an engine that is gone, or
// a listing that fails, ends the sweep of that engine, and a close that
// fails leaves the connection usable for a late client.
func TestOrphanSweepRefusals(t *testing.T) {
	t.Run("lease not fresh", func(t *testing.T) {
		r := newSweepRig(t, connDead)
		s := r.begin()
		r.m.SetFence(func() bool { return false })
		if n := s.Finish(context.Background()); n != 0 || !r.backend.has(connDead) || r.sweeps("skipped") != 1 {
			t.Fatalf("closed %d (skipped %v)", n, r.sweeps("skipped"))
		}
	})
	t.Run("engine gone", func(t *testing.T) {
		r := newSweepRig(t, connDead)
		s := r.begin()
		r.engines.gone = true
		if n := s.Finish(context.Background()); n != 0 || r.sweeps("skipped") != 1 {
			t.Fatalf("closed %d (skipped %v)", n, r.sweeps("skipped"))
		}
	})
	t.Run("first listing fails", func(t *testing.T) {
		r := newSweepRig(t, connDead)
		r.backend.listErr = &upstream.EngineError{Response: wire.ErrorResponse{Message: "Authorization callback failed: no"}}
		s := r.begin()
		r.backend.listErr = nil
		if n := s.Finish(context.Background()); n != 0 || !r.backend.has(connDead) || r.sweeps("failed") != 1 {
			t.Fatalf("closed %d (failed %v)", n, r.sweeps("failed"))
		}
	})
	t.Run("second listing fails", func(t *testing.T) {
		r := newSweepRig(t, connDead)
		s := r.begin()
		r.backend.listErr = errors.New("connection refused")
		if n := s.Finish(context.Background()); n != 0 || !r.backend.has(connDead) || r.sweeps("failed") != 1 {
			t.Fatalf("closed %d (failed %v)", n, r.sweeps("failed"))
		}
	})
	t.Run("close fails", func(t *testing.T) {
		r := newSweepRig(t, connDead)
		dead := r.id(connDead)
		s := r.begin()
		r.backend.closeErr = errors.New("connection refused")
		if n := s.Finish(context.Background()); n != 0 || r.sweeps("failed") != 1 {
			t.Fatalf("closed %d (failed %v)", n, r.sweeps("failed"))
		}
		r.backend.closeErr = nil
		if _, c, _ := r.m.Resolve(dead); c != nil {
			t.Fatalf("the client of a connection that was not closed got %+v", c)
		}
	})
	t.Run("connection gone meanwhile", func(t *testing.T) {
		r := newSweepRig(t, connDead)
		s := r.begin()
		r.backend.closeErr = gone()
		if n := s.Finish(context.Background()); n != 1 || r.sweeps("ok") != 1 {
			t.Fatalf("closed %d (ok %v): a connection the engine no longer knows counts as closed", n, r.sweeps("ok"))
		}
	})
}

// The marks of closed connections expire with the tombstones.
func TestOrphanMarksExpire(t *testing.T) {
	r := newSweepRig(t, connDead)
	clock := time.Now()
	r.m.SetClock(func() time.Time { return clock })
	r.begin().Finish(context.Background())
	if len(r.m.orphans) != 1 {
		t.Fatalf("%d marks, want 1", len(r.m.orphans))
	}
	clock = clock.Add(r.m.cfg.Session.TombstoneTTL.Duration + time.Second)
	r.m.Reap()
	if len(r.m.orphans) != 0 {
		t.Fatalf("%d marks after the tombstone TTL", len(r.m.orphans))
	}
}

// waitRunning counts running time only: a pause of the process starts the
// wait again.
func TestWaitRunning(t *testing.T) {
	const every = 5 * time.Second
	start := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	// run lets the ticks come after the given gaps and returns the result
	// and the time of the tick after which waitRunning returned.
	run := func(gaps ...time.Duration) (bool, time.Duration) {
		tick := make(chan time.Time)
		done := make(chan bool, 1)
		go func() { done <- waitRunning(context.Background(), 30*time.Second, every, tick, start) }()
		at := start
		for _, g := range gaps {
			select {
			case tick <- at.Add(g):
				at = at.Add(g)
			case ok := <-done:
				return ok, at.Sub(start)
			}
		}
		return <-done, at.Sub(start)
	}
	if ok, took := run(slices.Repeat([]time.Duration{every}, 7)...); !ok || took != 30*time.Second {
		t.Fatalf("regular ticks: %v after %s", ok, took)
	}
	// A pause of a minute after 25s: the wait starts again.
	gaps := append(slices.Repeat([]time.Duration{every}, 5), time.Minute)
	gaps = append(gaps, slices.Repeat([]time.Duration{every}, 7)...)
	if ok, took := run(gaps...); !ok || took != 25*time.Second+time.Minute+30*time.Second {
		t.Fatalf("with a pause: %v after %s", ok, took)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitRunning(ctx, time.Second, every, make(chan time.Time), start) {
		t.Fatal("waitRunning reported true after ctx ended")
	}
}
