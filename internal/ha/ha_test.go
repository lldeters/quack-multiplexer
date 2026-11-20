package ha

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"quackmux/internal/config"
	"quackmux/internal/metrics"
)

const (
	testNamespace = "quackmux"
	testLease     = "quackmux-test"
	waitTimeout   = 5 * time.Second
)

// Short timings as suggested for tests (client-go requires
// LeaseDuration > RenewDeadline > 1.2 * RetryPeriod).
func shortTimings() config.HAConfig {
	return timings(time.Second, 500*time.Millisecond, 100*time.Millisecond)
}

func timings(lease, renew, retry time.Duration) config.HAConfig {
	return config.HAConfig{
		Enabled:       true,
		LeaseName:     testLease,
		LeaseDuration: config.D(lease),
		RenewDeadline: config.D(renew),
		RetryPeriod:   config.D(retry),
	}
}

// testLogWriter sends log lines to the test log and drops lines written
// after the test finished (client-go reports new leaders asynchronously).
type testLogWriter struct {
	mu   sync.Mutex
	t    *testing.T
	done bool
}

func (w *testLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.done {
		w.t.Log(strings.TrimRight(string(p), "\n"))
	}
	return len(p), nil
}

func testLogger(t *testing.T) *slog.Logger {
	w := &testLogWriter{t: t}
	t.Cleanup(func() {
		w.mu.Lock()
		w.done = true
		w.mu.Unlock()
	})
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func newElector(t *testing.T, client kubernetes.Interface, identity string, cfg config.HAConfig, met *metrics.Metrics) *Elector {
	t.Helper()
	e, err := New(Options{Config: cfg, Client: client, Namespace: testNamespace, Identity: identity, Log: testLogger(t), Metrics: met})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// candidate runs an Elector in the background and records its callbacks.
type candidate struct {
	e        *Elector
	cancel   context.CancelFunc
	done     chan error
	started  chan context.Context
	starts   atomic.Int32
	stops    atomic.Int32
	leaderAt atomic.Bool // IsLeader() as seen by onStoppedLeading
}

// start runs e.Run. If block is set, onStartedLeading blocks until its
// context ends (the usual style); otherwise it returns at once.
func start(t *testing.T, e *Elector, block bool) *candidate {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c := &candidate{e: e, cancel: cancel, done: make(chan error, 1), started: make(chan context.Context, 1)}
	go func() {
		c.done <- e.Run(ctx, func(lctx context.Context) {
			c.starts.Add(1)
			c.started <- lctx
			if block {
				<-lctx.Done()
			}
		}, func() {
			c.leaderAt.Store(e.IsLeader())
			c.stops.Add(1)
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-c.done:
		case <-time.After(waitTimeout):
			t.Errorf("%s: Run did not return after cancel", e.Identity())
		}
	})
	return c
}

func (c *candidate) waitLeading(t *testing.T) context.Context {
	t.Helper()
	select {
	case lctx := <-c.started:
		return lctx
	case err := <-c.done:
		t.Fatalf("%s: Run returned before leading: %v", c.e.Identity(), err)
	case <-time.After(waitTimeout):
		t.Fatalf("%s: did not become leader within %s", c.e.Identity(), waitTimeout)
	}
	return nil
}

func (c *candidate) stop(t *testing.T) error {
	t.Helper()
	c.cancel()
	return c.wait(t)
}

func (c *candidate) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-c.done:
		c.done <- err // keep it for the cleanup
		return err
	case <-time.After(waitTimeout):
		t.Fatalf("%s: Run did not return within %s", c.e.Identity(), waitTimeout)
	}
	return nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", waitTimeout, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func getLease(t *testing.T, client kubernetes.Interface) *coordinationv1.Lease {
	t.Helper()
	l, err := client.CoordinationV1().Leases(testNamespace).Get(context.Background(), testLease, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get lease: %v", err)
	}
	return l
}

func holder(l *coordinationv1.Lease) string {
	if l.Spec.HolderIdentity == nil {
		return ""
	}
	return *l.Spec.HolderIdentity
}

func gauge(t *testing.T, m *metrics.Metrics) float64 {
	t.Helper()
	mfs, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "quackmux_is_leader" {
			return mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatal("quackmux_is_leader not found")
	return 0
}

// ---- configuration --------------------------------------------------------

func TestNew(t *testing.T) {
	client := fake.NewSimpleClientset()
	base := func() Options {
		return Options{Config: shortTimings(), Client: client, Namespace: testNamespace, Identity: "gw-0"}
	}
	tests := []struct {
		name    string
		mutate  func(o *Options)
		wantErr string
		check   func(t *testing.T, e *Elector)
	}{
		{
			name: "valid", mutate: func(*Options) {},
			check: func(t *testing.T, e *Elector) {
				if e.Lease() != testNamespace+"/"+testLease || e.Identity() != "gw-0" {
					t.Errorf("lease %q identity %q", e.Lease(), e.Identity())
				}
				if e.leaseDuration != time.Second || e.renewDeadline != 500*time.Millisecond || e.retryPeriod != 100*time.Millisecond {
					t.Errorf("timings %s/%s/%s", e.leaseDuration, e.renewDeadline, e.retryPeriod)
				}
				if e.watchInterval != 50*time.Millisecond {
					t.Errorf("watchInterval = %s", e.watchInterval)
				}
				if e.IsLeader() || e.Leader() != "" {
					t.Error("a new elector must not be leader")
				}
			},
		},
		{
			name: "defaults for an empty config", mutate: func(o *Options) { o.Config = config.HAConfig{} },
			check: func(t *testing.T, e *Elector) {
				if e.Lease() != testNamespace+"/"+DefaultLeaseName {
					t.Errorf("lease %q", e.Lease())
				}
				if e.leaseDuration != DefaultLeaseDuration || e.renewDeadline != DefaultRenewDeadline || e.retryPeriod != DefaultRetryPeriod {
					t.Errorf("timings %s/%s/%s", e.leaseDuration, e.renewDeadline, e.retryPeriod)
				}
				if e.watchInterval != time.Second {
					t.Errorf("watchInterval = %s", e.watchInterval)
				}
			},
		},
		{
			name: "explicit zero durations get defaults",
			mutate: func(o *Options) {
				o.Config.LeaseDuration, o.Config.RenewDeadline, o.Config.RetryPeriod = config.D(0), config.D(0), config.D(0)
			},
			check: func(t *testing.T, e *Elector) {
				if e.leaseDuration != DefaultLeaseDuration || e.renewDeadline != DefaultRenewDeadline || e.retryPeriod != DefaultRetryPeriod {
					t.Errorf("timings %s/%s/%s", e.leaseDuration, e.renewDeadline, e.retryPeriod)
				}
			},
		},
		{
			name: "config defaults are valid",
			mutate: func(o *Options) {
				var c config.Config
				c.ApplyDefaults()
				o.Config = c.HA
			},
			check: func(t *testing.T, e *Elector) {
				if e.Lease() != testNamespace+"/quackmux-default" {
					t.Errorf("lease %q", e.Lease())
				}
			},
		},
		{
			name: "ha.namespace wins", mutate: func(o *Options) { o.Config.Namespace = "other" },
			check: func(t *testing.T, e *Elector) {
				if e.Lease() != "other/"+testLease {
					t.Errorf("lease %q", e.Lease())
				}
			},
		},
		{
			name: "namespace argument as fallback", mutate: func(o *Options) { o.Config.Namespace = " " },
			check: func(t *testing.T, e *Elector) {
				if e.Lease() != testNamespace+"/"+testLease {
					t.Errorf("lease %q", e.Lease())
				}
			},
		},
		{
			name: "identity is trimmed", mutate: func(o *Options) { o.Identity = " gw-1\n" },
			check: func(t *testing.T, e *Elector) {
				if e.Identity() != "gw-1" {
					t.Errorf("identity %q", e.Identity())
				}
			},
		},
		{name: "missing client", mutate: func(o *Options) { o.Client = nil }, wantErr: "client"},
		{name: "missing namespace", mutate: func(o *Options) { o.Namespace = "" }, wantErr: "no namespace"},
		{name: "invalid namespace", mutate: func(o *Options) { o.Namespace = "Not_Valid" }, wantErr: "invalid lease namespace"},
		{name: "invalid lease name", mutate: func(o *Options) { o.Config.LeaseName = "has spaces" }, wantErr: "invalid lease name"},
		{
			name:    "lease duration below one second",
			mutate:  func(o *Options) { o.Config = timings(800*time.Millisecond, 500*time.Millisecond, 100*time.Millisecond) },
			wantErr: "at least 1s",
		},
		{
			name: "fractional lease duration",
			mutate: func(o *Options) {
				o.Config = timings(1500*time.Millisecond, 500*time.Millisecond, 100*time.Millisecond)
			},
			wantErr: "whole number of seconds",
		},
		{
			name:    "renew deadline not shorter than lease duration",
			mutate:  func(o *Options) { o.Config = timings(time.Second, time.Second, 100*time.Millisecond) },
			wantErr: "must be shorter than ha.leaseDuration",
		},
		{
			name:    "renew deadline too close to retry period",
			mutate:  func(o *Options) { o.Config = timings(2*time.Second, time.Second, 900*time.Millisecond) },
			wantErr: "must exceed 1.2 x ha.retryPeriod",
		},
		{
			name:    "negative renew deadline",
			mutate:  func(o *Options) { o.Config = timings(time.Second, -time.Second, 100*time.Millisecond) },
			wantErr: "ha.renewDeadline (-1s) must be positive",
		},
		{
			name:    "negative retry period",
			mutate:  func(o *Options) { o.Config = timings(time.Second, 500*time.Millisecond, -time.Second) },
			wantErr: "ha.retryPeriod (-1s) must be positive",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := base()
			tc.mutate(&o)
			e, err := New(o)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("New() error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			tc.check(t, e)
		})
	}
}

func TestDefaultIdentity(t *testing.T) {
	t.Run("POD_NAME", func(t *testing.T) {
		t.Setenv(PodNameEnv, "quackmux-7d4f9-abcde")
		e, err := New(Options{Client: fake.NewSimpleClientset(), Namespace: testNamespace})
		if err != nil {
			t.Fatal(err)
		}
		if e.Identity() != "quackmux-7d4f9-abcde" {
			t.Errorf("identity = %q", e.Identity())
		}
	})
	t.Run("host name", func(t *testing.T) {
		t.Setenv(PodNameEnv, "")
		host, err := os.Hostname()
		if err != nil {
			t.Skipf("no host name: %v", err)
		}
		id, err := DefaultIdentity()
		if err != nil || id != strings.TrimSpace(host) {
			t.Errorf("DefaultIdentity() = %q, %v; want %q", id, err, host)
		}
	})
}

// ---- election ---------------------------------------------------------------

func TestSingleCandidateLeadsAndReleases(t *testing.T) {
	client := fake.NewSimpleClientset()
	met := metrics.New()
	e := newElector(t, client, "gw-a", shortTimings(), met)
	if gauge(t, met) != 0 {
		t.Fatal("gauge must start at 0")
	}

	c := start(t, e, true)
	lctx := c.waitLeading(t)
	if !e.IsLeader() || e.Leader() != "gw-a" || gauge(t, met) != 1 {
		t.Fatalf("leading: IsLeader=%v Leader=%q gauge=%v", e.IsLeader(), e.Leader(), gauge(t, met))
	}
	l := getLease(t, client)
	if holder(l) != "gw-a" || l.Spec.LeaseDurationSeconds == nil || *l.Spec.LeaseDurationSeconds != 1 {
		t.Fatalf("lease spec = %+v", l.Spec)
	}
	if l.Labels["app.kubernetes.io/managed-by"] != "quackmux" {
		t.Errorf("lease labels = %v", l.Labels)
	}

	// The leader keeps renewing for longer than the renew deadline without
	// stepping down.
	firstRenew := l.Spec.RenewTime.Time
	time.Sleep(2 * e.renewDeadline)
	if lctx.Err() != nil || !e.IsLeader() {
		t.Fatal("leader stepped down while renewals succeed")
	}
	if l := getLease(t, client); !l.Spec.RenewTime.Time.After(firstRenew) || holder(l) != "gw-a" {
		t.Fatalf("lease was not renewed: first %s, now %s (holder %q)", firstRenew, l.Spec.RenewTime.Time, holder(l))
	}

	if err := c.stop(t); err != nil {
		t.Fatalf("Run after cancel = %v, want nil", err)
	}
	if lctx.Err() == nil {
		t.Error("leader context still active after Run returned")
	}
	if n := c.stops.Load(); n != 1 {
		t.Errorf("onStoppedLeading called %d times, want 1", n)
	}
	if c.leaderAt.Load() {
		t.Error("IsLeader was still true in onStoppedLeading")
	}
	if e.IsLeader() || gauge(t, met) != 0 {
		t.Errorf("after stop: IsLeader=%v gauge=%v", e.IsLeader(), gauge(t, met))
	}
	if h := holder(getLease(t, client)); h != "" {
		t.Errorf("lease not released on cancel, holder %q", h)
	}

	// The elector can campaign again.
	c2 := start(t, e, true)
	c2.waitLeading(t)
	if !e.IsLeader() {
		t.Error("not leader in second term")
	}
	if err := c2.stop(t); err != nil {
		t.Fatalf("second Run = %v", err)
	}
	if c2.stops.Load() != 1 || c2.starts.Load() != 1 {
		t.Errorf("second term callbacks: starts %d stops %d", c2.starts.Load(), c2.stops.Load())
	}
}

func TestPackageRun(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan context.Context, 1)
	var stops atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, shortTimings(), client, testNamespace, "gw-run", func(lctx context.Context) {
			started <- lctx
			<-lctx.Done()
		}, func() { stops.Add(1) }, testLogger(t))
	}()
	var lctx context.Context
	select {
	case lctx = <-started:
	case err := <-done:
		t.Fatalf("Run returned early: %v", err)
	case <-time.After(waitTimeout):
		t.Fatal("did not become leader")
	}
	if h := holder(getLease(t, client)); h != "gw-run" {
		t.Fatalf("holder = %q", h)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Run did not return after cancel")
	}
	if lctx.Err() == nil || stops.Load() != 1 {
		t.Errorf("leader ctx err %v, stops %d", lctx.Err(), stops.Load())
	}
}

func TestPackageRunRejectsInvalidConfig(t *testing.T) {
	err := Run(context.Background(), config.HAConfig{}, fake.NewSimpleClientset(), "", "gw", func(context.Context) {}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "namespace") {
		t.Fatalf("Run = %v, want a namespace error", err)
	}
}

func TestFailoverToSecondCandidate(t *testing.T) {
	client := fake.NewSimpleClientset()
	a := newElector(t, client, "gw-a", shortTimings(), nil)
	b := newElector(t, client, "gw-b", shortTimings(), nil)

	ca := start(t, a, true)
	actx := ca.waitLeading(t)
	cb := start(t, b, true)
	waitFor(t, "b to observe a as leader", func() bool { return b.Leader() == "gw-a" })
	time.Sleep(5 * b.retryPeriod)
	if b.IsLeader() || cb.starts.Load() != 0 {
		t.Fatal("second candidate leads while the first holds the lease")
	}

	if err := ca.stop(t); err != nil {
		t.Fatalf("a: Run = %v", err)
	}
	if actx.Err() == nil || ca.stops.Load() != 1 {
		t.Fatalf("a did not stop leading cleanly (ctx err %v, stops %d)", actx.Err(), ca.stops.Load())
	}
	// a released the lease, so b does not have to wait for it to expire.
	if h := holder(getLease(t, client)); h != "" && h != "gw-b" {
		t.Fatalf("holder after a stopped = %q", h)
	}
	cb.waitLeading(t)
	if !b.IsLeader() || b.Leader() != "gw-b" || a.IsLeader() {
		t.Fatalf("after failover: a.IsLeader=%v b.IsLeader=%v b.Leader=%q", a.IsLeader(), b.IsLeader(), b.Leader())
	}
	l := getLease(t, client)
	if holder(l) != "gw-b" || l.Spec.LeaseTransitions == nil || *l.Spec.LeaseTransitions != 1 {
		t.Fatalf("lease after failover: holder %q transitions %v", holder(l), l.Spec.LeaseTransitions)
	}
	if err := cb.stop(t); err != nil {
		t.Fatalf("b: Run = %v", err)
	}
}

func TestNoLeadershipWhileLeaseIsHeld(t *testing.T) {
	now := metav1.NowMicro()
	other, secs := "someone-else", int32(3600)
	client := fake.NewSimpleClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testLease},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: &other, LeaseDurationSeconds: &secs, AcquireTime: &now, RenewTime: &now,
		},
	})
	e := newElector(t, client, "gw-a", shortTimings(), nil)
	c := start(t, e, true)
	waitFor(t, "the current holder to be observed", func() bool { return e.Leader() == other })
	time.Sleep(5 * e.retryPeriod)
	if e.IsLeader() || c.starts.Load() != 0 {
		t.Fatal("took over a lease that has not expired")
	}
	if err := c.stop(t); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if c.starts.Load() != 0 || c.stops.Load() != 0 {
		t.Errorf("callbacks without leadership: starts %d stops %d", c.starts.Load(), c.stops.Load())
	}
	if h := holder(getLease(t, client)); h != other {
		t.Errorf("a follower released somebody else's lease: holder %q", h)
	}
}

func TestStepDownWhenRenewalFails(t *testing.T) {
	cfg := timings(2*time.Second, time.Second, 200*time.Millisecond)
	client := fake.NewSimpleClientset()
	var failing atomic.Bool
	client.PrependReactor("*", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failing.Load() {
			return true, nil, errors.New("injected: API server unreachable")
		}
		return false, nil, nil
	})
	met := metrics.New()
	e := newElector(t, client, "gw-a", cfg, met)
	c := start(t, e, true)
	lctx := c.waitLeading(t)

	failedAt := time.Now()
	failing.Store(true)
	select {
	case <-lctx.Done():
	case <-time.After(waitTimeout):
		t.Fatal("leader context not cancelled after renewals failed")
	}
	// Another candidate may take over LeaseDuration after the last renewal,
	// so the leader must have stopped before that.
	if elapsed := time.Since(failedAt); elapsed >= e.leaseDuration {
		t.Errorf("leader stepped down %s after the API failed, want < leaseDuration %s", elapsed, e.leaseDuration)
	} else {
		t.Logf("stepped down %s after the API failed", elapsed.Round(time.Millisecond))
	}
	err := c.wait(t)
	if !errors.Is(err, ErrLeadershipLost) {
		t.Fatalf("Run = %v, want ErrLeadershipLost", err)
	}
	if e.IsLeader() || gauge(t, met) != 0 || c.stops.Load() != 1 || c.leaderAt.Load() {
		t.Errorf("after loss: IsLeader=%v gauge=%v stops=%d leaderInStop=%v", e.IsLeader(), gauge(t, met), c.stops.Load(), c.leaderAt.Load())
	}
}

func TestCallbackMayReturnEarly(t *testing.T) {
	client := fake.NewSimpleClientset()
	e := newElector(t, client, "gw-a", shortTimings(), nil)
	c := start(t, e, false)
	lctx := c.waitLeading(t)
	time.Sleep(3 * e.retryPeriod)
	if !e.IsLeader() || lctx.Err() != nil || c.stops.Load() != 0 {
		t.Fatalf("leadership ended with the callback: IsLeader=%v ctxErr=%v stops=%d", e.IsLeader(), lctx.Err(), c.stops.Load())
	}
	if err := c.stop(t); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if c.stops.Load() != 1 || lctx.Err() == nil {
		t.Errorf("stops %d, ctx err %v", c.stops.Load(), lctx.Err())
	}
}

func TestRunArguments(t *testing.T) {
	t.Run("nil onStartedLeading", func(t *testing.T) {
		e := newElector(t, fake.NewSimpleClientset(), "gw-a", shortTimings(), nil)
		if err := e.Run(context.Background(), nil, func() {}); err == nil {
			t.Fatal("Run accepted a nil onStartedLeading")
		}
	})
	t.Run("nil onStoppedLeading is fine", func(t *testing.T) {
		e := newElector(t, fake.NewSimpleClientset(), "gw-a", shortTimings(), nil)
		ctx, cancel := context.WithCancel(context.Background())
		started := make(chan struct{})
		done := make(chan error, 1)
		go func() { done <- e.Run(ctx, func(context.Context) { close(started) }, nil) }()
		select {
		case <-started:
		case <-time.After(waitTimeout):
			t.Fatal("did not become leader")
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run = %v", err)
		}
	})
	t.Run("cancelled context", func(t *testing.T) {
		e := newElector(t, fake.NewSimpleClientset(), "gw-a", shortTimings(), nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var called atomic.Bool
		if err := e.Run(ctx, func(context.Context) { called.Store(true) }, func() { called.Store(true) }); err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
		if called.Load() || e.IsLeader() {
			t.Fatal("callbacks ran for a cancelled context")
		}
	})
	t.Run("concurrent Run", func(t *testing.T) {
		e := newElector(t, fake.NewSimpleClientset(), "gw-a", shortTimings(), nil)
		c := start(t, e, true)
		c.waitLeading(t)
		err := e.Run(context.Background(), func(context.Context) {}, nil)
		if err == nil || !strings.Contains(err.Error(), "already running") {
			t.Fatalf("second Run = %v", err)
		}
		if !e.IsLeader() {
			t.Fatal("rejected Run disturbed the running one")
		}
		if err := c.stop(t); err != nil {
			t.Fatalf("Run = %v", err)
		}
	})
}

// ---- renewal watchdog ---------------------------------------------------

type fakeLock struct {
	identity string
	err      error
	calls    int
}

func (f *fakeLock) Get(context.Context) (*resourcelock.LeaderElectionRecord, []byte, error) {
	return nil, nil, errors.New("not implemented")
}
func (f *fakeLock) Create(context.Context, resourcelock.LeaderElectionRecord) error {
	f.calls++
	return f.err
}
func (f *fakeLock) Update(context.Context, resourcelock.LeaderElectionRecord) error {
	f.calls++
	return f.err
}
func (f *fakeLock) RecordEvent(string) {}
func (f *fakeLock) Identity() string   { return f.identity }
func (f *fakeLock) Describe() string   { return "fake" }
func (f *fakeLock) setErr(err error)   { f.err = err }

func TestRenewTracker(t *testing.T) {
	ctx := context.Background()
	rec := func(h string) resourcelock.LeaderElectionRecord {
		return resourcelock.LeaderElectionRecord{HolderIdentity: h}
	}
	tests := []struct {
		name     string
		op       func(tr *renewTracker, fl *fakeLock) error
		recorded bool
	}{
		{"successful update as holder", func(tr *renewTracker, _ *fakeLock) error { return tr.Update(ctx, rec("me")) }, true},
		{"successful create as holder", func(tr *renewTracker, _ *fakeLock) error { return tr.Create(ctx, rec("me")) }, true},
		{"release does not count", func(tr *renewTracker, _ *fakeLock) error { return tr.Update(ctx, rec("")) }, false},
		{"other holder does not count", func(tr *renewTracker, _ *fakeLock) error { return tr.Update(ctx, rec("you")) }, false},
		{"failed update does not count", func(tr *renewTracker, fl *fakeLock) error {
			fl.setErr(errors.New("boom"))
			return tr.Update(ctx, rec("me"))
		}, false},
		{"failed create does not count", func(tr *renewTracker, fl *fakeLock) error {
			fl.setErr(errors.New("boom"))
			return tr.Create(ctx, rec("me"))
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fl := &fakeLock{identity: "me"}
			tr := newRenewTracker(fl)
			if _, ok := tr.sinceRenew(); ok {
				t.Fatal("fresh tracker reports a renewal")
			}
			err := tc.op(tr, fl)
			if (err != nil) != (fl.err != nil) || fl.calls != 1 {
				t.Fatalf("op error %v (lock err %v), calls %d", err, fl.err, fl.calls)
			}
			d, ok := tr.sinceRenew()
			if ok != tc.recorded {
				t.Fatalf("recorded = %v, want %v", ok, tc.recorded)
			}
			if ok && (d < 0 || d > time.Second) {
				t.Errorf("sinceRenew = %s", d)
			}
		})
	}

	t.Run("an older write does not replace a newer one", func(t *testing.T) {
		tr := newRenewTracker(&fakeLock{identity: "me"})
		tr.observe(2*time.Second, rec("me"), nil)
		tr.observe(time.Second, rec("me"), nil)
		if got := time.Duration(tr.last.Load()); got != 2*time.Second {
			t.Errorf("last = %s, want 2s", got)
		}
		tr.observe(3*time.Second, rec("me"), nil)
		if got := time.Duration(tr.last.Load()); got != 3*time.Second {
			t.Errorf("last = %s, want 3s", got)
		}
	})
}

func TestWatchdog(t *testing.T) {
	newE := func(t *testing.T) *Elector {
		e := newElector(t, fake.NewSimpleClientset(), "me", timings(time.Second, 200*time.Millisecond, 50*time.Millisecond), nil)
		e.watchInterval = 10 * time.Millisecond
		return e
	}
	// trackerRenewedAgo returns a tracker whose last renewal started ago.
	trackerRenewedAgo := func(ago time.Duration) *renewTracker {
		tr := &renewTracker{Interface: &fakeLock{identity: "me"}, base: time.Now().Add(-time.Hour)}
		tr.last.Store(int64(time.Since(tr.base) - ago))
		return tr
	}

	t.Run("steps down when renewals are overdue", func(t *testing.T) {
		e := newE(t)
		e.setLeader(true)
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		done := make(chan struct{})
		go func() {
			defer close(done)
			e.watch(ctx, trackerRenewedAgo(time.Second), cancel)
		}()
		select {
		case <-done:
		case <-time.After(waitTimeout):
			t.Fatal("watchdog did not fire")
		}
		if cause := context.Cause(ctx); !errors.Is(cause, ErrLeadershipLost) || !strings.Contains(cause.Error(), "not renewed") {
			t.Errorf("cause = %v", cause)
		}
		if e.IsLeader() {
			t.Error("still leader after stepping down")
		}
	})

	t.Run("stays while renewals succeed", func(t *testing.T) {
		e := newE(t)
		e.setLeader(true)
		tr := trackerRenewedAgo(0)
		ctx, cancel := context.WithCancelCause(context.Background())
		stop := make(chan struct{})
		renewerDone := make(chan struct{})
		go func() { // renew like client-go does, every retry period
			defer close(renewerDone)
			tk := time.NewTicker(e.retryPeriod)
			defer tk.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tk.C:
					_ = tr.Update(context.Background(), resourcelock.LeaderElectionRecord{HolderIdentity: "me"})
				}
			}
		}()
		done := make(chan struct{})
		go func() {
			defer close(done)
			e.watch(ctx, tr, cancel)
		}()
		time.Sleep(4 * e.renewDeadline)
		if ctx.Err() != nil {
			t.Fatalf("watchdog fired although renewals succeed: %v", context.Cause(ctx))
		}
		if !e.IsLeader() {
			t.Fatal("leader flag cleared while leading")
		}
		close(stop)
		<-renewerDone
		cancel(nil)
		<-done
		if e.IsLeader() {
			t.Error("leader flag not cleared when the term ended")
		}
	})

	t.Run("ignores a tracker without renewals", func(t *testing.T) {
		e := newE(t)
		ctx, cancel := context.WithCancelCause(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			e.watch(ctx, newRenewTracker(&fakeLock{identity: "me"}), cancel)
		}()
		time.Sleep(2 * e.renewDeadline)
		if ctx.Err() != nil {
			t.Fatalf("watchdog fired without any renewal data: %v", context.Cause(ctx))
		}
		cancel(nil)
		<-done
	})
}

// Validate ("quackmux validate") rejects what New would, without a cluster;
// in particular the lease name derived from an instance that is a valid
// label value but no object name.
func TestValidate(t *testing.T) {
	cfg, err := config.Parse([]byte("auth: {mode: none}\nserver: {sessionKeyFile: /x}\nha: {enabled: true}\nprovider: {kubernetes: {instance: Prod}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(cfg.HA); err == nil || !strings.Contains(err.Error(), "ha.leaseName") {
		t.Fatalf("lease name %q: %v", cfg.HA.LeaseName, err)
	}
	cfg.HA.LeaseName = "quackmux-prod"
	if err := Validate(cfg.HA); err != nil {
		t.Fatalf("explicit lease name: %v", err)
	}
	bad := cfg.HA
	bad.Namespace = "Not_A_Namespace"
	if err := Validate(bad); err == nil {
		t.Error("invalid namespace accepted")
	}
	bad = cfg.HA
	bad.RenewDeadline = bad.LeaseDuration
	if err := Validate(bad); err == nil {
		t.Error("renewDeadline = leaseDuration accepted")
	}
}
