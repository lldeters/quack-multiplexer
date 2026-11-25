package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quackmux/internal/engine"
	"quackmux/internal/upstream"
)

// Orphaned connections. The Quack server collects a connection whose lease
// ran out only when a message for it arrives, so the connection of a client
// that died while no gateway ran stays on its engine until the engine ends.
// After taking over (a restart, or a leader takeover with HA), the gateway
// lists the connections of every recovered engine, waits until every client
// that is still alive had time to send a message (Config.OrphanGrace),
// lists them again and closes the connections that were there from the
// start and that no client asked for in the meantime: CANCEL (a query of a
// dead client may still run) and DISCONNECT.
//
// A client asks for its connection with its first request after the
// takeover: adopt notes the connection before it confirms it with the
// engine, also if the confirmation then fails for a passing reason. Under
// the manager's lock, the sweep skips the connections of open sessions and
// the noted ones and marks the others as orphaned before it closes them;
// adopt refuses a marked connection. A client whose first request arrives
// just as the sweep decides is either noted first and keeps its connection,
// or gets a precise error, never a session whose connection the sweep
// closes behind its back.
//
// Only connections that existed at the first listing are candidates: a
// connection this process creates later belongs to a session of its own, or
// to a handshake that failed (and was closed). The grace covers a handshake
// that was under way at the first listing. CONNECTION engines are left out
// (engine.Manager.RecoveredTargets), and with leader election only a leader
// with a fresh lease closes connections (see SetFence).

// orphanParallelism bounds the engines that are listed or swept at a time.
const orphanParallelism = 8

// orphanReason is what the client of a connection the sweep closed is told.
const orphanReason = "no request arrived within the recovery grace after the gateway took over, " +
	"so the gateway closed the engine connection as orphaned"

// backendRef names a connection on an engine.
type backendRef struct{ engine, conn string }

// SweepOrphans starts the sweep of the orphaned connections on targets, the
// engines found when the registry was recovered
// (engine.Manager.RecoveredTargets), unless engine.closeOrphanedConnections
// is off. Call it once, right after the recovery and before requests are
// served: from then on the connections that clients ask for are noted. It
// returns at once; the sweep lists the connections, waits for
// Config.OrphanGrace of running time and closes the orphans, or stops when
// ctx ends.
func (m *Manager) SweepOrphans(ctx context.Context, targets []engine.Target) {
	if len(targets) == 0 {
		return
	}
	if !m.cfg.CloseOrphanedConnectionsEnabled() {
		m.log.Debug("not closing orphaned connections on recovered engines (engine.closeOrphanedConnections is off)")
		return
	}
	s := m.newOrphanSweep(targets)
	go func() {
		defer s.end()
		if s.list(ctx) == 0 {
			return
		}
		grace := m.cfg.OrphanGrace()
		m.log.Info("closing the connections on recovered engines that no client asks for in time",
			"engines", len(s.targets), "grace", grace)
		every := m.cfg.Session.CheckInterval.Duration
		t := time.NewTicker(every)
		defer t.Stop()
		if waitRunning(ctx, grace, every, t.C, time.Now()) {
			s.Finish(ctx)
		}
	}()
}

// waitRunning waits until the process ran for d since start, measured by
// the ticks of a ticker with period every. After a pause of the process (see
// paused) the wait starts again: clients whose requests failed or queued up
// meanwhile need the time to arrive. It reports false if ctx ended first.
func waitRunning(ctx context.Context, d, every time.Duration, tick <-chan time.Time, start time.Time) bool {
	last := start
	var ran time.Duration
	for ran < d {
		var t time.Time
		select {
		case <-ctx.Done():
			return false
		case t = <-tick:
		}
		if ctx.Err() != nil {
			return false
		}
		if gap := t.Sub(last); paused(gap, every) {
			ran = 0
		} else {
			ran += gap
		}
		last = t
	}
	return true
}

// OrphanSweep is a pending sweep. SweepOrphans runs it; BeginOrphanSweep and
// Finish are its two steps.
type OrphanSweep struct {
	m       *Manager
	targets []engine.Target
	// before holds the connections of each target at the first listing;
	// targets keeps the engines that had any.
	before map[string]map[string]bool
	ended  sync.Once
}

// newOrphanSweep registers a sweep: from now on adopt notes the connections
// that clients ask for.
func (m *Manager) newOrphanSweep(targets []engine.Target) *OrphanSweep {
	m.mu.Lock()
	m.sweeps++
	if m.seen == nil {
		m.seen = map[backendRef]bool{}
	}
	m.mu.Unlock()
	return &OrphanSweep{m: m, targets: targets, before: map[string]map[string]bool{}}
}

// end unregisters the sweep.
func (s *OrphanSweep) end() {
	s.ended.Do(func() {
		m := s.m
		m.mu.Lock()
		if m.sweeps--; m.sweeps == 0 {
			m.seen = nil
		}
		m.mu.Unlock()
	})
}

// BeginOrphanSweep registers a sweep and lists the connections of every
// target, like SweepOrphans does in the background.
func (m *Manager) BeginOrphanSweep(ctx context.Context, targets []engine.Target) *OrphanSweep {
	s := m.newOrphanSweep(targets)
	s.list(ctx)
	return s
}

// list lists the connections of every target and keeps the engines that
// have any; it returns their number. An engine whose listing fails is left
// out.
func (s *OrphanSweep) list(ctx context.Context) int {
	m := s.m
	var mu sync.Mutex
	var keep []engine.Target
	forEachTarget(s.targets, func(tg engine.Target) {
		ids, err := m.backend.Connections(ctx, tg.Endpoint, tg.Token)
		if err != nil {
			m.countSweep(m.sweepFailed(ctx, tg, err))
			return
		}
		if len(ids) == 0 {
			m.countSweep("ok") // nothing to close
			return
		}
		set := make(map[string]bool, len(ids))
		for _, id := range ids {
			set[id] = true
		}
		mu.Lock()
		s.before[tg.ID] = set
		keep = append(keep, tg)
		mu.Unlock()
	})
	s.targets = keep
	return len(keep)
}

// Finish closes the orphaned connections and returns how many it closed.
// Call it once the grace has passed; it ends the sweep.
func (s *OrphanSweep) Finish(ctx context.Context) int {
	defer s.end()
	var closed atomic.Int64
	forEachTarget(s.targets, func(tg engine.Target) {
		result, n := s.finish(ctx, tg)
		closed.Add(int64(n))
		s.m.countSweep(result)
		if met := s.m.met; met != nil {
			met.OrphansClosed.Add(float64(n))
		}
	})
	return int(closed.Load())
}

// finish sweeps one engine and returns the result for the metric and the
// number of connections it closed.
func (s *OrphanSweep) finish(ctx context.Context, tg engine.Target) (string, int) {
	m := s.m
	if !m.fresh() {
		m.log.Info("not closing orphaned connections: the leader lease is not fresh", "engine", tg.ID)
		return "skipped", 0
	}
	cur, ok := m.engines.Lookup(tg.ID)
	if !ok {
		return "skipped", 0 // the engine ended, and its connections with it
	}
	ids, err := m.backend.Connections(ctx, cur.Endpoint, cur.Token)
	if err != nil {
		return m.sweepFailed(ctx, tg, err), 0
	}
	var candidates []string
	for _, id := range ids {
		if s.before[tg.ID][id] {
			candidates = append(candidates, id)
		}
	}
	orphans := m.claimOrphans(tg.ID, candidates)
	var closed, kept []string
	for _, id := range orphans {
		if ctx.Err() != nil || !m.fresh() {
			kept = append(kept, id)
			continue
		}
		if err := m.closeOrphan(ctx, cur, id); err != nil {
			m.log.Warn("closing an orphaned connection failed", "engine", tg.ID, "err", err)
			kept = append(kept, id)
			continue
		}
		closed = append(closed, id)
	}
	// What was not closed stays usable: a client that comes back later
	// adopts it after all.
	m.releaseOrphans(tg.ID, kept)
	if len(orphans) > 0 {
		m.log.Info("closed orphaned connections", "engine", tg.ID, "closed", len(closed), "failed", len(kept),
			"adopted", len(candidates)-len(orphans))
	} else {
		m.log.Debug("no orphaned connections", "engine", tg.ID, "adopted", len(candidates))
	}
	if len(kept) > 0 {
		return "failed", len(closed)
	}
	return "ok", len(closed)
}

// sweepFailed logs a failed listing and returns the result for the metric.
func (m *Manager) sweepFailed(ctx context.Context, tg engine.Target, err error) string {
	if ctx.Err() != nil {
		return "skipped" // shutting down
	}
	args := []any{"engine", tg.ID, "err", err}
	if hint := listingHint(err); hint != "" {
		args = append(args, "hint", hint)
	}
	m.log.Warn("cannot list the connections of a recovered engine: its orphaned connections stay open", args...)
	return "failed"
}

func (m *Manager) countSweep(result string) {
	if m.met != nil {
		m.met.OrphanSweeps.WithLabelValues(result).Inc()
	}
}

// listingHint explains the usual reasons why an engine refuses the listing.
func listingHint(err error) string {
	var ee *upstream.EngineError
	if !errors.As(err, &ee) {
		return ""
	}
	switch msg := ee.Response.Message; {
	case strings.Contains(msg, "Authorization"):
		return "an authorization function on the engine rejects the listing; the macro of the engine image " +
			"lets exactly this query through (images/engine/authorize.sql)"
	case strings.Contains(msg, "out of range for vector with capacity"):
		return "quack_active_connections() of DuckDB v2.0.0 lists at most 2048 connections"
	}
	return ""
}

// fresh reports whether this process may act on engines (see SetFence).
func (m *Manager) fresh() bool { return m.fence == nil || m.fence() }

// claimOrphans marks the connections of an engine among candidates that no
// open session uses and no client asked for as orphaned, and returns them.
func (m *Manager) claimOrphans(engineID string, candidates []string) []string {
	if len(candidates) == 0 {
		return nil
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	inUse := map[string]bool{}
	for _, s := range m.sessions {
		if s.EngineID == engineID {
			inUse[s.BackendConnID] = true
		}
	}
	var out []string
	for _, id := range candidates {
		ref := backendRef{engineID, id}
		if inUse[id] || m.seen[ref] {
			continue
		}
		if m.orphans == nil {
			m.orphans = map[backendRef]time.Time{}
		}
		m.orphans[ref] = now
		out = append(out, id)
	}
	return out
}

// releaseOrphans drops the marks of connections the sweep did not close.
func (m *Manager) releaseOrphans(engineID string, ids []string) {
	if len(ids) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		delete(m.orphans, backendRef{engineID, id})
	}
}

// closeOrphan cancels whatever runs on an orphaned connection and closes it.
// A connection that the engine no longer knows counts as closed.
func (m *Manager) closeOrphan(ctx context.Context, tg engine.Target, conn string) error {
	if err := m.backend.CancelAll(ctx, tg.Endpoint, conn); err != nil && !connectionGone(err) {
		m.log.Debug("cancel on an orphaned connection failed", "engine", tg.ID, "err", err)
	}
	if err := m.backend.Disconnect(ctx, tg.Endpoint, conn); err != nil && !connectionGone(err) {
		return err
	}
	return nil
}

func connectionGone(err error) bool {
	var ee *upstream.EngineError
	return errors.As(err, &ee) && upstream.IsSessionGone(ee.Response.Message)
}

// noteAdoption notes that a client asks for route's connection, so that a
// pending sweep leaves it alone, or refuses the adoption if the sweep closed
// the connection.
func (m *Manager) noteAdoption(engineID string, route Route) *Closed {
	ref := backendRef{engineID, route.BackendConnID}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, orphaned := m.orphans[ref]; orphaned {
		c := Closed{Reason: orphanReason, ClosedAt: m.now()}
		if m.sessions[route.SessionID] == nil {
			m.tombstones[route.SessionID] = tombstone{Closed: c, user: route.User}
		}
		return &c
	}
	if m.seen != nil {
		m.seen[ref] = true
	}
	return nil
}

// expireOrphansLocked forgets the connections the sweep closed longer ago
// than ttl: the engine itself refuses them by now.
func (m *Manager) expireOrphansLocked(now time.Time, ttl time.Duration) {
	for ref, at := range m.orphans {
		if now.Sub(at) > ttl {
			delete(m.orphans, ref)
		}
	}
}

// forEachTarget runs f for every target, orphanParallelism at a time.
func forEachTarget(targets []engine.Target, f func(engine.Target)) {
	sem := make(chan struct{}, orphanParallelism)
	var wg sync.WaitGroup
	for _, tg := range targets {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			f(tg)
		}()
	}
	wg.Wait()
}
