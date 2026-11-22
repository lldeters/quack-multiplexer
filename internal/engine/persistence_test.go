package engine_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quackmux/internal/config"
	"quackmux/internal/engine"
	"quackmux/internal/engine/fake"
	"quackmux/internal/metrics"
)

const persistentConfig = `
engine:
  checkInterval: 1h   # the tests call Reconcile themselves
  drainTimeout: 10m
  startupTimeout: 30s
profiles:
  default: {}
  personal:
    allowedShareLevels: [USER]
    allowedSubdomains: [default]
    persistence: {size: 1Gi}
`

var personalKey = engine.Key{ShareLevel: engine.ShareUser, Owner: "alice", Subdomain: "default", Profile: "personal"}

// guardedProvider wraps the fake provider. It records every launch for a key
// while another instance of that key still exists (an overlap: two engines on
// one data volume), it can hold back terminations like a pod in its grace
// period, and it keeps data volumes for the admin operations.
type guardedProvider struct {
	*fake.Provider
	list []engine.Instance // what the first List returns, if set

	mu       sync.Mutex
	overlaps []string
	order    []string
	hold     bool
	held     []string
	vols     map[string]engine.Volume
	block    chan struct{} // DeleteVolume waits for it if set
	entered  chan struct{} // DeleteVolume signals here before it waits
}

func newGuarded() *guardedProvider {
	return &guardedProvider{Provider: fake.New(), vols: map[string]engine.Volume{}}
}

func (g *guardedProvider) List(ctx context.Context) ([]engine.Instance, error) {
	if g.list != nil {
		return g.list, nil
	}
	return g.Provider.List(ctx)
}

func (g *guardedProvider) Launch(ctx context.Context, req engine.LaunchRequest) (engine.Instance, error) {
	live, _ := g.Provider.List(ctx)
	g.mu.Lock()
	for _, inst := range live {
		if inst.Key == req.Key {
			g.overlaps = append(g.overlaps, req.ID+" next to "+inst.ID)
		}
	}
	g.order = append(g.order, "launch "+req.ID)
	g.mu.Unlock()
	return g.Provider.Launch(ctx, req)
}

func (g *guardedProvider) Terminate(ctx context.Context, id, reason string) error {
	g.mu.Lock()
	g.order = append(g.order, "terminate "+id)
	if g.hold {
		g.held = append(g.held, id)
		g.mu.Unlock()
		return nil
	}
	g.mu.Unlock()
	return g.Provider.Terminate(ctx, id, reason)
}

// release lets held terminations finish (the pods disappear).
func (g *guardedProvider) release() {
	g.mu.Lock()
	ids := g.held
	g.held, g.hold = nil, false
	g.mu.Unlock()
	for _, id := range ids {
		_ = g.Provider.Terminate(context.Background(), id, "released")
	}
}

func (g *guardedProvider) heldIDs() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.held...)
}

func (g *guardedProvider) checkNoOverlap(t *testing.T) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.overlaps) > 0 {
		t.Fatalf("engines of one persistent key overlapped: %v (calls: %v)", g.overlaps, g.order)
	}
}

func (g *guardedProvider) Volumes(context.Context) ([]engine.Volume, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]engine.Volume, 0, len(g.vols))
	for _, v := range g.vols {
		out = append(out, v)
	}
	return out, nil
}

func (g *guardedProvider) DeleteVolume(_ context.Context, name string) error {
	g.mu.Lock()
	block, entered := g.block, g.entered
	g.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if block != nil {
		<-block
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.vols[name]; !ok {
		return engine.ErrVolumeNotFound
	}
	delete(g.vols, name)
	return nil
}

// startPersistent starts a manager on g with yaml (persistentConfig if
// empty).
func startPersistent(t *testing.T, g *guardedProvider, clock *testClock, fence func() bool, yaml string) (*engine.Manager, *closerRecorder) {
	t.Helper()
	if yaml == "" {
		yaml = persistentConfig
	}
	cfg, err := config.Parse([]byte("auth: {mode: none}\n" + yaml))
	if err != nil {
		t.Fatal(err)
	}
	m := engine.NewManager(cfg, g, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New())
	m.SetClock(clock.Now)
	rec := &closerRecorder{}
	m.SetSessionCloser(rec)
	if fence != nil {
		m.SetFence(fence)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return m, rec
}

// recoverOutdated registers a ready engine of personalKey with an old spec
// hash, as a restarted gateway finds it after the profile changed.
func recoverOutdated(t *testing.T, g *guardedProvider, clock *testClock) string {
	t.Helper()
	const id = "quack-user-alice-old"
	srv := g.AddReady(id, personalKey, "tok")
	t.Cleanup(srv.Close)
	g.list = []engine.Instance{{
		ID: id, Key: personalKey, ProfileName: "personal", SpecHash: "old", Phase: engine.PhaseReady,
		Endpoint: srv.URL(), Token: "tok", CreatedAt: clock.Now().Add(-time.Hour), User: "alice",
	}}
	return id
}

func acquire(t *testing.T, m *engine.Manager, timeout time.Duration) (*engine.Lease, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return m.Acquire(ctx, personalKey, "alice")
}

func state(m *engine.Manager, id string) engine.State {
	s, _ := m.Exists(id)
	return s
}

// A profile change replaces the engine of a persistent key without overlap:
// the old engine keeps taking sessions, ends once idle, and the successor is
// launched only after it is gone.
func TestPersistentSpecChangeReplacesWithoutOverlap(t *testing.T) {
	g := newGuarded()
	clock := newTestClock()
	old := recoverOutdated(t, g, clock)
	m, _ := startPersistent(t, g, clock, nil, "")
	clock.Advance(10 * time.Minute) // past engine.recoveryGrace

	for i := 0; i < 2; i++ {
		l, err := acquire(t, m, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if l.ID != old {
			t.Fatalf("session %d went to %s, want the outdated engine %s (the only one that may open the data)", i, l.ID, old)
		}
		l.Commit()
	}
	m.Reconcile()
	if v, _ := m.Get(old); v.State != engine.StateReady || !strings.Contains(v.Message, "replacement pending") {
		t.Fatalf("engine with sessions: %s %q", v.State, v.Message)
	}
	m.DetachSession(old)
	m.DetachSession(old)
	g.mu.Lock()
	g.hold = true // the pod takes its time to stop
	g.mu.Unlock()
	m.Reconcile()
	if s := state(m, old); s != engine.StateTerminating {
		t.Fatalf("idle outdated engine is %s, want TERMINATING", s)
	}

	type result struct {
		l   *engine.Lease
		err error
	}
	done := make(chan result, 1)
	go func() {
		l, err := acquire(t, m, 10*time.Second)
		done <- result{l, err}
	}()
	time.Sleep(300 * time.Millisecond)
	if n := g.Launches(); n != 0 {
		t.Fatalf("%d launches while the old engine still exists", n)
	}
	g.release()
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.l.ID == old || g.Launches() != 1 {
		t.Fatalf("new session on %s after %d launches", r.l.ID, g.Launches())
	}
	r.l.Commit()
	g.checkNoOverlap(t)
}

// Sessions that arrive together while the previous engine of a persistent
// key stops share one successor: a single launch, after the old engine is
// gone.
func TestPersistentConcurrentConnectsShareSuccessor(t *testing.T) {
	g := newGuarded()
	clock := newTestClock()
	old := recoverOutdated(t, g, clock)
	m, _ := startPersistent(t, g, clock, nil, "")
	clock.Advance(10 * time.Minute) // past engine.recoveryGrace
	g.mu.Lock()
	g.hold = true
	g.mu.Unlock()
	m.Reconcile() // idle and outdated: terminated, its pod takes a while
	if s := state(m, old); s != engine.StateTerminating {
		t.Fatalf("outdated engine is %s, want TERMINATING", s)
	}
	const n = 5
	ids := make(chan string, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			l, err := acquire(t, m, 10*time.Second)
			if err != nil {
				errs <- err
				return
			}
			l.Commit()
			ids <- l.ID
		}()
	}
	time.Sleep(300 * time.Millisecond)
	g.release()
	first := ""
	for i := 0; i < n; i++ {
		select {
		case err := <-errs:
			t.Fatal(err)
		case id := <-ids:
			if first == "" {
				first = id
			}
			if id != first || id == old {
				t.Fatalf("sessions on %s and %s (old engine %s)", first, id, old)
			}
		}
	}
	if g.Launches() != 1 {
		t.Fatalf("%d launches for one key", g.Launches())
	}
	g.checkNoOverlap(t)
}

// engine.maxLifetime replaces the engine of a persistent key like a profile
// change: it keeps serving (instead of draining) until it is idle.
func TestPersistentMaxLifetimeKeepsServing(t *testing.T) {
	g := newGuarded()
	clock := newTestClock()
	m, _ := startPersistent(t, g, clock, nil, strings.Replace(persistentConfig, "drainTimeout: 10m", "drainTimeout: 10m\n  maxLifetime: 1h", 1))
	l, err := acquire(t, m, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	l.Commit()
	first := l.ID
	clock.Advance(61 * time.Minute)
	m.Reconcile()
	if v, _ := m.Get(first); v.State != engine.StateReady || !strings.Contains(v.Message, "replacement pending: max lifetime") {
		t.Fatalf("engine past its max lifetime: %s %q", v.State, v.Message)
	}
	l, err = acquire(t, m, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if l.ID != first {
		t.Fatalf("a new session went to %s, want %s", l.ID, first)
	}
	l.Release()
	m.DetachSession(first)
	m.Reconcile() // idle now: replaced
	l, err = acquire(t, m, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	l.Commit()
	if l.ID == first || g.Launches() != 2 {
		t.Fatalf("session on %s after %d launches", l.ID, g.Launches())
	}
	g.checkNoOverlap(t)
}

// engine.drainTimeout after the replacement fell due, the sessions of the
// old engine are closed; a new session waits until the engine is gone.
func TestPersistentReplacementDeadline(t *testing.T) {
	g := newGuarded()
	clock := newTestClock()
	old := recoverOutdated(t, g, clock)
	m, rec := startPersistent(t, g, clock, nil, "")
	clock.Advance(10 * time.Minute)
	l, err := acquire(t, m, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	l.Commit()
	m.Reconcile() // replacement pending
	clock.Advance(11 * time.Minute)
	m.Reconcile()
	if s := state(m, old); s != engine.StateDraining {
		t.Fatalf("overdue engine is %s, want DRAINING", s)
	}
	rec.mu.Lock()
	drained := strings.Join(rec.drain, ",")
	rec.mu.Unlock()
	if drained != old {
		t.Fatalf("sessions closed for %q, want %s", drained, old)
	}
	done := make(chan error, 1)
	go func() {
		l, err := acquire(t, m, 10*time.Second)
		if err == nil {
			l.Commit()
		}
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	if g.Launches() != 0 {
		t.Fatal("launched next to the draining engine")
	}
	m.DetachSession(old) // the session manager closed the session
	m.Reconcile()        // drained: terminate (the fake reports it gone)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	g.checkNoOverlap(t)
}

// A failed engine of a persistent key (kept for engine.failedRetention
// otherwise) is terminated as soon as a session needs the key.
func TestPersistentFailedEngineMakesRoom(t *testing.T) {
	g := newGuarded()
	clock := newTestClock()
	srv := g.AddReady("quack-user-alice-failed", personalKey, "tok")
	defer srv.Close()
	g.list = []engine.Instance{{
		ID: "quack-user-alice-failed", Key: personalKey, ProfileName: "personal", Phase: engine.PhaseFailed,
		Token: "tok", CreatedAt: clock.Now(), Message: "pod is being deleted",
	}}
	m, _ := startPersistent(t, g, clock, nil, "")
	l, err := acquire(t, m, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	l.Commit()
	g.mu.Lock()
	order := strings.Join(g.order, ", ")
	g.mu.Unlock()
	if !strings.HasPrefix(order, "terminate quack-user-alice-failed, launch ") {
		t.Fatalf("calls: %s", order)
	}
	g.checkNoOverlap(t)
}

// If the previous engine does not end within the startup budget, the
// connect fails with a precise error and nothing is launched.
func TestPersistentHandoverTimeout(t *testing.T) {
	g := newGuarded()
	clock := newTestClock()
	old := recoverOutdated(t, g, clock)
	m, _ := startPersistent(t, g, clock, nil, strings.Replace(persistentConfig, "startupTimeout: 30s", "startupTimeout: 2s", 1))
	g.mu.Lock()
	g.hold = true // the old pod never goes away (a lost node)
	g.mu.Unlock()
	if err := m.Terminate(old, "admin"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := acquire(t, m, 30*time.Second)
	if !errors.Is(err, engine.ErrHandover) || !strings.Contains(err.Error(), old) {
		t.Fatalf("error %v", err)
	}
	if d := time.Since(start); d < 2*time.Second || d > 10*time.Second {
		t.Errorf("failed after %s, want about the startup timeout (2s)", d)
	}
	if g.Launches() != 0 || len(g.heldIDs()) != 1 {
		t.Fatalf("launches %d, held %v", g.Launches(), g.heldIDs())
	}
}

// Launches for persistent keys need a fresh leader lease.
func TestPersistentLaunchNeedsFreshLease(t *testing.T) {
	g := newGuarded()
	m, _ := startPersistent(t, g, newTestClock(), func() bool { return false }, "")
	if _, err := acquire(t, m, 5*time.Second); !errors.Is(err, engine.ErrNotLeader) {
		t.Fatalf("error %v", err)
	}
	if g.Launches() != 0 {
		t.Fatal("launched without a fresh lease")
	}
}

// A leader that resumes after a freeze leaves the engines of a persistent
// key alone: the new leader may serve them by now. It neither terminates a
// failed engine for a new session nor waits for one, and fails at once.
func TestPersistentStaleLeaderLeavesHolderAlone(t *testing.T) {
	g := newGuarded()
	clock := newTestClock()
	srv := g.AddReady("quack-user-alice-failed", personalKey, "tok")
	defer srv.Close()
	g.list = []engine.Instance{{
		ID: "quack-user-alice-failed", Key: personalKey, ProfileName: "personal", Phase: engine.PhaseFailed,
		Token: "tok", CreatedAt: clock.Now(), Message: "pod is being deleted",
	}}
	var fresh atomic.Bool
	m, _ := startPersistent(t, g, clock, fresh.Load, "")
	start := time.Now()
	if _, err := acquire(t, m, 10*time.Second); !errors.Is(err, engine.ErrNotLeader) {
		t.Fatalf("error %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("a stale leader waited %s for the previous engine", d)
	}
	g.mu.Lock()
	calls := strings.Join(g.order, ", ")
	g.mu.Unlock()
	if calls != "" || state(m, "quack-user-alice-failed") != engine.StateFailed {
		t.Fatalf("a stale leader acted on the engines of the key: %q, state %s", calls, state(m, "quack-user-alice-failed"))
	}
	// With a fresh lease the failed engine makes room as usual.
	fresh.Store(true)
	l, err := acquire(t, m, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	l.Commit()
	g.checkNoOverlap(t)
}

// Before the initial recovery the registry may lack the engine that uses a
// volume: deleting it must wait for that.
func TestDeleteVolumeNeedsSyncedRegistry(t *testing.T) {
	g := newGuarded()
	g.vols["vol-alice"] = engine.Volume{Name: "vol-alice", Key: personalKey.String(), EngineKey: personalKey}
	cfg, err := config.Parse([]byte("auth: {mode: none}\n" + persistentConfig))
	if err != nil {
		t.Fatal(err)
	}
	m := engine.NewManager(cfg, g, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New())
	if _, err := m.DeleteVolume(context.Background(), "vol-alice"); !errors.Is(err, engine.ErrNotSynced) {
		t.Fatalf("delete before the registry is synced: %v", err)
	}
	if _, ok := g.vols["vol-alice"]; !ok {
		t.Fatal("the volume was deleted")
	}
}

func TestDeleteVolume(t *testing.T) {
	g := newGuarded()
	g.vols["vol-alice"] = engine.Volume{Name: "vol-alice", Key: personalKey.String(), EngineKey: personalKey}
	clock := newTestClock()
	m, _ := startPersistent(t, g, clock, nil, "")
	l, err := acquire(t, m, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	l.Commit()
	vols, err := m.Volumes(context.Background())
	if err != nil || len(vols) != 1 || vols[0].Engine != l.ID {
		t.Fatalf("volumes %+v %v", vols, err)
	}
	if _, err := m.DeleteVolume(context.Background(), "vol-alice"); !errors.Is(err, engine.ErrVolumeInUse) {
		t.Fatalf("delete while in use: %v", err)
	}
	m.DetachSession(l.ID)
	if err := m.Terminate(l.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "engine gone", func() bool { _, ok := m.Exists(l.ID); return !ok })

	block, entered := make(chan struct{}), make(chan struct{}, 1)
	g.mu.Lock()
	g.block, g.entered = block, entered
	g.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := m.DeleteVolume(context.Background(), "vol-alice")
		done <- err
	}()
	<-entered // the deletion request is under way
	if _, err := acquire(t, m, time.Second); !errors.Is(err, engine.ErrVolumeDeleting) {
		t.Fatalf("connect during the deletion: %v", err)
	}
	close(block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := m.DeleteVolume(context.Background(), "vol-alice"); !errors.Is(err, engine.ErrVolumeNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	l, err = acquire(t, m, 5*time.Second) // a new (empty) volume is the provider's business
	if err != nil {
		t.Fatal(err)
	}
	l.Commit()
	g.checkNoOverlap(t)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// During engine.recoveryGrace a drained, idle engine found after a restart
// may still have sessions that have not re-attached: a new session waits
// instead of terminating it, and gets it once the grace period is over.
func TestPersistentHolderRespectsRecoveryGrace(t *testing.T) {
	g := newGuarded()
	clock := newTestClock()
	srv := g.AddReady("quack-user-alice-recovered", personalKey, "tok")
	defer srv.Close()
	g.list = []engine.Instance{{
		ID: "quack-user-alice-recovered", Key: personalKey, ProfileName: "personal", Phase: engine.PhaseReady,
		Endpoint: srv.URL(), Token: "tok", CreatedAt: clock.Now().Add(-time.Hour),
	}}
	m, _ := startPersistent(t, g, clock, nil, strings.Replace(persistentConfig, "startupTimeout: 30s", "startupTimeout: 2s", 1))
	if err := m.Drain("quack-user-alice-recovered", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := acquire(t, m, 10*time.Second); !errors.Is(err, engine.ErrHandover) {
		t.Fatalf("connect during the recovery grace: %v", err)
	}
	g.mu.Lock()
	calls := strings.Join(g.order, ", ")
	g.mu.Unlock()
	if calls != "" {
		t.Fatalf("calls during the recovery grace: %s", calls)
	}
	clock.Advance(10 * time.Minute) // past engine.recoveryGrace
	l, err := acquire(t, m, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	l.Commit()
	g.checkNoOverlap(t)
}
