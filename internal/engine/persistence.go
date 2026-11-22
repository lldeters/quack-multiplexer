package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Profiles with persistence (profiles.<name>.persistence) give every engine
// key a data volume of its own. A DuckDB database file can be opened for
// writing by one process only, so the manager keeps at most one engine per
// persistent key, in any state: a successor is launched only after its
// predecessor has left the registry, which happens when the provider reports
// it gone (for Kubernetes: the pod object is deleted, so its containers have
// stopped). The rules:
//
//   - The engine of a persistent key keeps taking new sessions when it is due
//     for replacement (profile changed, engine.maxLifetime reached): there is
//     no other engine to send them to (servesNewLocked).
//   - Reconcile terminates such an engine as soon as it is idle, and drains it
//     and closes its sessions engine.drainTimeout after the replacement fell
//     due (replaceLocked).
//   - A session that finds no serving engine but another engine of its key
//     (draining, failed or terminating) waits for that engine to be gone,
//     within the startup budget of the connect; failed engines are terminated
//     at once for it (awaitPredecessorLocked, holderLocked, waitHolder).
//   - Launches for persistent keys, and the terminations and waits that make
//     room for them, need a fresh leader lease (SetFence) and no deletion of
//     the key's volume in progress (DeleteVolume).
//
// The provider must report an engine gone only once no process of it can
// still open the volume (the Kubernetes provider: the pod object is deleted,
// which the kubelet does after the containers stopped and the volumes were
// unmounted).

// Errors of persistent keys and data volumes.
var (
	// ErrHandover: the previous engine of a persistent key did not end within
	// the startup budget of the connect.
	ErrHandover = errors.New("the previous engine of this key still holds its data volume")
	// ErrVolumeDeleting: the data volume of the key is being deleted.
	ErrVolumeDeleting = errors.New("the data volume of this engine key is being deleted, try again shortly")
	// ErrNotLeader: engines of persistent keys are only launched with a fresh
	// leader lease.
	ErrNotLeader = errors.New("this gateway does not hold a fresh leader lease")
	// ErrVolumeNotFound: no data volume of this gateway instance has that name.
	ErrVolumeNotFound = errors.New("data volume not found")
	// ErrVolumeInUse: an engine of the volume's key exists.
	ErrVolumeInUse = errors.New("data volume is in use")
)

// Volume is the data volume of an engine key, as a VolumeProvider reports it.
type Volume struct {
	// Name of the volume (for Kubernetes the PersistentVolumeClaim).
	Name       string `json:"name"`
	Key        string `json:"key"`
	ShareLevel string `json:"shareLevel"`
	Owner      string `json:"owner"`
	Subdomain  string `json:"subdomain"`
	Profile    string `json:"profile"`
	// User is the user whose session caused the volume to be created.
	User         string    `json:"user,omitempty"`
	Size         string    `json:"size,omitempty"`
	StorageClass string    `json:"storageClass,omitempty"`
	AccessModes  []string  `json:"accessModes,omitempty"`
	Phase        string    `json:"phase,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	// LastLaunch is when an engine with this volume was last launched.
	LastLaunch time.Time `json:"lastLaunch,omitzero"`
	// Deleting is set once the deletion of the volume was requested.
	Deleting bool `json:"deleting,omitempty"`
	// Engine is the engine of the key, if one exists (set by the manager).
	Engine string `json:"engine,omitempty"`
	// EngineKey is the parsed key.
	EngineKey Key `json:"-"`
}

// VolumeProvider is implemented by providers that keep a data volume per
// engine key (the Kubernetes provider, for profiles with persistence).
type VolumeProvider interface {
	// Volumes lists the data volumes of this gateway instance.
	Volumes(ctx context.Context) ([]Volume, error)
	// DeleteVolume deletes a data volume of this gateway instance. It returns
	// ErrVolumeNotFound for names that are not one.
	DeleteVolume(ctx context.Context, name string) error
}

// holderRecheck bounds a wait for the previous engine of a persistent key:
// the wait re-evaluates the registry at least this often.
const holderRecheck = time.Second

// persistent reports whether a profile keeps a data volume per key. The
// configuration does not change at runtime.
func (m *Manager) persistent(profile string) bool {
	p, ok := m.cfg.Profiles[profile]
	return ok && p.Persistence != nil
}

// awaitPredecessorLocked runs with m.mu held when a session of a persistent
// key found no engine to join. It returns (false, nil) with m.mu still held
// when the key may get a new engine now. Otherwise it releases m.mu and
// returns an error when no engine may be launched for the key (no fresh
// leader lease, volume deletion in progress, or the previous engine did not
// end within the deadline), or true once it waited for the previous engine
// of the key (until it was gone or holderRecheck passed); the caller then
// looks at the registry again.
//
// The lease is checked first: a leader that resumes after a freeze must
// neither stop an engine that the new leader serves by now nor wait for one.
func (m *Manager) awaitPredecessorLocked(ctx context.Context, key Key, deadline time.Time) (bool, error) {
	if err := m.persistentLaunchAllowedLocked(key); err != nil {
		m.mu.Unlock()
		return false, err
	}
	h := m.holderLocked(key)
	if h == nil {
		return false, nil
	}
	return true, m.waitHolder(ctx, h, deadline)
}

// holderLocked returns an engine that still exists for key although it takes
// no new sessions (draining, failed or terminating), or nil. Failed engines
// and idle draining engines are terminated right away: a session needs the
// key's data volume. Like Reconcile, it leaves recovered engines alone during
// engine.recoveryGrace (their sessions may not have re-attached yet). Only
// call it with a fresh leader lease (persistentLaunchAllowedLocked).
func (m *Manager) holderLocked(key Key) *engine {
	var holder *engine
	inRecovery := m.now().Before(m.recoveryUntil)
	for _, e := range m.engines {
		if e.Warm || e.Key != key {
			continue
		}
		idle := e.Sessions == 0 && e.Reserved == 0 && e.inFlight.Load() == 0
		switch {
		case e.State == StateFailed && e.Reserved == 0:
			m.terminateLocked(e, "failed: "+e.Message+" (a new session needs its data volume)")
		case e.State == StateDraining && idle && !(e.Recovered && inRecovery):
			m.terminateLocked(e, "drained")
		}
		if holder == nil || olderEngine(e, holder) {
			holder = e
		}
	}
	return holder
}

// waitHolder waits, with m.mu held on entry and released on return, until
// the engine h is gone or holderRecheck passed. It fails when ctx ends or
// the deadline passes first.
func (m *Manager) waitHolder(ctx context.Context, h *engine, deadline time.Time) error {
	gone, id, state := h.gone, h.ID, h.State
	m.mu.Unlock()
	left := time.Until(deadline)
	if left <= 0 {
		return fmt.Errorf("%w (engine %s, %s); try again later", ErrHandover, id, state)
	}
	t := time.NewTimer(min(left, holderRecheck))
	defer t.Stop()
	select {
	case <-gone:
	case <-t.C:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// persistentLaunchAllowedLocked checks whether this gateway may launch an
// engine for a persistent key, or make room for one, apart from the other
// engines of the key.
func (m *Manager) persistentLaunchAllowedLocked(key Key) error {
	if m.volumeOps[key] > 0 {
		return ErrVolumeDeleting
	}
	// A leader that resumes after a freeze must not start a second writer
	// next to the engine the new leader started meanwhile, nor stop an
	// engine the new leader serves.
	if m.fence != nil && !m.fence() {
		return ErrNotLeader
	}
	return nil
}

// replacementDueLocked returns why a READY engine must be replaced, or "".
func (m *Manager) replacementDueLocked(e *engine, now time.Time) string {
	if max := m.cfg.EngineMaxLifetime(e.ProfileName); max > 0 && now.Sub(e.CreatedAt) > max {
		return fmt.Sprintf("max lifetime %s reached", max)
	}
	if !m.specCurrentLocked(e) && m.replaceOnSpecChange() {
		return "profile changed"
	}
	return ""
}

// replaceLocked handles a READY engine of a persistent key that is due for
// replacement. Its successor may start only once it is gone, so it keeps
// serving until it is idle and is terminated then. engine.drainTimeout after
// the replacement fell due it is drained; the result is true then, and the
// caller closes its sessions.
func (m *Manager) replaceLocked(e *engine, why string, now time.Time, idle bool) bool {
	if e.replaceSince.IsZero() {
		e.replaceSince = now
		e.Message = "replacement pending: " + why
		m.log.Info("engine replacement pending: it is replaced once it has no sessions", "engine", e.ID,
			"key", e.Key.String(), "reason", why, "sessions", e.Sessions,
			"deadline", now.Add(m.cfg.Engine.DrainTimeout.Duration))
	}
	switch {
	case idle:
		m.terminateLocked(e, "replaced: "+why)
	case now.Sub(e.replaceSince) >= m.cfg.Engine.DrainTimeout.Duration:
		m.drainLocked(e, why+"; replacement overdue (engine.drainTimeout)")
		return true
	}
	return false
}

// Volumes lists the data volumes of the provider, each with the engine of
// its key if one exists. Providers without data volumes have none.
func (m *Manager) Volumes(ctx context.Context) ([]Volume, error) {
	vp, ok := m.prov.(VolumeProvider)
	if !ok {
		return nil, nil
	}
	vols, err := vp.Volumes(ctx)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	for i := range vols {
		vols[i].Engine = m.engineOfKeyLocked(vols[i].EngineKey)
	}
	m.mu.RUnlock()
	sort.Slice(vols, func(i, j int) bool { return vols[i].Name < vols[j].Name })
	return vols, nil
}

// engineOfKeyLocked returns the id of an engine of key ("" if none).
func (m *Manager) engineOfKeyLocked(key Key) string {
	var found *engine
	for _, e := range m.engines {
		if !e.Warm && e.Key == key && (found == nil || olderEngine(e, found)) {
			found = e
		}
	}
	if found == nil {
		return ""
	}
	return found.ID
}

// DeleteVolume deletes the data volume name. It refuses (ErrVolumeInUse)
// while an engine of the volume's key exists, and no engine is launched for
// the key while the deletion request runs. It needs a fresh leader lease and
// a synced registry: only the leader launches engines, and before the
// initial recovery the registry may not know every engine yet.
func (m *Manager) DeleteVolume(ctx context.Context, name string) (Volume, error) {
	vp, ok := m.prov.(VolumeProvider)
	if !ok {
		return Volume{}, ErrVolumeNotFound
	}
	if m.fence != nil && !m.fence() {
		return Volume{}, ErrNotLeader
	}
	if !m.Synced() {
		return Volume{}, ErrNotSynced
	}
	vols, err := vp.Volumes(ctx)
	if err != nil {
		return Volume{}, err
	}
	var v *Volume
	for i := range vols {
		if vols[i].Name == name {
			v = &vols[i]
			break
		}
	}
	if v == nil {
		return Volume{}, ErrVolumeNotFound
	}
	key := v.EngineKey
	m.mu.Lock()
	if id := m.engineOfKeyLocked(key); id != "" {
		state := m.engines[id].State
		m.mu.Unlock()
		v.Engine = id
		return *v, fmt.Errorf("%w: engine %s (%s) of key %s exists; terminate it first", ErrVolumeInUse, id, state, key)
	}
	m.volumeOps[key]++
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if m.volumeOps[key]--; m.volumeOps[key] <= 0 {
			delete(m.volumeOps, key)
		}
		m.mu.Unlock()
	}()
	if err := vp.DeleteVolume(ctx, name); err != nil {
		return *v, err
	}
	v.Deleting = true
	m.log.Info("data volume deleted", "volume", name, "key", key.String())
	return *v, nil
}
