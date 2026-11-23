package proxy

import (
	"net/http"
	"time"

	"quackmux/internal/wire"
)

// ReasonNotLeader is the reason Options.Ready gives on a follower, a replica
// that does not lead (and did not lead in its current term). Quack traffic
// should never reach a follower: the Service of the HA overlay selects the
// leader's pod by its label. Requests that do (a Service without that
// selector, a port-forward to a follower) count with outcome "not_leader",
// and a warning says so. Other reasons, such as a leader that is stepping
// down, count as "not_ready".
const ReasonNotLeader = "not the leader"

// notLeaderWarnInterval rate-limits the warning about Quack requests that
// reach a replica that does not lead.
const notLeaderWarnInterval = time.Minute

// refuseNotReady answers a request on a replica that does not serve. It does
// not read the body: the request did not run, and the client may send it
// again. As always, the answer is an ERROR_RESPONSE with HTTP 200.
func (g *Gateway) refuseNotReady(w http.ResponseWriter, r *http.Request, why string, start time.Time) {
	writeError(w, "quackmux: gateway is not ready: "+why, wire.ExceptionConnection, false)
	outcome := "not_ready"
	if why == ReasonNotLeader {
		outcome = "not_leader"
		g.warnNotLeader(r)
	}
	g.count(wire.TypeInvalid, outcome, start)
}

// warnNotLeader logs, at most once per notLeaderWarnInterval, that a Quack
// request reached a replica that does not lead.
func (g *Gateway) warnNotLeader(r *http.Request) {
	now := time.Now().UnixNano()
	last := g.notLeaderWarned.Load()
	if now-last < int64(notLeaderWarnInterval) || !g.notLeaderWarned.CompareAndSwap(last, now) {
		return
	}
	g.log.Warn("Quack requests reach this replica, although it does not lead (quackmux_requests_total{outcome=\"not_leader\"}); "+
		"with leader election every Service that routes Quack traffic to the gateway must select the label quackmux.io/leader=true",
		"remote", r.RemoteAddr)
}
