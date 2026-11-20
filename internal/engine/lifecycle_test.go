package engine_test

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
	"quackmux/internal/engine/fake"
	"quackmux/internal/metrics"
)

// testClock is a manually advanced clock.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock { return &testClock{now: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func startManager(t *testing.T, yaml string, prov engine.Provider, now func() time.Time) *engine.Manager {
	t.Helper()
	cfg, err := config.Parse([]byte("auth: {mode: none}\n" + yaml))
	if err != nil {
		t.Fatal(err)
	}
	m := engine.NewManager(cfg, prov, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New())
	if now != nil {
		m.SetClock(now)
	}
	m.SetSessionCloser(&closerRecorder{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return m
}

// listOverride replaces the provider's initial List (what a restarted
// gateway discovers).
type listOverride struct {
	*fake.Provider
	list []engine.Instance
}

func (p *listOverride) List(context.Context) ([]engine.Instance, error) { return p.list, nil }

// After a gateway restart, an engine that has served for hours but is not
// ready at that moment must get a full startup budget, and come back when its
// pod is ready again.
func TestRecoveredNotReadyEngineKeepsStartupBudget(t *testing.T) {
	prov := &listOverride{Provider: fake.New()}
	srv := prov.AddReady("quack-user-alice-abcde", userKey, "tok")
	defer srv.Close()
	clock := newTestClock()
	prov.list = []engine.Instance{{
		ID: "quack-user-alice-abcde", Key: userKey, ProfileName: "default", Phase: engine.PhaseStarting,
		Token: "tok", CreatedAt: clock.Now().Add(-3 * time.Hour), Message: "not ready",
	}}
	m := startManager(t, "", prov, clock.Now)
	clock.Advance(5 * time.Second)
	m.Reconcile()
	if v, _ := m.Get("quack-user-alice-abcde"); v.State != engine.StateStarting {
		t.Fatalf("recovered engine is %s after the first reconcile (%s), want STARTING", v.State, v.Message)
	}
	prov.Emit("quack-user-alice-abcde", engine.PhaseReady, "")
	waitFor(t, "engine ready again", func() bool {
		v, _ := m.Get("quack-user-alice-abcde")
		return v.State == engine.StateReady
	})

	// A recovered engine that stays not ready fails once its budget is used.
	prov2 := &listOverride{Provider: fake.New()}
	srv2 := prov2.AddReady("quack-user-bob-fghij", engine.Key{ShareLevel: engine.ShareUser, Owner: "bob", Subdomain: "default", Profile: "default"}, "tok")
	defer srv2.Close()
	clock2 := newTestClock()
	prov2.list = []engine.Instance{{
		ID: "quack-user-bob-fghij", Key: engine.Key{ShareLevel: engine.ShareUser, Owner: "bob", Subdomain: "default", Profile: "default"},
		ProfileName: "default", Phase: engine.PhaseStarting, Token: "tok", CreatedAt: clock2.Now().Add(-time.Hour),
	}}
	m2 := startManager(t, "engine: {startupTimeout: 60s}\n", prov2, clock2.Now)
	clock2.Advance(61 * time.Second)
	m2.Reconcile()
	if v, _ := m2.Get("quack-user-bob-fghij"); v.State != engine.StateFailed {
		t.Fatalf("engine still %s after its startup budget", v.State)
	}
}

// goneBeforeLaunchReturns models a pod deleted while Launch still runs: its
// Gone event is handled before Launch returns.
type goneBeforeLaunchReturns struct {
	*fake.Provider
	events chan engine.Instance
}

func (p *goneBeforeLaunchReturns) Events() <-chan engine.Instance { return p.events }

func (p *goneBeforeLaunchReturns) Launch(ctx context.Context, req engine.LaunchRequest) (engine.Instance, error) {
	inst := engine.Instance{ID: req.ID, Key: req.Key, ProfileName: req.ProfileName, SpecHash: req.SpecHash,
		Phase: engine.PhaseStarting, Token: req.Token, User: req.User, CreatedAt: time.Now()}
	gone := inst
	gone.Phase, gone.Message = engine.PhaseGone, "pod deleted"
	p.events <- gone
	time.Sleep(100 * time.Millisecond) // e.g. the owner reference PATCH
	return inst, nil
}

func (p *goneBeforeLaunchReturns) Terminate(context.Context, string, string) error { return nil }

// The result of a launch must not register an engine whose Gone event came
// first.
func TestLaunchResultDoesNotReviveDeletedEngine(t *testing.T) {
	prov := &goneBeforeLaunchReturns{Provider: fake.New(), events: make(chan engine.Instance, 16)}
	m := startManager(t, "engine: {startupTimeout: 1s}\n", prov, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _ = m.Acquire(ctx, userKey, "alice")
	time.Sleep(300 * time.Millisecond)
	if views := m.List(); len(views) != 0 {
		t.Fatalf("an engine whose pod is gone was registered again: %+v", views)
	}
}

// launchCounter counts launch attempts, including failed ones.
type launchCounter struct {
	*fake.Provider
	attempts atomic.Int64
}

func (p *launchCounter) Launch(ctx context.Context, req engine.LaunchRequest) (engine.Instance, error) {
	p.attempts.Add(1)
	return p.Provider.Launch(ctx, req)
}

// A failed launch must not leave an entry behind (it would be terminated
// again and again).
func TestFailedLaunchIsForgotten(t *testing.T) {
	prov := &launchCounter{Provider: fake.New()}
	prov.FailLaunch = errors.New("pods is forbidden: exceeded quota")
	m := startManager(t, "", prov, nil)
	if _, err := m.Acquire(context.Background(), userKey, "alice"); err == nil {
		t.Fatal("Acquire succeeded although the launch failed")
	}
	waitFor(t, "failed engine removed", func() bool { return len(m.List()) == 0 })
}

// A warm pool whose engines cannot start backs off instead of launching on
// every reconcile.
func TestWarmPoolBacksOffAfterFailedStarts(t *testing.T) {
	prov := &launchCounter{Provider: fake.New()}
	prov.FailLaunch = errors.New("pods is forbidden: exceeded quota")
	clock := newTestClock()
	m := startManager(t, "profiles:\n  default:\n    warmPool: {size: 1}\n", prov, clock.Now)
	m.Reconcile()
	waitFor(t, "first warm launch attempt", func() bool { return prov.attempts.Load() == 1 })
	waitFor(t, "failed warm engine removed", func() bool { return len(m.List()) == 0 })
	for i := 0; i < 5; i++ { // 25s: within the first back-off
		clock.Advance(5 * time.Second)
		m.Reconcile()
	}
	time.Sleep(50 * time.Millisecond)
	if n := prov.attempts.Load(); n != 1 {
		t.Fatalf("%d warm launch attempts within the back-off, want 1", n)
	}
	clock.Advance(10 * time.Second)
	m.Reconcile()
	waitFor(t, "warm launch retried after the back-off", func() bool { return prov.attempts.Load() == 2 })
}
