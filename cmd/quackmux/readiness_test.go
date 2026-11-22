package main

import (
	"sync/atomic"
	"testing"

	"quackmux/internal/ha"
	"quackmux/internal/proxy"
)

type fakeLeadership struct{ steppingDown, fresh bool }

func (f fakeLeadership) SteppingDown() bool { return f.steppingDown }
func (f fakeLeadership) Fresh() bool        { return f.fresh }

// state is one combination of what readiness looks at.
type state struct {
	stopping, leading, started, synced bool
	ha                                 *fakeLeadership // nil: no leader election
}

func (s state) readiness(t *testing.T, standbyAsked *bool) *readiness {
	t.Helper()
	r := &readiness{stopping: new(atomic.Bool), leading: new(atomic.Bool), started: new(atomic.Bool),
		synced: func() bool { return s.synced }}
	r.stopping.Store(s.stopping)
	r.leading.Store(s.leading)
	r.started.Store(s.started)
	if s.ha != nil {
		r.elector = *s.ha
		r.standby = func() (bool, string) { *standbyAsked = true; return true, "" }
	}
	return r
}

func TestReadiness(t *testing.T) {
	follower := &fakeLeadership{}
	leader := &fakeLeadership{fresh: true}
	stale := &fakeLeadership{steppingDown: true}
	for _, tc := range []struct {
		name                 string
		s                    state
		serving, ready       bool
		servingWhy, readyWhy string
		standbyAsked         bool
	}{
		// Without leader election readiness is serving, as before.
		{name: "base: starting up", s: state{}, servingWhy: "starting up", readyWhy: "starting up"},
		{name: "base: syncing", s: state{started: true}, servingWhy: "starting up", readyWhy: "starting up"},
		{name: "base: serving", s: state{started: true, synced: true}, serving: true, ready: true},
		{name: "base: shutting down", s: state{stopping: true, started: true, synced: true},
			servingWhy: "shutting down", readyWhy: "shutting down"},
		// With it a follower is ready as a standby ...
		{name: "follower", s: state{ha: follower}, servingWhy: proxy.ReasonNotLeader, ready: true, standbyAsked: true},
		{name: "follower that shuts down", s: state{ha: follower, stopping: true},
			servingWhy: "shutting down", readyWhy: "shutting down"},
		// ... but a leader only once it serves: nobody else can take over.
		{name: "leader starting up", s: state{ha: leader, leading: true},
			servingWhy: "starting up", readyWhy: "starting up"},
		{name: "leader syncing engines", s: state{ha: leader, leading: true, started: true},
			servingWhy: "starting up", readyWhy: "starting up"},
		{name: "leader serving", s: state{ha: leader, leading: true, started: true, synced: true}, serving: true, ready: true},
		{name: "leader with a stale lease", s: state{ha: stale, leading: true, started: true, synced: true},
			servingWhy: ha.ReasonSteppingDown, readyWhy: ha.ReasonSteppingDown},
		{name: "former leader", s: state{ha: stale, started: true, synced: true},
			servingWhy: ha.ReasonSteppingDown, readyWhy: ha.ReasonSteppingDown},
		{name: "leader that shuts down", s: state{ha: leader, leading: true, started: true, synced: true, stopping: true},
			servingWhy: "shutting down", readyWhy: "shutting down"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked bool
			r := tc.s.readiness(t, &asked)
			if ok, why := r.serving(); ok != tc.serving || why != tc.servingWhy {
				t.Errorf("serving = %v %q, want %v %q", ok, why, tc.serving, tc.servingWhy)
			}
			if ok, why := r.ready(); ok != tc.ready || why != tc.readyWhy {
				t.Errorf("ready = %v %q, want %v %q", ok, why, tc.ready, tc.readyWhy)
			}
			if asked != tc.standbyAsked {
				t.Errorf("standby readiness asked: %v, want %v", asked, tc.standbyAsked)
			}
		})
	}
}
