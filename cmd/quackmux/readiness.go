package main

import (
	"sync/atomic"

	"quackmux/internal/ha"
	"quackmux/internal/proxy"
)

// leadership is what readiness asks of the elector (ha.Elector).
type leadership interface {
	SteppingDown() bool
	Fresh() bool
}

// readiness decides whether this process serves Quack requests (serving)
// and whether its pod is ready (ready).
type readiness struct {
	stopping, leading, started *atomic.Bool
	// synced reports whether the engine registry finished its initial sync.
	synced func() bool
	// elector and standby are nil without leader election; standby is the
	// readiness of a follower (ha.StandbyReady).
	elector leadership
	standby func() (bool, string)
}

// serving reports whether this process serves Quack requests. With leader
// election only the leader with a fresh lease does; its pod carries the
// label that the Service selects.
func (r *readiness) serving() (bool, string) {
	if r.stopping.Load() {
		return false, "shutting down"
	}
	if r.elector != nil {
		if r.elector.SteppingDown() {
			return false, ha.ReasonSteppingDown
		}
		if !r.leading.Load() || !r.elector.Fresh() {
			return false, proxy.ReasonNotLeader
		}
	}
	if !r.started.Load() || !r.synced() {
		return false, "starting up"
	}
	return true, ""
}

// ready is the readiness of the pod (/readyz). Without leader election it is
// serving. With it every healthy replica is ready, so that rollouts, the
// PodDisruptionBudget and health checks work as usual; the Service routes
// Quack traffic by the leader label. Of the replicas that do not serve, only
// a follower is ready, as a standby that could take over at once (standby).
// A leader that does not serve yet (its engine registry has not synced),
// steps down or shuts down is not: nobody else can take the lease over
// while it renews it.
func (r *readiness) ready() (bool, string) {
	ok, why := r.serving()
	if ok || r.standby == nil || why != proxy.ReasonNotLeader {
		return ok, why
	}
	return r.standby()
}
