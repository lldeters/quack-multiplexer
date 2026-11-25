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
// that died while no gateway ran stays on its engine until the engine ends,
// with its temporary tables, an open transaction or a running query.
//
// After taking over (a restart, or a leader takeover with HA), the gateway
// lists the connections of every recovered engine and waits until the
// engine lease of every connection that only the previous process kept
// alive has run out (Config.OrphanGrace): the gateway asks for
// session.backendHeartbeatTimeout at every handshake with an engine and
// renews that lease only while a session is open. Then it sends a HEARTBEAT
// to each listed connection that belongs to no open session, and the engine
// decides: it removes a connection whose lease ran out (and interrupts its
// query), and renews one whose lease still runs. Nobody can use a connection
// whose lease ran out any more, since its next message gets the same answer;
// so the sweep never takes a connection away from a client that could still
// use it, whatever kept the client away meanwhile (a lost node, a frozen
// leader, a partition), and leaves connections alone that others keep alive
// (another gateway on a static engine, a client that reaches the engine
// directly).
//
// Only connections that existed when the gateway took over are probed: a
// connection this process creates later belongs to a session of its own, or
// to a handshake that failed (and was closed). CONNECTION engines are left
// out (engine.Manager.RecoveredTargets), and with leader election only a
// leader with a fresh lease probes (see SetFence).

// orphanParallelism bounds the engines that are listed or probed at a time.
const orphanParallelism = 8

// SweepOrphans starts the sweep of the orphaned connections on targets, the
// engines found when the registry was recovered
// (engine.Manager.RecoveredTargets), unless engine.closeOrphanedConnections
// is off. Call it once, right after the recovery. It returns at once; the
// sweep lists the connections, waits for Config.OrphanGrace by the manager's
// clock and probes them, or stops when ctx ends.
func (m *Manager) SweepOrphans(ctx context.Context, targets []engine.Target) {
	if len(targets) == 0 {
		return
	}
	if !m.cfg.CloseOrphanedConnectionsEnabled() {
		m.log.Debug("not closing orphaned connections on recovered engines (engine.closeOrphanedConnections is off)")
		return
	}
	start := m.now()
	go func() {
		s := m.BeginOrphanSweep(ctx, targets)
		if len(s.targets) == 0 {
			return
		}
		grace := m.cfg.OrphanGrace()
		m.log.Info("closing the connections on recovered engines whose engine lease runs out without a session",
			"engines", len(s.targets), "grace", grace)
		if m.waitUntil(ctx, start.Add(grace)) {
			s.Finish(ctx)
		}
	}()
}

// waitUntil waits until the manager's clock reaches t, looking every
// session.checkInterval. The engines measure their leases in real time, and
// so does the wait: a pause of this process does not extend it. It reports
// false if ctx ended first.
func (m *Manager) waitUntil(ctx context.Context, t time.Time) bool {
	tick := time.NewTicker(m.cfg.Session.CheckInterval.Duration)
	defer tick.Stop()
	for m.now().Before(t) {
		select {
		case <-ctx.Done():
			return false
		case <-tick.C:
		}
	}
	return ctx.Err() == nil
}

// OrphanSweep is a pending sweep: the connections its engines held when the
// gateway took over. SweepOrphans runs it; BeginOrphanSweep and Finish are
// its two steps.
type OrphanSweep struct {
	m *Manager
	// targets keeps the engines that held connections; before holds them.
	targets []engine.Target
	before  map[string][]string
}

// BeginOrphanSweep lists the connections of every target, like SweepOrphans
// does in the background, and keeps the engines that have any; an engine
// whose listing fails is left out. Finish probes them once their leases ran
// out.
func (m *Manager) BeginOrphanSweep(ctx context.Context, targets []engine.Target) *OrphanSweep {
	s := &OrphanSweep{m: m, before: map[string][]string{}}
	var mu sync.Mutex
	forEachTarget(targets, func(tg engine.Target) {
		ids, err := m.backend.Connections(ctx, tg.Endpoint, tg.Token)
		if err != nil {
			m.countSweep(m.listingFailed(ctx, tg, err))
			return
		}
		if len(ids) == 0 {
			m.countSweep("ok") // nothing to close
			return
		}
		mu.Lock()
		s.before[tg.ID] = ids
		s.targets = append(s.targets, tg)
		mu.Unlock()
	})
	return s
}

// Finish probes the connections that the engines held at the first listing
// and that belong to no open session, and returns how many of them the
// engines removed because their lease had run out. Call it once
// Config.OrphanGrace has passed since BeginOrphanSweep: earlier, a probe
// only renews the lease of an orphan.
func (s *OrphanSweep) Finish(ctx context.Context) int {
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

// finish probes the connections of one engine and returns the result for
// the metric and the number of connections the engine removed.
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
	inUse := m.backendConns(tg.ID)
	result := "ok"
	var closed, kept, gone, failed, adopted int
	for _, id := range s.before[tg.ID] {
		if inUse[id] {
			adopted++
			continue
		}
		if ctx.Err() != nil || !m.fresh() {
			result = "skipped"
			break
		}
		err := m.backend.Heartbeat(ctx, cur.Endpoint, id)
		var ee *upstream.EngineError
		switch {
		case err == nil:
			kept++ // its lease still runs: someone keeps it alive
		case errors.As(err, &ee) && ee.Response.Message == upstream.MsgLeaseExpired:
			closed++ // the engine removed it and interrupted its query
		case errors.As(err, &ee) && upstream.IsSessionGone(ee.Response.Message):
			gone++ // gone meanwhile
		default:
			failed++
			m.log.Warn("probing a connection on a recovered engine failed", "engine", tg.ID, "err", err)
		}
	}
	if failed > 0 {
		result = "failed"
	}
	if closed+kept+gone+failed > 0 {
		m.log.Info("closed orphaned connections", "engine", tg.ID, "closed", closed, "kept", kept, "gone", gone,
			"failed", failed, "adopted", adopted)
	} else {
		m.log.Debug("no orphaned connections", "engine", tg.ID, "adopted", adopted)
	}
	return result, closed
}

// backendConns returns the engine connections of the open sessions on an
// engine.
func (m *Manager) backendConns(engineID string) map[string]bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]bool{}
	for _, s := range m.sessions {
		if s.EngineID == engineID {
			out[s.BackendConnID] = true
		}
	}
	return out
}

// listingFailed logs a failed listing and returns the result for the
// metric. An engine whose authorization function refuses the listing (the
// strict macro of the engine image) is skipped rather than failed: that is
// how it is configured.
func (m *Manager) listingFailed(ctx context.Context, tg engine.Target, err error) string {
	if ctx.Err() != nil {
		return "skipped" // shutting down
	}
	var ee *upstream.EngineError
	if errors.As(err, &ee) && strings.HasPrefix(ee.Response.Message, "Authorization ") {
		m.log.Info("not closing orphaned connections: the authorization function of the engine refuses the listing",
			"engine", tg.ID, "err", err, "hint", "the macro of the engine image lets the listing through with "+
				"quack_authorization_function = 'quackmux_authorize_listing' (images/engine/authorize.sql)")
		return "skipped"
	}
	args := []any{"engine", tg.ID, "err", err}
	if ee != nil && strings.Contains(ee.Response.Message, "out of range for vector with capacity") {
		args = append(args, "hint", "quack_active_connections() of DuckDB v2.0.0 lists at most 2048 connections")
	}
	m.log.Warn("cannot list the connections of a recovered engine: its orphaned connections stay open", args...)
	return "failed"
}

func (m *Manager) countSweep(result string) {
	if m.met != nil {
		m.met.OrphanSweeps.WithLabelValues(result).Inc()
	}
}

// fresh reports whether this process may act on engines (see SetFence).
func (m *Manager) fresh() bool { return m.fence == nil || m.fence() }

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
