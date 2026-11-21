// Package ha elects a leader among gateway replicas on a Kubernetes Lease
// (coordination.k8s.io/v1). Only the leader serves Quack traffic and manages
// engines; the other replicas stay unready until they acquire the lease.
//
// A leader that loses the lease steps down: the context handed to its
// onStartedLeading callback is cancelled and Run returns ErrLeadershipLost.
// The gateway then exits and restarts as a follower, and the new leader
// takes over engines and sessions by discovery and adoption (see
// docs/design.md, section 6). On an orderly shutdown (the context passed to
// Run is cancelled) the lease is released so that a follower can take over
// at once instead of waiting for the lease to expire.
package ha

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"

	"quackmux/internal/config"
	"quackmux/internal/metrics"
)

// ErrLeadershipLost is returned by Run when this replica could not keep the
// lease while its context was still active.
var ErrLeadershipLost = errors.New("leadership lost")

// Defaults for unset fields of config.HAConfig (config.ApplyDefaults sets
// the same values).
const (
	DefaultLeaseName     = "quackmux"
	DefaultLeaseDuration = 15 * time.Second
	DefaultRenewDeadline = 10 * time.Second
	DefaultRetryPeriod   = 2 * time.Second
)

// PodNameEnv is consulted for the default identity (set it from the
// downward API: fieldRef metadata.name).
const PodNameEnv = "POD_NAME"

// Options configure an Elector.
type Options struct {
	// Config holds the lease name, namespace and timings. Unset or zero
	// fields get the defaults above.
	Config config.HAConfig
	// Client talks to the Kubernetes API. Leader election needs get, create
	// and update on leases in the lease namespace.
	Client kubernetes.Interface
	// Namespace of the lease when Config.Namespace is empty (normally the
	// gateway's own namespace).
	Namespace string
	// Identity names this candidate in the lease and must be unique among
	// the replicas. Default: $POD_NAME, else the host name.
	Identity string
	// Log receives election events; nil discards them.
	Log *slog.Logger
	// Metrics, if set, gets its IsLeader gauge updated with the leadership
	// of this replica.
	Metrics *metrics.Metrics
}

// Elector campaigns for the lease. Its methods are safe for concurrent use;
// Run may be called again after it returned, but not concurrently.
type Elector struct {
	client        kubernetes.Interface
	namespace     string
	name          string
	identity      string
	leaseDuration time.Duration
	renewDeadline time.Duration
	retryPeriod   time.Duration
	// watchInterval is the period of the renewal watchdog.
	watchInterval time.Duration
	// campaignDelay is how long the first Run waits before it campaigns.
	campaignDelay time.Duration
	log           *slog.Logger
	// clientLog receives client-go's leader election messages.
	clientLog logr.Logger
	met       *metrics.Metrics

	running   atomic.Bool
	campaigns atomic.Bool // set by the first Run
	elector   atomic.Pointer[leaderelection.LeaderElector]
	// renew tracks the renewals of the current Run (see Fresh).
	renew atomic.Pointer[renewTracker]

	leaderMu sync.Mutex
	leader   atomic.Bool
}

// Run campaigns for the lease described by cfg until ctx is cancelled or
// leadership is lost. See Elector.Run for the callback contract. namespace
// is used when cfg.Namespace is empty; an empty identity defaults to
// $POD_NAME or the host name.
func Run(ctx context.Context, cfg config.HAConfig, client kubernetes.Interface, namespace, identity string,
	onStartedLeading func(ctx context.Context), onStoppedLeading func(), log *slog.Logger) error {
	e, err := New(Options{Config: cfg, Client: client, Namespace: namespace, Identity: identity, Log: log})
	if err != nil {
		return err
	}
	return e.Run(ctx, onStartedLeading, onStoppedLeading)
}

// Validate checks the settings that need no cluster (for "quackmux
// validate"): the lease name, an explicit namespace and the timings.
func Validate(c config.HAConfig) error {
	if ns := strings.TrimSpace(c.Namespace); ns != "" {
		if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
			return fmt.Errorf("ha: invalid lease namespace %q: %s", ns, strings.Join(errs, "; "))
		}
	}
	if name := leaseName(c); len(validation.IsDNS1123Subdomain(name)) > 0 {
		// The default name is derived from provider.kubernetes.instance,
		// which may be a label value that is no object name ("Prod").
		return fmt.Errorf("ha: invalid lease name %q: %s (set ha.leaseName)", name,
			strings.Join(validation.IsDNS1123Subdomain(name), "; "))
	}
	return validateTimings(durationOr(c.LeaseDuration, DefaultLeaseDuration),
		durationOr(c.RenewDeadline, DefaultRenewDeadline), durationOr(c.RetryPeriod, DefaultRetryPeriod))
}

func leaseName(c config.HAConfig) string {
	if name := strings.TrimSpace(c.LeaseName); name != "" {
		return name
	}
	return DefaultLeaseName
}

// New validates the options and creates an Elector. It does not contact the
// Kubernetes API.
func New(o Options) (*Elector, error) {
	if o.Client == nil {
		return nil, errors.New("ha: a Kubernetes client is required")
	}
	ns := strings.TrimSpace(o.Config.Namespace)
	if ns == "" {
		ns = strings.TrimSpace(o.Namespace)
	}
	if ns == "" {
		return nil, errors.New("ha: no namespace for the lease (set ha.namespace)")
	}
	if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
		return nil, fmt.Errorf("ha: invalid lease namespace %q: %s", ns, strings.Join(errs, "; "))
	}
	if err := Validate(o.Config); err != nil {
		return nil, err
	}
	name := leaseName(o.Config)
	identity := strings.TrimSpace(o.Identity)
	if identity == "" {
		var err error
		if identity, err = DefaultIdentity(); err != nil {
			return nil, err
		}
	}

	lease := durationOr(o.Config.LeaseDuration, DefaultLeaseDuration)
	renew := durationOr(o.Config.RenewDeadline, DefaultRenewDeadline)
	retry := durationOr(o.Config.RetryPeriod, DefaultRetryPeriod)

	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = log.With("component", "ha", "lease", ns+"/"+name, "identity", identity)
	e := &Elector{
		client:        o.Client,
		namespace:     ns,
		name:          name,
		identity:      identity,
		leaseDuration: lease,
		renewDeadline: renew,
		retryPeriod:   retry,
		watchInterval: watchInterval(lease, renew, retry),
		campaignDelay: 3 * retry,
		log:           log,
		// client-go's own messages are chatty; demote its info messages one
		// level below Info (errors stay errors).
		clientLog: logr.FromSlogHandler(log.Handler()).WithName("leaderelection").V(1),
		met:       o.Metrics,
	}
	e.setLeader(false)
	return e, nil
}

// DefaultIdentity returns $POD_NAME, or the host name if it is not set.
func DefaultIdentity() (string, error) {
	if v := strings.TrimSpace(os.Getenv(PodNameEnv)); v != "" {
		return v, nil
	}
	h, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("ha: determine identity: %w", err)
	}
	if h = strings.TrimSpace(h); h == "" {
		return "", errors.New("ha: determine identity: empty host name (set " + PodNameEnv + ")")
	}
	return h, nil
}

func durationOr(d config.Duration, def time.Duration) time.Duration {
	if !d.IsSet() || d.Duration == 0 {
		return def
	}
	return d.Duration
}

// validateTimings applies client-go's rules with configuration-level
// messages. The Lease stores its duration in whole seconds, so a fractional
// duration would be truncated and other candidates could take over before
// this one gives up.
func validateTimings(lease, renew, retry time.Duration) error {
	switch {
	case lease < time.Second:
		return fmt.Errorf("ha.leaseDuration (%s) must be at least 1s", lease)
	case lease%time.Second != 0:
		return fmt.Errorf("ha.leaseDuration (%s) must be a whole number of seconds (the Lease stores seconds)", lease)
	case renew <= 0:
		return fmt.Errorf("ha.renewDeadline (%s) must be positive", renew)
	case retry <= 0:
		return fmt.Errorf("ha.retryPeriod (%s) must be positive", retry)
	case renew >= lease:
		return fmt.Errorf("ha.renewDeadline (%s) must be shorter than ha.leaseDuration (%s)", renew, lease)
	case renew <= time.Duration(leaderelection.JitterFactor*float64(retry)):
		return fmt.Errorf("ha.renewDeadline (%s) must exceed %.1f x ha.retryPeriod (%s)", renew, leaderelection.JitterFactor, retry)
	}
	return nil
}

// watchInterval picks the watchdog period: frequent enough that the leader
// steps down well before another candidate may take the lease over.
func watchInterval(lease, renew, retry time.Duration) time.Duration {
	d := min(retry/2, (lease-renew)/4)
	return max(d, 10*time.Millisecond)
}

// Identity returns the identity this candidate uses in the lease.
func (e *Elector) Identity() string { return e.identity }

// Lease returns "namespace/name" of the lease.
func (e *Elector) Lease() string { return e.namespace + "/" + e.name }

// IsLeader reports whether this replica currently leads: it holds the lease
// and its onStartedLeading context is active. It turns false as soon as
// leadership ends, before onStoppedLeading runs.
func (e *Elector) IsLeader() bool { return e.leader.Load() }

// Fresh reports whether this replica leads and renewed the lease within
// RenewDeadline: only then can no other replica have taken it over, since
// they wait LeaseDuration after the last renewal they saw. Background work
// that acts on engines (closing sessions, stopping engines) checks it, not
// IsLeader: a leader that resumes after a freeze (a stopped container or VM,
// a stalled node) still counts as leader until its watchdog notices the
// stale lease, and may otherwise act on sessions that the new leader serves.
func (e *Elector) Fresh() bool {
	if !e.leader.Load() {
		return false
	}
	t := e.renew.Load()
	if t == nil {
		return false
	}
	since, ok := t.sinceRenew()
	return ok && since <= e.renewDeadline
}

// Leader returns the identity of the last observed lease holder ("" if none
// was observed yet or the lease was released).
func (e *Elector) Leader() string {
	if le := e.elector.Load(); le != nil {
		return le.GetLeader()
	}
	return ""
}

func (e *Elector) setLeader(v bool) {
	e.leaderMu.Lock()
	defer e.leaderMu.Unlock()
	e.leader.Store(v)
	if e.met != nil {
		if v {
			e.met.IsLeader.Set(1)
		} else {
			e.met.IsLeader.Set(0)
		}
	}
}

// Run campaigns for the lease and blocks until ctx is cancelled or
// leadership is lost.
//
// Once the lease is acquired, onStartedLeading runs in its own goroutine
// with a context that is cancelled when leadership ends. It must return
// promptly after that; Run waits for it. Leadership does not end when the
// callback returns early, only when ctx is cancelled or the lease cannot be
// renewed. onStoppedLeading (optional) is called exactly once after
// onStartedLeading returned, and only if onStartedLeading was called.
//
// The leader steps down when it could not renew the lease for RenewDeadline,
// which is before any other candidate may acquire it (LeaseDuration). When
// ctx is cancelled the lease is released.
//
// Run returns nil after ctx was cancelled, ErrLeadershipLost (possibly
// wrapped) if leadership was lost while ctx was still active, and another
// error if the election could not start (nil callback, already running).
func (e *Elector) Run(ctx context.Context, onStartedLeading func(ctx context.Context), onStoppedLeading func()) error {
	if onStartedLeading == nil {
		return errors.New("ha: onStartedLeading must not be nil")
	}
	if !e.running.CompareAndSwap(false, true) {
		return errors.New("ha: elector is already running")
	}
	defer e.running.Store(false)

	// electCtx is cancelled by the parent or by the renewal watchdog; the
	// cause tells them apart.
	electCtx, stopElection := context.WithCancelCause(ctx)
	defer stopElection(nil)

	lock := newRenewTracker(&resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Namespace: e.namespace, Name: e.name},
		Client:     e.client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: e.identity},
		Labels:     map[string]string{"app.kubernetes.io/managed-by": "quackmux"},
	})
	e.renew.Store(lock)

	// client-go starts the leader callback in a goroutine of its own and does
	// not wait for it. Track it so that Run never returns while it runs and
	// never starts it after Run decided to return.
	var (
		mu      sync.Mutex
		closed  bool
		started bool
		leading sync.WaitGroup
	)
	le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   e.leaseDuration,
		RenewDeadline:   e.renewDeadline,
		RetryPeriod:     e.retryPeriod,
		ReleaseOnCancel: true,
		Name:            e.Lease(),
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leCtx context.Context) {
				mu.Lock()
				if closed || leCtx.Err() != nil {
					mu.Unlock()
					return
				}
				started = true
				leading.Add(1)
				mu.Unlock()
				defer leading.Done()
				e.lead(leCtx, lock, stopElection, onStartedLeading)
			},
			// Called by client-go whenever its Run returns, even without
			// leadership; Run below calls onStoppedLeading instead.
			OnStoppedLeading: func() {},
			OnNewLeader: func(identity string) {
				if identity != "" && identity != e.identity {
					e.log.Info("observed new leader", "leader", identity)
				}
			},
		},
	})
	if err != nil {
		return fmt.Errorf("ha: %w", err)
	}
	e.elector.Store(le)

	// A process that just started lets the candidates that run already go
	// first; they ask every retryPeriod (with jitter up to 2.2 times that).
	// When a leader goes away orderly (a deleted pod, a rollout), the follower
	// that waited for the lease takes it over, not the pod that replaces the
	// old leader. In a rollout that pod has the old template (its ReplicaSet
	// re-creates it), and its leadership would stall the rollout again.
	if !e.campaigns.Swap(true) && e.campaignDelay > 0 {
		e.log.Debug("waiting before campaigning", "delay", e.campaignDelay)
		select {
		case <-time.After(e.campaignDelay):
		case <-electCtx.Done():
		}
	}
	e.log.Info("campaigning for leadership", "leaseDuration", e.leaseDuration, "renewDeadline", e.renewDeadline,
		"retryPeriod", e.retryPeriod)
	le.Run(klog.NewContext(electCtx, e.clientLog))

	mu.Lock()
	closed = true
	led := started
	mu.Unlock()
	leading.Wait()
	e.setLeader(false)

	if led && onStoppedLeading != nil {
		onStoppedLeading()
	}
	if ctx.Err() != nil {
		e.log.Info("stopped campaigning", "wasLeader", led)
		return nil
	}
	err = context.Cause(electCtx)
	if !errors.Is(err, ErrLeadershipLost) {
		err = fmt.Errorf("%w: the lease could not be renewed within %s", ErrLeadershipLost, e.renewDeadline)
	}
	e.log.Error("lost leadership", "err", err)
	return err
}

// lead runs the leader callback and the renewal watchdog for one term.
func (e *Elector) lead(leCtx context.Context, lock *renewTracker, stepDown context.CancelCauseFunc, onStartedLeading func(context.Context)) {
	ctx, cancel := context.WithCancel(leCtx)
	defer cancel()

	e.setLeader(true)
	e.log.Info("became leader")
	watchdog := make(chan struct{})
	go func() {
		defer close(watchdog)
		e.watch(ctx, lock, stepDown)
	}()

	onStartedLeading(ctx)
	<-ctx.Done() // leadership lasts until it is lost or ctx is cancelled
	<-watchdog
	e.log.Info("stopped leading")
}

// watch steps down when the lease was not renewed for RenewDeadline. client-go
// gives up somewhat later (a retry period on top) and, with ReleaseOnCancel,
// only cancels the leader context after its release attempt, which can take
// another RenewDeadline when the API server is unreachable. Another candidate
// may take over LeaseDuration after the last renewal, so the leader must stop
// before that. watch also clears the leader flag as soon as the term ends.
func (e *Elector) watch(ctx context.Context, lock *renewTracker, stepDown context.CancelCauseFunc) {
	defer e.setLeader(false)
	t := time.NewTicker(e.watchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			since, ok := lock.sinceRenew()
			if !ok || since <= e.renewDeadline {
				continue
			}
			e.setLeader(false)
			stepDown(fmt.Errorf("%w: the lease was not renewed for %s (renewDeadline %s)",
				ErrLeadershipLost, since.Round(time.Millisecond), e.renewDeadline))
			return
		}
	}
}

// renewTracker wraps the lease lock and remembers when this candidate last
// successfully wrote a lease record that names it as the holder. It measures
// on the monotonic clock and records the start of the write, the earliest
// moment the API server can have stored it.
//
// It also guards the release. client-go (v0.37) releases on cancel after
// reading the lease, but decides on the record it observed at its last
// renewal: a leader that resumes after a freeze, while another candidate
// took the lease over, still believes it holds it, and its update carries
// the resourceVersion it just read, so it would succeed and empty the
// lease of the new leader (seen in test/e2e/kind/ha.sh, h3). A release is
// only written if the record read right before names this candidate.
type renewTracker struct {
	resourcelock.Interface
	base time.Time
	// last is the offset from base of the start of the last successful
	// acquire or renew, or -1 if there was none.
	last atomic.Int64
	// holder is the holder in the record read last ("" before any read).
	holder atomic.Pointer[string]
}

// errNotHolder refuses the release of a lease that another candidate holds.
var errNotHolder = errors.New("ha: the lease is held by another candidate, not releasing it")

// Get implements resourcelock.Interface.
func (t *renewTracker) Get(ctx context.Context) (*resourcelock.LeaderElectionRecord, []byte, error) {
	r, raw, err := t.Interface.Get(ctx)
	if err == nil && r != nil {
		h := r.HolderIdentity
		t.holder.Store(&h)
	}
	return r, raw, err
}

func newRenewTracker(l resourcelock.Interface) *renewTracker {
	t := &renewTracker{Interface: l, base: time.Now()}
	t.last.Store(-1)
	return t
}

// Create implements resourcelock.Interface.
func (t *renewTracker) Create(ctx context.Context, r resourcelock.LeaderElectionRecord) error {
	start := time.Since(t.base)
	err := t.Interface.Create(ctx, r)
	t.observe(start, r, err)
	return err
}

// Update implements resourcelock.Interface.
func (t *renewTracker) Update(ctx context.Context, r resourcelock.LeaderElectionRecord) error {
	if r.HolderIdentity == "" {
		// A release: client-go read the lease right before.
		if h := t.holder.Load(); h == nil || *h != t.Identity() {
			return errNotHolder
		}
	}
	start := time.Since(t.base)
	err := t.Interface.Update(ctx, r)
	t.observe(start, r, err)
	return err
}

func (t *renewTracker) observe(start time.Duration, r resourcelock.LeaderElectionRecord, err error) {
	if err != nil || r.HolderIdentity == "" || r.HolderIdentity != t.Identity() {
		return // failed, or a release
	}
	for {
		old := t.last.Load()
		if int64(start) <= old || t.last.CompareAndSwap(old, int64(start)) {
			return
		}
	}
}

// sinceRenew returns the time since the last successful acquire or renew
// started; ok is false if there was none.
func (t *renewTracker) sinceRenew() (d time.Duration, ok bool) {
	last := t.last.Load()
	if last < 0 {
		return 0, false
	}
	return time.Since(t.base) - time.Duration(last), true
}
