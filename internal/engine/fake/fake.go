// Package fake is an engine provider that runs in-process fake Quack engines
// (quacktest servers). It is used by integration tests and the demo mode
// ("provider.type: fake" is not exposed in configuration).
package fake

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"quackmux/internal/config"
	"quackmux/internal/engine"
	"quackmux/internal/quacktest"
)

// Provider launches quacktest servers.
type Provider struct {
	// StartDelay delays readiness (simulates pod start-up).
	StartDelay time.Duration
	// FailLaunch makes Launch fail with this error.
	FailLaunch error
	// NeverReady keeps launched engines in PhaseStarting.
	NeverReady bool
	// Permanent makes the provider behave like the static provider: it
	// cannot launch, and Terminate keeps the engine.
	Permanent bool

	mu        sync.Mutex
	instances map[string]*entry
	events    chan engine.Instance
	seq       atomic.Int64
	launches  atomic.Int64
	claims    map[string]engine.Key
	synced    chan struct{}
}

type entry struct {
	inst   engine.Instance
	server *quacktest.Server
}

// New creates a fake provider.
func New() *Provider {
	return &Provider{
		instances: map[string]*entry{},
		events:    make(chan engine.Instance, 1024),
		claims:    map[string]engine.Key{},
		synced:    make(chan struct{}),
	}
}

// Name implements engine.Provider.
func (p *Provider) Name() string { return "fake" }

// CanLaunch implements engine.Provider.
func (p *Provider) CanLaunch() bool { return !p.Permanent }

// AddReady registers a running engine (like a static engine, or one found at
// start-up) and returns its server.
func (p *Provider) AddReady(id string, key engine.Key, token string) *quacktest.Server {
	srv := quacktest.NewServer(token)
	p.mu.Lock()
	p.instances[id] = &entry{server: srv, inst: engine.Instance{
		ID: id, Key: key, ProfileName: key.Profile, Phase: engine.PhaseReady, Endpoint: srv.URL(),
		Token: token, CreatedAt: time.Now(),
	}}
	p.mu.Unlock()
	return srv
}

// Emit sends a provider event for an engine with the given phase (tests).
func (p *Provider) Emit(id string, phase engine.Phase, msg string) {
	p.mu.Lock()
	e := p.instances[id]
	p.mu.Unlock()
	if e == nil {
		return
	}
	inst := e.inst
	inst.Phase, inst.Message = phase, msg
	p.events <- inst
}

// NewID implements engine.Provider.
func (p *Provider) NewID(key engine.Key, warm bool) string {
	n := p.seq.Add(1)
	if warm {
		return fmt.Sprintf("fake-warm-%d", n)
	}
	return fmt.Sprintf("fake-%s-%d", key.ShareLevel, n)
}

// SpecHash implements engine.Provider.
func (p *Provider) SpecHash(name string, prof config.Profile) string {
	return engine.HashJSON(struct {
		Name    string
		Profile config.Profile
	}{name, prof})
}

// Run implements engine.Provider.
func (p *Provider) Run(ctx context.Context) error {
	close(p.synced)
	<-ctx.Done()
	return ctx.Err()
}

// WaitSynced implements engine.Provider.
func (p *Provider) WaitSynced(ctx context.Context) error {
	select {
	case <-p.synced:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// List implements engine.Provider.
func (p *Provider) List(context.Context) ([]engine.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]engine.Instance, 0, len(p.instances))
	for _, e := range p.instances {
		out = append(out, e.inst)
	}
	return out, nil
}

// Launch implements engine.Provider.
func (p *Provider) Launch(ctx context.Context, req engine.LaunchRequest) (engine.Instance, error) {
	if p.FailLaunch != nil {
		return engine.Instance{}, p.FailLaunch
	}
	p.launches.Add(1)
	srv := quacktest.NewServer(req.Token)
	inst := engine.Instance{
		ID: req.ID, Key: req.Key, Warm: req.Warm, ProfileName: req.ProfileName, SpecHash: req.SpecHash,
		Phase: engine.PhaseStarting, Token: req.Token, User: req.User, CreatedAt: time.Now(),
	}
	p.mu.Lock()
	p.instances[req.ID] = &entry{inst: inst, server: srv}
	p.mu.Unlock()
	if !p.NeverReady {
		go func() {
			if p.StartDelay > 0 {
				time.Sleep(p.StartDelay)
			}
			p.mu.Lock()
			e := p.instances[req.ID]
			if e == nil {
				p.mu.Unlock()
				return
			}
			e.inst.Phase = engine.PhaseReady
			e.inst.Endpoint = srv.URL()
			ready := e.inst
			p.mu.Unlock()
			p.events <- ready
		}()
	}
	return inst, nil
}

// Claim implements engine.Provider.
func (p *Provider) Claim(_ context.Context, id string, key engine.Key) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.instances[id]
	if e == nil {
		return fmt.Errorf("unknown engine %s", id)
	}
	e.inst.Key, e.inst.Warm = key, false
	p.claims[id] = key
	return nil
}

// Terminate implements engine.Provider.
func (p *Provider) Terminate(_ context.Context, id, reason string) error {
	if p.Permanent {
		return nil
	}
	p.mu.Lock()
	e := p.instances[id]
	delete(p.instances, id)
	p.mu.Unlock()
	if e == nil {
		// Like the real providers: an engine without an instance (a failed
		// launch, or one deleted already) is reported gone.
		p.events <- engine.Instance{ID: id, Phase: engine.PhaseGone, Message: reason}
		return nil
	}
	e.server.Close()
	gone := e.inst
	gone.Phase, gone.Message = engine.PhaseGone, reason
	p.events <- gone
	return nil
}

// Events implements engine.Provider.
func (p *Provider) Events() <-chan engine.Instance { return p.events }

// Server returns the fake server behind an engine (tests).
func (p *Provider) Server(id string) *quacktest.Server {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.instances[id]; e != nil {
		return e.server
	}
	return nil
}

// Launches counts Launch calls.
func (p *Provider) Launches() int { return int(p.launches.Load()) }

// IDs lists live engine ids.
func (p *Provider) IDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.instances))
	for id := range p.instances {
		out = append(out, id)
	}
	return out
}

// Crash simulates an engine that disappears (pod deleted or node lost).
func (p *Provider) Crash(id string) {
	p.mu.Lock()
	e := p.instances[id]
	delete(p.instances, id)
	p.mu.Unlock()
	if e == nil {
		return
	}
	e.server.Close()
	gone := e.inst
	gone.Phase, gone.Message = engine.PhaseGone, "crashed"
	p.events <- gone
}

// Adopt hands an existing instance (as if found at start-up) to the provider.
// It is used to simulate a gateway restart: a second gateway is built on a
// provider that already owns the engines of the first.
func (p *Provider) Adopt(from *Provider) {
	from.mu.Lock()
	defer from.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, e := range from.instances {
		p.instances[id] = e
	}
	from.instances = map[string]*entry{}
}
