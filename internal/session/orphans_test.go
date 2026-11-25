package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
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

// sweepBackend is an engine's connection table behind the Backend interface,
// with the lease semantics of the Quack server: a message for a connection
// whose lease ran out removes it and fails, any other message renews it.
type sweepBackend struct {
	mu       sync.Mutex
	conns    map[string]bool // id -> lease expired
	beats    map[string]int  // heartbeats per connection
	listings int
	closes   []string // DISCONNECT and CANCEL: the sweep sends neither
	listErr  error
	beatErr  error
}

func newSweepBackend(conns ...string) *sweepBackend {
	b := &sweepBackend{conns: map[string]bool{}, beats: map[string]int{}}
	for _, c := range conns {
		b.conns[c] = false
	}
	return b
}

func (b *sweepBackend) has(conn string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.conns[conn]
	return ok
}

// add creates a connection, expire lets its lease run out.
func (b *sweepBackend) add(conn string) {
	b.mu.Lock()
	b.conns[conn] = false
	b.mu.Unlock()
}

func (b *sweepBackend) expire(conns ...string) {
	b.mu.Lock()
	for _, c := range conns {
		b.conns[c] = true
	}
	b.mu.Unlock()
}

func engineErr(msg string) error {
	return &upstream.EngineError{Response: wire.ErrorResponse{Message: msg}}
}

// message applies the lease check of the Quack server to a message for conn.
func (b *sweepBackend) message(conn string) error {
	expired, ok := b.conns[conn]
	switch {
	case !ok:
		return engineErr(upstream.MsgInvalidConnection)
	case expired:
		delete(b.conns, conn)
		return engineErr(upstream.MsgLeaseExpired)
	}
	return nil
}

func (b *sweepBackend) Disconnect(_ context.Context, _, conn string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closes = append(b.closes, "DISCONNECT "+conn)
	if err := b.message(conn); err != nil {
		return err
	}
	delete(b.conns, conn)
	return nil
}

func (b *sweepBackend) CancelAll(_ context.Context, _, conn string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closes = append(b.closes, "CANCEL "+conn)
	return b.message(conn)
}

func (b *sweepBackend) Heartbeat(_ context.Context, _, conn string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.beats[conn]++
	if b.beatErr != nil {
		return b.beatErr
	}
	return b.message(conn)
}

func (b *sweepBackend) Connections(context.Context, string, string) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.listings++
	if b.listErr != nil {
		return nil, b.listErr
	}
	var out []string
	for c := range b.conns {
		out = append(out, c)
	}
	return out, nil
}

func (b *sweepBackend) heartbeats(conn string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.beats[conn]
}

type sweepRig struct {
	t       *testing.T
	m       *Manager
	engines *sweepEngines
	backend *sweepBackend
	met     *metrics.Metrics
	sealer  *Sealer
}

func newSweepRig(t *testing.T, yaml string, conns ...string) *sweepRig {
	t.Helper()
	cfg, err := config.Parse([]byte("auth: {mode: none}\n" + yaml))
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
	connLive    = "A1A1A1A1A1A1A1A1A1A1A1A1A1A1A1A1"
	connDead    = "D0D0D0D0D0D0D0D0D0D0D0D0D0D0D0D0"
	connCut     = "B2B2B2B2B2B2B2B2B2B2B2B2B2B2B2B2"
	connForeign = "C3C3C3C3C3C3C3C3C3C3C3C3C3C3C3C3"
	connLate    = "E4E4E4E4E4E4E4E4E4E4E4E4E4E4E4E4"
)

// The sweep probes what no session adopted. The engine removes the dead
// client's connection, whose lease ran out; it keeps the connection of a
// client that was cut off for longer than any heartbeat interval but whose
// lease still runs, and one that someone else keeps alive. The sweep sends
// neither DISCONNECT nor CANCEL, and leaves the adopted connection and one
// created after the takeover alone.
func TestOrphanSweepRemovesOnlyExpiredConnections(t *testing.T) {
	r := newSweepRig(t, "", connLive, connDead, connCut, connForeign)
	live, dead, cut := r.id(connLive), r.id(connDead), r.id(connCut)
	s := r.begin()
	if s, c, err := r.m.Resolve(live); s == nil || c != nil || err != nil {
		t.Fatalf("adoption of the live session failed: %v %v", c, err)
	}
	r.backend.add(connLate)                       // a handshake of this process
	r.backend.expire(connDead, connLate)          // nobody renewed them
	beatsBefore := r.backend.heartbeats(connLive) // the confirming heartbeat

	if n := s.Finish(context.Background()); n != 1 {
		t.Fatalf("closed %d connections, want 1", n)
	}
	if r.backend.has(connDead) {
		t.Fatal("the connection of the dead client is still there")
	}
	for _, c := range []string{connLive, connCut, connForeign, connLate} {
		if !r.backend.has(c) {
			t.Fatalf("the sweep removed %s", c)
		}
	}
	if len(r.backend.closes) != 0 {
		t.Fatalf("the sweep sent %v", r.backend.closes)
	}
	if n := r.backend.heartbeats(connLive); n != beatsBefore {
		t.Fatal("the sweep probed the connection of an open session")
	}
	if r.backend.heartbeats(connLate) != 0 {
		t.Fatal("the sweep probed a connection created after the takeover")
	}
	if r.backend.heartbeats(connCut) != 1 || r.backend.heartbeats(connForeign) != 1 {
		t.Fatal("the sweep did not probe every connection that no session adopted")
	}
	if got := r.counter("quackmux_orphaned_connections_closed_total", ""); got != 1 || r.sweeps("ok") != 1 {
		t.Fatalf("metrics: %v closed, %v ok sweeps", got, r.sweeps("ok"))
	}

	// The client that was cut off comes back after the sweep and keeps its
	// connection; the dead one, should it come back, learns that the engine
	// let its connection go.
	if s, c, err := r.m.Resolve(cut); s == nil || c != nil || err != nil {
		t.Fatalf("the client that was cut off lost its session: %+v %v", c, err)
	}
	if _, c, err := r.m.Resolve(dead); err != nil || c == nil || c.Transient || c.MustInvalidate {
		t.Fatalf("the late client got %+v, %v", c, err)
	}
}

// SweepOrphans waits for the engine lease plus the heartbeat grace by the
// manager's clock: ticks alone do not end the wait, whatever their values.
func TestSweepOrphansWaitsForTheEngineLease(t *testing.T) {
	r := newSweepRig(t, "session: {checkInterval: 5ms, backendHeartbeatTimeout: 60s, heartbeatGrace: 10s}\n", connDead)
	var mu sync.Mutex
	clock := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	advance := func(d time.Duration) {
		mu.Lock()
		clock = clock.Add(d)
		mu.Unlock()
	}
	r.m.SetClock(now)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.m.SweepOrphans(ctx, []engine.Target{r.engines.target()})
	eventually(t, "the first listing", func() bool {
		r.backend.mu.Lock()
		defer r.backend.mu.Unlock()
		return r.backend.listings == 1
	})
	r.backend.expire(connDead)
	advance(69 * time.Second)
	time.Sleep(50 * time.Millisecond) // ten ticks
	if r.backend.heartbeats(connDead) != 0 {
		t.Fatal("the sweep probed before the engine lease and the heartbeat grace had passed")
	}
	advance(time.Second)
	eventually(t, "the probe", func() bool { return !r.backend.has(connDead) })
	if r.counter("quackmux_orphaned_connections_closed_total", "") != 1 {
		t.Fatal("the removed connection was not counted")
	}
}

// A sweep whose context ends while it waits probes nothing.
func TestSweepOrphansStopsWithContext(t *testing.T) {
	r := newSweepRig(t, "session: {checkInterval: 5ms}\n", connDead)
	var late atomic.Bool
	r.m.SetClock(func() time.Time {
		if late.Load() {
			return time.Now().Add(2 * time.Hour)
		}
		return time.Now()
	})
	ctx, cancel := context.WithCancel(context.Background())
	r.m.SweepOrphans(ctx, []engine.Target{r.engines.target()})
	eventually(t, "the first listing", func() bool {
		r.backend.mu.Lock()
		defer r.backend.mu.Unlock()
		return r.backend.listings == 1
	})
	cancel()
	late.Store(true)
	time.Sleep(50 * time.Millisecond)
	if r.backend.heartbeats(connDead) != 0 {
		t.Fatal("the sweep probed after its context ended")
	}
}

// engine.closeOrphanedConnections: false keeps the gateway from even
// listing the connections.
func TestSweepOrphansSwitchedOff(t *testing.T) {
	r := newSweepRig(t, "engine: {closeOrphanedConnections: false}\n", connDead)
	r.m.SweepOrphans(context.Background(), []engine.Target{r.engines.target()})
	time.Sleep(20 * time.Millisecond)
	r.backend.mu.Lock()
	defer r.backend.mu.Unlock()
	if r.backend.listings != 0 {
		t.Fatalf("%d listings with the sweep switched off", r.backend.listings)
	}
}

// Without a fresh leader lease nothing is probed; an engine that is gone, or
// a listing that fails, ends the sweep of that engine; a probe that fails
// leaves the connection to its client, and a connection that is gone
// meanwhile does not count as closed.
func TestOrphanSweepRefusals(t *testing.T) {
	t.Run("lease not fresh", func(t *testing.T) {
		r := newSweepRig(t, "", connDead)
		s := r.begin()
		r.backend.expire(connDead)
		r.m.SetFence(func() bool { return false })
		if n := s.Finish(context.Background()); n != 0 || !r.backend.has(connDead) || r.sweeps("skipped") != 1 {
			t.Fatalf("closed %d (skipped %v)", n, r.sweeps("skipped"))
		}
	})
	t.Run("engine gone", func(t *testing.T) {
		r := newSweepRig(t, "", connDead)
		s := r.begin()
		r.engines.gone = true
		if n := s.Finish(context.Background()); n != 0 || r.backend.heartbeats(connDead) != 0 || r.sweeps("skipped") != 1 {
			t.Fatalf("closed %d (skipped %v)", n, r.sweeps("skipped"))
		}
	})
	t.Run("listing refused by the authorization function", func(t *testing.T) {
		r := newSweepRig(t, "", connDead)
		r.backend.listErr = engineErr(`Authorization callback failed: Invalid Input Error: quackmux_authorize: not allowed on this engine: "quack_active_connections"`)
		s := r.begin()
		if n := s.Finish(context.Background()); n != 0 || r.backend.heartbeats(connDead) != 0 || r.sweeps("skipped") != 1 {
			t.Fatalf("closed %d (skipped %v)", n, r.sweeps("skipped"))
		}
	})
	t.Run("listing fails", func(t *testing.T) {
		r := newSweepRig(t, "", connDead)
		r.backend.listErr = errors.New("connection refused")
		s := r.begin()
		if n := s.Finish(context.Background()); n != 0 || r.backend.heartbeats(connDead) != 0 || r.sweeps("failed") != 1 {
			t.Fatalf("closed %d (failed %v)", n, r.sweeps("failed"))
		}
	})
	t.Run("probe fails", func(t *testing.T) {
		r := newSweepRig(t, "", connCut)
		cut := r.id(connCut)
		s := r.begin()
		r.backend.beatErr = errors.New("connection refused")
		if n := s.Finish(context.Background()); n != 0 || r.sweeps("failed") != 1 {
			t.Fatalf("closed %d (failed %v)", n, r.sweeps("failed"))
		}
		r.backend.beatErr = nil
		if _, c, _ := r.m.Resolve(cut); c != nil {
			t.Fatalf("the client of a connection the sweep could not probe got %+v", c)
		}
	})
	t.Run("connection gone meanwhile", func(t *testing.T) {
		r := newSweepRig(t, "", connDead)
		s := r.begin()
		r.backend.mu.Lock()
		delete(r.backend.conns, connDead)
		r.backend.mu.Unlock()
		if n := s.Finish(context.Background()); n != 0 || r.sweeps("ok") != 1 {
			t.Fatalf("closed %d (ok %v): a connection that went by itself was counted", n, r.sweeps("ok"))
		}
	})
}

// eventually polls cond for up to five seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
