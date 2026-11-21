package engine_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"quackmux/internal/config"
	"quackmux/internal/engine"
	"quackmux/internal/engine/fake"
	"quackmux/internal/metrics"
)

type closerRecorder struct {
	mu    sync.Mutex
	gone  []string
	drain []string
}

func (c *closerRecorder) EngineGone(id, reason string) {
	c.mu.Lock()
	c.gone = append(c.gone, id)
	c.mu.Unlock()
}

func (c *closerRecorder) DrainEngine(id, reason string) {
	c.mu.Lock()
	c.drain = append(c.drain, id)
	c.mu.Unlock()
}

func newManager(t *testing.T, yaml string, prov *fake.Provider) (*engine.Manager, *closerRecorder) {
	t.Helper()
	cfg, err := config.Parse([]byte("auth: {mode: none}\n" + yaml))
	if err != nil {
		t.Fatal(err)
	}
	m := engine.NewManager(cfg, prov, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New())
	rec := &closerRecorder{}
	m.SetSessionCloser(rec)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		for _, id := range prov.IDs() {
			_ = prov.Terminate(context.Background(), id, "cleanup")
		}
	})
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return m, rec
}

// waitFor polls cond until it holds. The deadline is generous: under the race
// detector and a loaded machine a few goroutine hops can take seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

var userKey = engine.Key{ShareLevel: engine.ShareUser, Owner: "alice", Subdomain: "default", Profile: "default"}

func TestAcquireBeforeStartFails(t *testing.T) {
	cfg, _ := config.Parse([]byte("auth: {mode: none}\n"))
	m := engine.NewManager(cfg, fake.New(), slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if _, err := m.Acquire(context.Background(), userKey, "alice"); !errors.Is(err, engine.ErrNotSynced) {
		t.Fatalf("got %v", err)
	}
}

func TestCancelledWaiterReleasesReservation(t *testing.T) {
	prov := fake.New()
	prov.StartDelay = 300 * time.Millisecond
	m, _ := newManager(t, "", prov)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := m.Acquire(ctx, userKey, "alice"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	views := m.List()
	if len(views) != 1 || views[0].Reserved != 0 || views[0].State != engine.StateStarting {
		t.Fatalf("reservation not released or launch aborted: %+v", views)
	}
	// The launch continues; a later Acquire gets the same engine.
	lease, err := m.Acquire(context.Background(), userKey, "alice")
	if err != nil {
		t.Fatal(err)
	}
	lease.Commit()
	lease.Commit() // idempotent
	if prov.Launches() != 1 {
		t.Fatalf("launches = %d", prov.Launches())
	}
	if v, _ := m.Get(lease.ID); v.Sessions != 1 || v.Reserved != 0 {
		t.Fatalf("accounting %+v", v)
	}
}

func TestReleaseDoesNotAttach(t *testing.T) {
	m, _ := newManager(t, "", fake.New())
	lease, err := m.Acquire(context.Background(), userKey, "alice")
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	lease.Commit() // no effect after Release
	if v, _ := m.Get(lease.ID); v.Sessions != 0 || v.Reserved != 0 {
		t.Fatalf("accounting %+v", v)
	}
}

func TestFailedStartupIsReported(t *testing.T) {
	prov := fake.New()
	prov.NeverReady = true
	m, _ := newManager(t, "engine: {startupTimeout: 1s, failedRetention: 0s}\n", prov)
	start := time.Now()
	_, err := m.Acquire(context.Background(), userKey, "alice")
	if !errors.Is(err, engine.ErrStartTimeout) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("startup timeout not enforced")
	}
	// The reconcile loop marks it failed and removes it.
	time.Sleep(1100 * time.Millisecond)
	m.Reconcile() // STARTING beyond timeout -> FAILED
	m.Reconcile() // FAILED with retention 0 -> TERMINATING
	waitFor(t, "removal", func() bool { return len(m.List()) == 0 })
}

func TestEngineGoneNotifiesSessions(t *testing.T) {
	prov := fake.New()
	m, rec := newManager(t, "", prov)
	lease, err := m.Acquire(context.Background(), userKey, "alice")
	if err != nil {
		t.Fatal(err)
	}
	lease.Commit()
	prov.Crash(lease.ID)
	waitFor(t, "notification", func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.gone) == 1 && rec.gone[0] == lease.ID
	})
	if _, ok := m.Lookup(lease.ID); ok {
		t.Fatal("crashed engine still routable")
	}
}

func TestLimitsCountEngines(t *testing.T) {
	m, _ := newManager(t, "limits: {maxEngines: 2}\n", fake.New())
	for _, owner := range []string{"a", "b"} {
		k := userKey
		k.Owner = owner
		if _, err := m.Acquire(context.Background(), k, owner); err != nil {
			t.Fatal(err)
		}
	}
	k := userKey
	k.Owner = "c"
	if _, err := m.Acquire(context.Background(), k, "c"); !errors.Is(err, engine.ErrLimit) {
		t.Fatalf("got %v", err)
	}
}

func TestDrainAndTerminate(t *testing.T) {
	m, rec := newManager(t, "", fake.New())
	lease, err := m.Acquire(context.Background(), userKey, "alice")
	if err != nil {
		t.Fatal(err)
	}
	lease.Commit()
	if err := m.Drain(lease.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if v, _ := m.Get(lease.ID); v.State != engine.StateDraining {
		t.Fatalf("state %s", v.State)
	}
	// Still routable for existing sessions, but new sessions get a new engine.
	if _, ok := m.Lookup(lease.ID); !ok {
		t.Fatal("draining engine must stay routable")
	}
	l2, err := m.Acquire(context.Background(), userKey, "alice")
	if err != nil || l2.ID == lease.ID {
		t.Fatalf("new session went to draining engine: %v", err)
	}
	if err := m.Terminate(lease.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "termination", func() bool { _, ok := m.Get(lease.ID); return !ok })
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.gone) == 0 {
		t.Fatal("sessions of a terminated engine must be closed")
	}
	if err := m.Drain("nope", "x"); err == nil {
		t.Fatal("unknown engine accepted")
	}
}
