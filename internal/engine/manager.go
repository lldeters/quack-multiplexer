package engine

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"quackmux/internal/config"
	"quackmux/internal/metrics"
)

// State is the manager's lifecycle state of an engine.
type State string

const (
	StateStarting    State = "STARTING"
	StateReady       State = "READY"
	StateDraining    State = "DRAINING"
	StateFailed      State = "FAILED"
	StateTerminating State = "TERMINATING"
)

// Errors returned by Acquire.
var (
	ErrLimit        = errors.New("engine limit reached")
	ErrStartTimeout = errors.New("engine did not become ready in time")
	ErrEngineFailed = errors.New("engine failed")
	ErrCannotLaunch = errors.New("no engine is available for this key and the provider cannot launch one")
	ErrNotSynced    = errors.New("engine registry is not initialized yet")
)

// SessionCloser is implemented by the session manager. The engine manager
// calls it (never while holding its lock) when sessions must go away.
type SessionCloser interface {
	// EngineGone closes all sessions of an engine that no longer exists or
	// failed. Their clients get a must_invalidate error on the next request.
	EngineGone(engineID, reason string)
	// DrainEngine closes all sessions of a draining engine whose drain
	// timeout elapsed (cancelling running queries).
	DrainEngine(engineID, reason string)
}

// engine is the manager's record. Fields are guarded by Manager.mu, except
// the atomics which the proxy updates on the hot path.
type engine struct {
	ID          string
	Key         Key
	Warm        bool
	User        string
	ProfileName string
	SpecHash    string
	State       State
	Endpoint    string
	Token       string
	CreatedAt   time.Time
	ReadyAt     time.Time
	StateSince  time.Time
	Message     string
	Recovered   bool
	Sessions    int
	Reserved    int
	// Volume is the data volume claim of an engine of a persistent profile.
	Volume      string
	terminateAt time.Time // last Terminate call, for retries
	// replaceSince is when a persistent engine became due for replacement
	// (see replaceLocked); zero while it is not.
	replaceSince time.Time

	inFlight atomic.Int64
	lastUsed atomic.Int64 // unix nanos

	ready chan struct{} // closed exactly once, when the engine leaves STARTING
	gone  chan struct{} // closed exactly once, when the engine leaves the registry
	// launched is closed when the provider's Launch call returned; nil for
	// engines this process did not launch (see terminateLocked).
	launched chan struct{}
}

func (e *engine) touch(now time.Time) { e.lastUsed.Store(now.UnixNano()) }

func (e *engine) idleSince() time.Time { return time.Unix(0, e.lastUsed.Load()) }

// startedAt is when the engine's startup budget began: its creation, or, for
// an engine this process found while it was not ready (perhaps only for a
// moment), the moment it was found.
func (e *engine) startedAt() time.Time {
	if e.Recovered && e.StateSince.After(e.CreatedAt) {
		return e.StateSince
	}
	return e.CreatedAt
}

func (e *engine) target() Target {
	return Target{ID: e.ID, Endpoint: e.Endpoint, Token: e.Token, State: e.State, Key: e.Key}
}

// View is a snapshot of an engine.
type View struct {
	ID          string    `json:"id"`
	Key         string    `json:"key"`
	ShareLevel  string    `json:"shareLevel"`
	Owner       string    `json:"owner"`
	Subdomain   string    `json:"subdomain"`
	Profile     string    `json:"profile"`
	PoolIndex   int       `json:"poolIndex"`
	Warm        bool      `json:"warm"`
	User        string    `json:"user,omitempty"`
	State       State     `json:"state"`
	Endpoint    string    `json:"endpoint,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	ReadyAt     time.Time `json:"readyAt,omitzero"`
	StateSince  time.Time `json:"stateSince"`
	LastUsed    time.Time `json:"lastUsed"`
	Message     string    `json:"message,omitempty"`
	Recovered   bool      `json:"recovered"`
	Sessions    int       `json:"sessions"`
	Reserved    int       `json:"reserved"`
	InFlight    int64     `json:"inFlight"`
	SpecHash    string    `json:"specHash"`
	SpecCurrent bool      `json:"specCurrent"`
	Volume      string    `json:"volume,omitempty"`
}

// Target is what the proxy needs to forward a request.
type Target struct {
	ID       string
	Endpoint string
	Token    string
	State    State
	Key      Key
}

// Lease is a reservation on an engine obtained by Acquire. Exactly one of
// Commit or Release must be called.
type Lease struct {
	Target
	m    *Manager
	done atomic.Bool
}

// Commit turns the reservation into an attached session.
func (l *Lease) Commit() {
	if l.done.Swap(true) {
		return
	}
	l.m.mu.Lock()
	defer l.m.mu.Unlock()
	if e := l.m.engines[l.ID]; e != nil {
		e.Reserved--
		e.Sessions++
		e.touch(l.m.now())
	}
}

// Release drops the reservation without attaching a session (connect failed).
func (l *Lease) Release() {
	if l.done.Swap(true) {
		return
	}
	l.m.release(l.ID)
}

// Manager owns the engine registry and lifecycle.
type Manager struct {
	cfg  *config.Config
	prov Provider
	log  *slog.Logger
	met  *metrics.Metrics
	now  func() time.Time
	// fence, if set, must report true for Reconcile to act (see SetFence).
	fence func() bool

	mu            sync.RWMutex
	engines       map[string]*engine
	byUID         map[UID]string
	polling       map[Key]uint64
	specHashes    map[string]string
	recoveryUntil time.Time
	closer        SessionCloser
	// warmFailures counts warm engines of a profile that failed in a row;
	// refills back off while warmRetryAt lies ahead.
	warmFailures map[string]int
	warmRetryAt  map[string]time.Time
	// volumeOps counts data volume deletions in progress per key; no engine
	// is launched for such a key (see DeleteVolume).
	volumeOps map[Key]int

	synced   chan struct{}
	syncOnce sync.Once
}

// NewManager creates a manager. Call SetSessionCloser and Start before use.
func NewManager(cfg *config.Config, prov Provider, log *slog.Logger, met *metrics.Metrics) *Manager {
	return &Manager{
		cfg:          cfg,
		prov:         prov,
		log:          log.With("component", "engines"),
		met:          met,
		now:          time.Now,
		engines:      map[string]*engine{},
		byUID:        map[UID]string{},
		polling:      map[Key]uint64{},
		specHashes:   map[string]string{},
		warmFailures: map[string]int{},
		warmRetryAt:  map[string]time.Time{},
		volumeOps:    map[Key]int{},
		synced:       make(chan struct{}),
	}
}

// SetClock replaces the clock (tests).
func (m *Manager) SetClock(now func() time.Time) { m.now = now }

// SetSessionCloser wires the session manager.
func (m *Manager) SetSessionCloser(c SessionCloser) { m.closer = c }

// SetFence makes Reconcile do nothing while fence reports false. With leader
// election it reports whether this process still holds a fresh lease
// (ha.Elector.Fresh): a leader that resumes after a freeze must not stop or
// launch engines on its outdated view. Call it before Start.
func (m *Manager) SetFence(fence func() bool) { m.fence = fence }

// Provider returns the underlying provider.
func (m *Manager) Provider() Provider { return m.prov }

// Start runs the provider, recovers existing engines and starts the
// reconcile loop. It returns once the registry is initialized; background
// work stops when ctx is cancelled.
func (m *Manager) Start(ctx context.Context) error {
	go func() {
		if err := m.prov.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			m.log.Error("engine provider stopped", "err", err)
		}
	}()
	go m.eventLoop(ctx)
	if err := m.prov.WaitSynced(ctx); err != nil {
		return fmt.Errorf("engine provider sync: %w", err)
	}
	instances, err := m.prov.List(ctx)
	if err != nil {
		return fmt.Errorf("list engines: %w", err)
	}
	now := m.now()
	m.mu.Lock()
	for _, inst := range instances {
		m.registerLocked(inst, now, true)
	}
	m.recoveryUntil = now.Add(m.cfg.Engine.RecoveryGrace.Duration)
	n := len(m.engines)
	m.mu.Unlock()
	if n > 0 {
		m.log.Info("recovered engines", "count", n, "recoveryGraceUntil", m.recoveryUntil)
	}
	m.syncOnce.Do(func() { close(m.synced) })
	go m.reconcileLoop(ctx)
	return nil
}

// Synced reports whether Start finished the initial recovery.
func (m *Manager) Synced() bool {
	select {
	case <-m.synced:
		return true
	default:
		return false
	}
}

// registerLocked adds a discovered instance unless it is known already.
func (m *Manager) registerLocked(inst Instance, now time.Time, recovered bool) *engine {
	if e, ok := m.engines[inst.ID]; ok {
		return e
	}
	if inst.Phase == PhaseGone {
		return nil
	}
	e := &engine{
		ID:          inst.ID,
		Key:         inst.Key,
		Warm:        inst.Warm,
		User:        inst.User,
		ProfileName: inst.ProfileName,
		SpecHash:    inst.SpecHash,
		Endpoint:    inst.Endpoint,
		Token:       inst.Token,
		CreatedAt:   inst.CreatedAt,
		StateSince:  now,
		Message:     inst.Message,
		Recovered:   recovered,
		Volume:      inst.Volume,
		// Recorded by an earlier gateway process (see replaceLocked).
		replaceSince: inst.ReplaceDue,
		ready:        make(chan struct{}),
		gone:         make(chan struct{}),
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now
	}
	e.touch(now)
	switch inst.Phase {
	case PhaseReady:
		e.State, e.ReadyAt = StateReady, now
		close(e.ready)
	case PhaseFailed:
		e.State = StateFailed
		close(e.ready)
	default:
		e.State = StateStarting
	}
	m.addLocked(e)
	return e
}

func (m *Manager) addLocked(e *engine) {
	m.engines[e.ID] = e
	m.byUID[UIDOf(e.ID)] = e.ID
}

// IDByUID resolves the engine UID carried in a connection id.
func (m *Manager) IDByUID(uid UID) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.byUID[uid]
	return id, ok
}

func (m *Manager) eventLoop(ctx context.Context) {
	ch := m.prov.Events()
	for {
		select {
		case <-ctx.Done():
			return
		case inst, ok := <-ch:
			if !ok {
				return
			}
			m.handleEvent(inst)
		}
	}
}

func (m *Manager) handleEvent(inst Instance) { m.applyEvent(inst, nil) }

// applyEvent applies a provider event. With only set (the result of a
// launch), it applies inst to that engine alone: if the engine is no longer
// registered - its pod was deleted and the Gone event came first - the
// result is dropped instead of registering the engine again.
func (m *Manager) applyEvent(inst Instance, only *engine) {
	now := m.now()
	var notify bool
	var reason string
	m.mu.Lock()
	e, known := m.engines[inst.ID]
	if only != nil && e != only {
		m.mu.Unlock()
		return
	}
	if !known {
		// Instances created by an earlier gateway process arrive through the
		// initial List; instances that show up later are adopted as well,
		// so that the registry always mirrors what is running.
		if inst.Phase != PhaseGone && m.Synced() {
			m.registerLocked(inst, now, true)
			m.log.Info("adopted engine", "engine", inst.ID, "key", inst.Key.String(), "phase", inst.Phase)
		}
		m.mu.Unlock()
		return
	}
	if inst.Message != "" {
		e.Message = inst.Message
	}
	if inst.Endpoint != "" {
		e.Endpoint = inst.Endpoint
	}
	if inst.Volume != "" {
		e.Volume = inst.Volume
	}
	switch inst.Phase {
	case PhaseReady:
		if e.State == StateFailed && !m.prov.CanLaunch() {
			// Engines of a static provider are permanent: they recover in place.
			e.State, e.StateSince = StateReady, now
			e.touch(now)
			m.log.Info("engine recovered", "engine", e.ID, "key", e.Key.String())
		}
		if e.State == StateStarting {
			e.State, e.ReadyAt, e.StateSince = StateReady, now, now
			e.touch(now)
			close(e.ready)
			if e.Warm {
				delete(m.warmFailures, e.ProfileName)
				delete(m.warmRetryAt, e.ProfileName)
			}
			if m.met != nil {
				m.met.EngineStarts.WithLabelValues(e.ProfileName, "ready").Inc()
				m.met.EngineStartTime.WithLabelValues(e.ProfileName).Observe(now.Sub(e.CreatedAt).Seconds())
			}
			m.log.Info("engine ready", "engine", e.ID, "key", e.Key.String(), "startup", now.Sub(e.CreatedAt).Round(time.Millisecond))
		}
	case PhaseFailed:
		if e.State != StateFailed && e.State != StateTerminating {
			notify = e.Sessions > 0
			reason = "engine failed: " + inst.Message
			m.setFailedLocked(e, now, inst.Message)
		}
	case PhaseGone:
		notify = true
		reason = "engine terminated"
		if e.State != StateTerminating {
			reason = "engine disappeared"
			if inst.Message != "" {
				reason += ": " + inst.Message
			}
			if m.met != nil {
				m.met.EngineTerminated.WithLabelValues("disappeared").Inc()
			}
		}
		m.removeLocked(e)
	}
	m.mu.Unlock()
	if notify && m.closer != nil {
		m.closer.EngineGone(inst.ID, reason)
	}
}

// leaveStartingLocked closes the ready channel when an engine leaves STARTING.
func leaveStartingLocked(e *engine) {
	if e.State == StateStarting {
		close(e.ready)
	}
}

// claimTimeout bounds persisting a warm engine claim (a PATCH of the pod).
const claimTimeout = 30 * time.Second

// Back-off of warm pool refills after warm engines failed to start.
const (
	warmRetryBase = 30 * time.Second
	warmRetryMax  = 10 * time.Minute
)

func (m *Manager) setFailedLocked(e *engine, now time.Time, msg string) {
	if e.State == StateStarting && m.met != nil {
		m.met.EngineStarts.WithLabelValues(e.ProfileName, "failed").Inc()
	}
	if e.State == StateStarting && e.Warm {
		// A warm pool whose engines cannot start (quota, admission, image)
		// would otherwise launch a new one on every reconcile.
		n := m.warmFailures[e.ProfileName] + 1
		m.warmFailures[e.ProfileName] = n
		m.warmRetryAt[e.ProfileName] = now.Add(min(warmRetryBase<<min(n-1, 5), warmRetryMax))
	}
	leaveStartingLocked(e)
	e.State, e.StateSince = StateFailed, now
	if msg != "" {
		e.Message = msg
	}
	m.log.Warn("engine failed", "engine", e.ID, "key", e.Key.String(), "message", e.Message)
}

func (m *Manager) removeLocked(e *engine) {
	leaveStartingLocked(e)
	close(e.gone)
	delete(m.engines, e.ID)
	delete(m.byUID, UIDOf(e.ID))
	m.log.Info("engine removed", "engine", e.ID, "key", e.Key.String(), "state", e.State)
}

// Acquire returns a reservation on a ready engine for key, launching one if
// needed. user is the authenticated user (for limits and bookkeeping).
func (m *Manager) Acquire(ctx context.Context, key Key, user string) (*Lease, error) {
	if !m.Synced() {
		return nil, ErrNotSynced
	}
	prof, ok := m.cfg.Profiles[key.Profile]
	if !ok {
		return nil, fmt.Errorf("unknown profile %q", key.Profile)
	}
	key.PoolIndex = 0
	if key.ShareLevel != ShareConnection && prof.Pool.Size > 1 {
		key.PoolIndex = m.selectPoolIndex(key.Base(), prof.Pool)
	}
	// A persistent key has one startup budget for the whole call, including
	// the wait for its previous engine (see reserve). Other keys keep a
	// budget per engine.
	var deadline time.Time
	if prof.Persistence != nil {
		deadline = time.Now().Add(m.cfg.EngineStartupTimeout(key.Profile))
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		e, err := m.reserve(ctx, key, user, prof, deadline)
		if err != nil {
			return nil, err
		}
		lease, err := m.wait(ctx, e, deadline)
		if err == nil {
			return lease, nil
		}
		if !errors.Is(err, errRetry) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

var errRetry = errors.New("engine left service while waiting")

// reserve finds, claims or launches the engine for key and reserves it.
func (m *Manager) reserve(ctx context.Context, key Key, user string, prof config.Profile, deadline time.Time) (*engine, error) {
	claimFailed, waitLogged := false, false
	for {
		m.mu.Lock()
		if e := m.findLocked(key); e != nil {
			e.Reserved++
			m.mu.Unlock()
			return e, nil
		}
		if prof.Persistence != nil {
			// Only one engine may use the data volume of the key (see
			// persistence.go). awaitPredecessorLocked keeps m.mu only when
			// the key may get a new engine now.
			waited, err := m.awaitPredecessorLocked(ctx, key, deadline, &waitLogged)
			if err != nil {
				return nil, err
			}
			if waited {
				continue
			}
		}
		if key.ShareLevel == ShareConnection && !claimFailed {
			if err := m.checkUserLimitLocked(user); err != nil {
				m.mu.Unlock()
				return nil, err
			}
			if w := m.claimWarmLocked(key, user); w != nil {
				w.Reserved++
				m.mu.Unlock()
				// Persist the claim even if the client gives up meanwhile: a
				// cancelled claim would cost the warm engine.
				cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claimTimeout)
				err := m.prov.Claim(cctx, w.ID, key, user)
				cancel()
				if err != nil {
					// The claim could not be persisted, so a restarted gateway could
					// hand the engine out again: never use it. One attempt per
					// connect: a failing API must not cost the whole warm pool.
					m.log.Warn("warm engine claim failed, terminating it", "engine", w.ID, "err", err)
					m.mu.Lock()
					w.Reserved--
					m.terminateLocked(w, "warm claim failed")
					m.mu.Unlock()
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					claimFailed = true
					continue
				}
				return w, nil
			}
		}
		if !m.prov.CanLaunch() {
			m.mu.Unlock()
			return nil, ErrCannotLaunch
		}
		if err := m.checkLimitsLocked(key, user); err != nil {
			m.mu.Unlock()
			return nil, err
		}
		e := m.newEngineLocked(key, user, false)
		e.Reserved = 1
		m.mu.Unlock()
		m.launch(e, prof)
		return e, nil
	}
}

// findLocked returns the engine that serves key: a READY or STARTING engine
// with exactly that key and a current spec, READY first. Draining engines
// take no new sessions. Normally there is at most one; after a restart there
// can be more (an engine that failed its startup timeout but became ready
// later, next to its replacement): then the oldest serves, every time, and
// Reconcile drains the others, so that the sessions of a key do not spread
// over engines with different database state.
func (m *Manager) findLocked(key Key) *engine {
	var best *engine
	for _, e := range m.engines {
		if !m.servesNewLocked(e, key) {
			continue
		}
		if best == nil || (best.State == StateStarting && e.State == StateReady) ||
			(best.State == e.State && olderEngine(e, best)) {
			best = e
		}
	}
	return best
}

// servesNewLocked reports whether e may take new sessions of key.
func (m *Manager) servesNewLocked(e *engine, key Key) bool {
	if e.Warm || e.Key != key || (e.State != StateReady && e.State != StateStarting) {
		return false
	}
	if m.persistent(e.ProfileName) {
		// The engine of a persistent key is the only one that may open its
		// data volume, so it keeps taking sessions until it is replaced
		// (Reconcile, replaceLocked). One without the volume takes none.
		return m.mountsVolumeLocked(e)
	}
	return m.specCurrentLocked(e) || !m.replaceOnSpecChange()
}

// olderEngine orders engines by creation, then by id.
func olderEngine(a, b *engine) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.ID < b.ID
}

func (m *Manager) claimWarmLocked(key Key, user string) *engine {
	var pick *engine
	inRecovery := m.now().Before(m.recoveryUntil)
	now := m.now()
	for _, e := range m.engines {
		if !e.Warm || e.ProfileName != key.Profile || e.State != StateReady {
			continue
		}
		if max := m.cfg.EngineMaxLifetime(e.ProfileName); max > 0 && now.Sub(e.CreatedAt) > max {
			continue // recycled by maintainWarmPoolsLocked
		}
		if e.Recovered && inRecovery {
			// A claim that was in flight when the previous gateway process
			// died may not have been persisted; its session re-attaches with
			// its next heartbeat. Do not hand such engines out meanwhile.
			continue
		}
		if !m.specCurrentLocked(e) && m.replaceOnSpecChange() {
			continue
		}
		if pick == nil || e.ReadyAt.Before(pick.ReadyAt) {
			pick = e
		}
	}
	if pick == nil {
		return nil
	}
	pick.Warm = false
	pick.Key = key
	pick.User = user
	m.log.Info("claimed warm engine", "engine", pick.ID, "key", key.String())
	return pick
}

func (m *Manager) selectPoolIndex(base Key, pool config.PoolConfig) int {
	if pool.SelectPolicy == "POLLING" {
		m.mu.Lock()
		n := m.polling[base]
		m.polling[base] = n + 1
		m.mu.Unlock()
		return int(n % uint64(pool.Size))
	}
	return rand.IntN(pool.Size)
}

func (m *Manager) replaceOnSpecChange() bool {
	return m.cfg.Engine.ReplaceOnSpecChange == nil || *m.cfg.Engine.ReplaceOnSpecChange
}

// currentSpecHashLocked caches the provider's spec hash per profile (the
// configuration does not change at runtime).
func (m *Manager) currentSpecHashLocked(profile string) (string, bool) {
	if h, ok := m.specHashes[profile]; ok {
		return h, true
	}
	prof, ok := m.cfg.Profiles[profile]
	if !ok {
		return "", false
	}
	h := m.prov.SpecHash(profile, prof)
	m.specHashes[profile] = h
	return h, true
}

func (m *Manager) specCurrentLocked(e *engine) bool {
	h, ok := m.currentSpecHashLocked(e.ProfileName)
	if !ok {
		return false
	}
	return e.SpecHash == "" || e.SpecHash == h
}

// checkLimitsLocked checks the limits before an engine is launched for key
// on behalf of user (empty for warm pool refills).
func (m *Manager) checkLimitsLocked(key Key, user string) error {
	l := m.cfg.Limits
	var total, starting int
	for _, e := range m.engines {
		if e.State == StateFailed || e.State == StateTerminating {
			continue
		}
		total++
		if e.State == StateStarting {
			starting++
		}
	}
	if l.MaxEngines > 0 && total >= l.MaxEngines {
		return fmt.Errorf("%w: %d engines running (limits.maxEngines)", ErrLimit, total)
	}
	if l.MaxStartingEngines > 0 && starting >= l.MaxStartingEngines {
		return fmt.Errorf("%w: %d engines starting (limits.maxStartingEngines), try again shortly", ErrLimit, starting)
	}
	return m.checkUserLimitLocked(user)
}

// checkUserLimitLocked enforces limits.maxEnginesPerUser: the engines a
// user caused to be started or claimed, whatever their share level. Shared
// engines count too: otherwise any user could start engines for arbitrary
// SERVER or GROUP subdomains until limits.maxEngines locks everybody out.
func (m *Manager) checkUserLimitLocked(user string) error {
	max := m.cfg.Limits.MaxEnginesPerUser
	if max <= 0 || user == "" {
		return nil
	}
	n := 0
	for _, e := range m.engines {
		if !e.Warm && e.User == user && e.State != StateFailed && e.State != StateTerminating {
			n++
		}
	}
	if n >= max {
		return fmt.Errorf("%w: user %q already has %d engines (limits.maxEnginesPerUser)", ErrLimit, user, n)
	}
	return nil
}

func newToken() string {
	b := make([]byte, 32)
	if _, err := cryptorand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}

func (m *Manager) newEngineLocked(key Key, user string, warm bool) *engine {
	now := m.now()
	hash, _ := m.currentSpecHashLocked(key.Profile)
	e := &engine{
		ID:          m.prov.NewID(key, warm),
		Key:         key,
		Warm:        warm,
		User:        user,
		ProfileName: key.Profile,
		SpecHash:    hash,
		State:       StateStarting,
		Token:       newToken(),
		CreatedAt:   now,
		StateSince:  now,
		ready:       make(chan struct{}),
		gone:        make(chan struct{}),
		launched:    make(chan struct{}),
	}
	e.touch(now)
	m.addLocked(e)
	return e
}

// launch asks the provider to create the engine. It runs detached from any
// client request: a client that gives up does not abort a launch others may
// be waiting for.
func (m *Manager) launch(e *engine, prof config.Profile) {
	req := LaunchRequest{
		ID:          e.ID,
		Key:         e.Key,
		Warm:        e.Warm,
		User:        e.User,
		ProfileName: e.ProfileName,
		Profile:     prof,
		Token:       e.Token,
		SpecHash:    e.SpecHash,
	}
	if prof.Persistence != nil {
		// Checked once more right before the instance is created: the lease
		// may have gone stale since reserve checked it (persistence.go).
		req.Fence = m.fence
	}
	m.log.Info("launching engine", "engine", e.ID, "key", e.Key.String(), "warm", e.Warm, "user", e.User)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), m.cfg.EngineStartupTimeout(e.ProfileName))
		defer cancel()
		inst, err := m.prov.Launch(ctx, req)
		close(e.launched)
		if err != nil {
			m.mu.Lock()
			if m.engines[e.ID] == e && e.State == StateStarting {
				msg := "launch failed: " + err.Error()
				m.setFailedLocked(e, m.now(), msg)
				// Remove whatever may have been half created.
				m.terminateLocked(e, msg)
			}
			m.mu.Unlock()
			return
		}
		m.applyEvent(inst, e)
	}()
}

// wait blocks until a reserved engine is ready, at most until deadline if
// that is set. On failure the reservation is dropped. errRetry asks Acquire
// to pick another engine.
func (m *Manager) wait(ctx context.Context, e *engine, deadline time.Time) (*Lease, error) {
	timeout := m.cfg.EngineStartupTimeout(e.ProfileName)
	remaining := timeout - m.now().Sub(e.startedAt())
	if !deadline.IsZero() {
		remaining = min(remaining, time.Until(deadline))
	}
	if remaining < time.Second {
		remaining = time.Second
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-e.ready:
	case <-ctx.Done():
		m.release(e.ID)
		return nil, ctx.Err()
	case <-timer.C:
		m.release(e.ID)
		return nil, fmt.Errorf("%w (%s)", ErrStartTimeout, timeout)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.engines[e.ID] == e && e.State == StateReady && e.Endpoint != "" {
		return &Lease{Target: e.target(), m: m}, nil
	}
	e.Reserved--
	e.touch(m.now())
	if e.State == StateDraining {
		return nil, errRetry
	}
	msg := e.Message
	if msg == "" {
		msg = string(e.State)
	}
	return nil, fmt.Errorf("%w: %s", ErrEngineFailed, msg)
}

func (m *Manager) release(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.engines[id]; e != nil {
		e.Reserved--
		e.touch(m.now())
	}
}

// Lookup returns the routing target of an engine. Sessions may keep using
// READY and DRAINING engines.
func (m *Manager) Lookup(id string) (Target, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.engines[id]
	if !ok || (e.State != StateReady && e.State != StateDraining) || e.Endpoint == "" {
		return Target{}, false
	}
	return e.target(), true
}

// Exists reports whether the registry knows the engine, and its state.
func (m *Manager) Exists(id string) (State, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.engines[id]
	if !ok {
		return "", false
	}
	return e.State, true
}

// AttachAdopted accounts a session that was re-attached after a gateway
// restart. A warm engine that turns out to carry a session is marked claimed
// so that it is never handed out a second time.
func (m *Manager) AttachAdopted(id, sessionID, user string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.engines[id]
	if e == nil {
		return
	}
	e.Sessions++
	e.touch(m.now())
	if e.Warm {
		e.Warm = false
		e.Key = Key{ShareLevel: ShareConnection, Owner: sessionID, Profile: e.ProfileName}
		e.User = user
		claimKey := e.Key
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := m.prov.Claim(ctx, id, claimKey, user); err != nil {
				m.log.Warn("re-claiming adopted warm engine failed", "engine", id, "err", err)
			}
		}()
	}
}

// DetachSession accounts a closed session.
func (m *Manager) DetachSession(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.engines[id]; e != nil && e.Sessions > 0 {
		e.Sessions--
		e.touch(m.now())
	}
}

// BeginRequest / EndRequest count proxied requests per engine.
func (m *Manager) BeginRequest(id string) {
	m.mu.RLock()
	e := m.engines[id]
	m.mu.RUnlock()
	if e != nil {
		e.inFlight.Add(1)
		e.touch(m.now())
	}
}

func (m *Manager) EndRequest(id string) {
	m.mu.RLock()
	e := m.engines[id]
	m.mu.RUnlock()
	if e != nil {
		e.inFlight.Add(-1)
		e.touch(m.now())
	}
}

// MarkFailed takes an engine out of service (for example after it reported
// that its database was invalidated). Its sessions are closed. Permanent
// (static) engines come back with their next successful health probe.
func (m *Manager) MarkFailed(id, reason string) {
	m.mu.Lock()
	e := m.engines[id]
	if e == nil || e.State == StateFailed || e.State == StateTerminating {
		m.mu.Unlock()
		return
	}
	m.setFailedLocked(e, m.now(), reason)
	m.mu.Unlock()
	if m.closer != nil {
		m.closer.EngineGone(id, "engine failed: "+reason)
	}
}

// ErrPermanent is returned when an operation needs a provider that can
// launch engines (static engines are managed elsewhere).
var ErrPermanent = errors.New("engines of this provider are managed externally and cannot be drained or terminated")

// Drain stops routing new sessions to an engine; it terminates once its
// sessions are gone (or engine.drainTimeout closes them).
func (m *Manager) Drain(id, reason string) error {
	if !m.prov.CanLaunch() {
		return ErrPermanent
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.engines[id]
	if e == nil {
		return fmt.Errorf("engine %q not found", id)
	}
	if e.State == StateReady || e.State == StateStarting {
		m.drainLocked(e, reason)
	}
	return nil
}

func (m *Manager) drainLocked(e *engine, reason string) {
	leaveStartingLocked(e)
	e.State, e.StateSince = StateDraining, m.now()
	e.Message = "draining: " + reason
	m.log.Info("engine draining", "engine", e.ID, "key", e.Key.String(), "reason", reason)
}

// Terminate stops an engine immediately; its sessions are closed.
func (m *Manager) Terminate(id, reason string) error {
	if !m.prov.CanLaunch() {
		return ErrPermanent
	}
	m.mu.Lock()
	e := m.engines[id]
	if e == nil {
		m.mu.Unlock()
		return fmt.Errorf("engine %q not found", id)
	}
	hadSessions := e.Sessions > 0
	m.terminateLocked(e, reason)
	m.mu.Unlock()
	if hadSessions && m.closer != nil {
		m.closer.EngineGone(id, "engine terminated: "+reason)
	}
	return nil
}

func (m *Manager) terminateLocked(e *engine, reason string) {
	now := m.now()
	if e.State != StateTerminating {
		leaveStartingLocked(e)
		e.State, e.StateSince = StateTerminating, now
		e.Message = reason
		if m.met != nil {
			m.met.EngineTerminated.WithLabelValues(terminationLabel(reason)).Inc()
		}
		m.log.Info("terminating engine", "engine", e.ID, "key", e.Key.String(), "reason", reason)
	}
	e.terminateAt = now
	id, launched := e.ID, e.launched
	launchTimeout := m.cfg.EngineStartupTimeout(e.ProfileName)
	go func() {
		if launched != nil {
			// While the launch is under way, the instance may not exist yet:
			// the provider would find nothing to stop and report the engine
			// gone, and the instance could appear afterwards, next to a
			// successor (for a persistent key: on the same data volume).
			t := time.NewTimer(launchTimeout + time.Minute)
			defer t.Stop()
			select {
			case <-launched:
			case <-t.C:
				m.log.Warn("terminate engine postponed: its launch has not returned (will retry)", "engine", id)
				return
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := m.prov.Terminate(ctx, id, reason); err != nil {
			m.log.Warn("terminate engine failed (will retry)", "engine", id, "err", err)
		}
	}()
}

// terminationLabel keeps metric label cardinality bounded.
func terminationLabel(reason string) string {
	for _, p := range []string{"idle", "drained", "failed", "launch failed", "warm", "admin", "startup timeout", "replaced"} {
		if len(reason) >= len(p) && reason[:len(p)] == p {
			return p
		}
	}
	return "other"
}

// List returns snapshots of all engines, sorted by creation time.
func (m *Manager) List() []View {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]View, 0, len(m.engines))
	for _, e := range m.engines {
		out = append(out, m.viewLocked(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Get returns one engine snapshot.
func (m *Manager) Get(id string) (View, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.engines[id]
	if !ok {
		return View{}, false
	}
	return m.viewLocked(e), true
}

func (m *Manager) viewLocked(e *engine) View {
	return View{
		ID: e.ID, Key: e.Key.String(), ShareLevel: string(e.Key.ShareLevel), Owner: e.Key.Owner,
		Subdomain: e.Key.Subdomain, Profile: e.ProfileName, PoolIndex: e.Key.PoolIndex, Warm: e.Warm,
		User: e.User, State: e.State, Endpoint: e.Endpoint, CreatedAt: e.CreatedAt, ReadyAt: e.ReadyAt,
		StateSince: e.StateSince, LastUsed: e.idleSince(), Message: e.Message, Recovered: e.Recovered,
		Sessions: e.Sessions, Reserved: e.Reserved, InFlight: e.inFlight.Load(), SpecHash: e.SpecHash,
		SpecCurrent: m.specCurrentLocked(e), Volume: e.Volume,
	}
}

func (m *Manager) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(m.cfg.Engine.CheckInterval.Duration)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Both cases may be ready at once, and select picks at random.
			if ctx.Err() != nil {
				return
			}
			m.Reconcile()
		}
	}
}

// Reconcile applies timeouts and keeps warm pools filled. It runs
// periodically and may be called directly (tests).
func (m *Manager) Reconcile() {
	if m.fence != nil && !m.fence() {
		m.log.Debug("not reconciling engines: the leader lease is not fresh")
		return
	}
	now := m.now()
	var drainClose []string
	m.mu.Lock()
	inRecovery := now.Before(m.recoveryUntil)
	permanent := !m.prov.CanLaunch()
	// The READY engine that serves each key (see findLocked).
	serving := map[Key]*engine{}
	for _, e := range m.engines {
		if e.State == StateReady && m.servesNewLocked(e, e.Key) {
			if cur := serving[e.Key]; cur == nil || olderEngine(e, cur) {
				serving[e.Key] = e
			}
		}
	}
	for _, e := range m.engines {
		if permanent {
			// Externally managed engines: only the provider's health probes
			// change their state; the gateway never stops them.
			continue
		}
		idle := e.Sessions == 0 && e.Reserved == 0 && e.inFlight.Load() == 0
		idleFor := now.Sub(e.idleSince())
		protected := e.Recovered && inRecovery
		switch e.State {
		case StateStarting:
			if limit := m.cfg.EngineStartupTimeout(e.ProfileName); now.Sub(e.startedAt()) > limit {
				msg := fmt.Sprintf("startup timeout after %s", limit)
				if e.Message != "" {
					msg += ": " + e.Message
				}
				m.setFailedLocked(e, now, msg)
			}
		case StateReady:
			if e.Warm {
				continue // see maintainWarmPoolsLocked
			}
			if why := m.replacementDueLocked(e, now); why != "" {
				if !m.mountsVolumeLocked(e) {
					m.drainLocked(e, why)
				} else if m.replaceLocked(e, why, now, idle, protected) {
					drainClose = append(drainClose, e.ID)
				}
				continue
			}
			if !e.replaceSince.IsZero() {
				m.replacementCancelledLocked(e)
			}
			if s := serving[e.Key]; s != nil && s != e {
				m.drainLocked(e, "duplicate engine: "+s.ID+" serves "+e.Key.String())
				continue
			}
			if idle && !protected {
				if limit := m.cfg.EngineIdleTimeout(string(e.Key.ShareLevel), e.ProfileName); idleFor >= limit {
					m.terminateLocked(e, fmt.Sprintf("idle for %s", idleFor.Round(time.Second)))
				}
			}
		case StateDraining:
			if idle && !protected {
				m.terminateLocked(e, "drained")
				continue
			}
			if e.Sessions > 0 && now.Sub(e.StateSince) > m.cfg.Engine.DrainTimeout.Duration {
				drainClose = append(drainClose, e.ID)
			}
		case StateFailed:
			if e.Reserved == 0 && now.Sub(e.StateSince) >= m.cfg.Engine.FailedRetention.Duration {
				m.terminateLocked(e, "failed: "+e.Message)
			}
		case StateTerminating:
			if now.Sub(e.terminateAt) > 2*time.Minute {
				m.terminateLocked(e, e.Message) // retry; the provider reports Gone when done
			}
		}
	}
	m.maintainWarmPoolsLocked()
	m.updateGaugesLocked()
	m.mu.Unlock()

	if m.closer != nil {
		for _, id := range drainClose {
			m.closer.DrainEngine(id, "engine drain timeout")
		}
	}
}

func (m *Manager) maintainWarmPoolsLocked() {
	if !m.prov.CanLaunch() {
		return
	}
	names := make([]string, 0, len(m.cfg.Profiles))
	for name := range m.cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		prof := m.cfg.Profiles[name]
		var warm []*engine
		for _, e := range m.engines {
			if !e.Warm || e.ProfileName != name || (e.State != StateStarting && e.State != StateReady) {
				continue
			}
			if !m.specCurrentLocked(e) && m.replaceOnSpecChange() {
				m.terminateLocked(e, "warm engine outdated")
				continue
			}
			if max := m.cfg.EngineMaxLifetime(name); max > 0 && e.State == StateReady && m.now().Sub(e.CreatedAt) > max {
				// Never hand out an engine that would be drained right away.
				m.terminateLocked(e, fmt.Sprintf("warm engine reached max lifetime %s", max))
				continue
			}
			warm = append(warm, e)
		}
		for len(warm) > prof.WarmPool.Size {
			m.terminateLocked(warm[len(warm)-1], "warm pool shrunk")
			warm = warm[:len(warm)-1]
		}
		if now := m.now(); len(warm) < prof.WarmPool.Size && now.Before(m.warmRetryAt[name]) {
			continue // the last warm engines of this profile failed: back off
		}
		for i := len(warm); i < prof.WarmPool.Size; i++ {
			key := Key{ShareLevel: ShareConnection, Profile: name}
			if err := m.checkLimitsLocked(key, ""); err != nil {
				break
			}
			m.launch(m.newEngineLocked(key, "", true), prof)
		}
	}
}

func (m *Manager) updateGaugesLocked() {
	if m.met == nil {
		return
	}
	m.met.Engines.Reset()
	for _, e := range m.engines {
		state := string(e.State)
		if e.Warm {
			state = "WARM_" + state
		}
		m.met.Engines.WithLabelValues(state, string(e.Key.ShareLevel), e.ProfileName).Inc()
	}
}
