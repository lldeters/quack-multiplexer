// Package static is an engine provider for Quack servers that are run
// elsewhere: a DuckDB process on a VM, a server another team operates, an
// engine of another gateway. The gateway only routes to them; it never
// launches, drains or terminates them.
//
// Every entry of provider.static.engines becomes one engine with a fixed
// engine key: share level (USER, GROUP or SERVER; CONNECTION needs engines
// that can be launched), owner (the user or group; empty for SERVER),
// subdomain and profile (by default the configured defaults, normally
// "default") and pool index. A session uses a static engine when its key is
// exactly that key; sessions without a matching engine are rejected. The
// engine's id is the entry's name, its endpoint the URL (without /quack and
// without a trailing slash), and its token comes from token or tokenFile
// (read once at start-up; a rotated token needs a gateway restart).
//
// Health: Run sends GET <url>/ to every engine every healthInterval (5s
// timeout per probe, a fresh connection each time). The stock Quack server
// answers it with HTTP 200 and its banner, without authentication. The first
// round decides whether an engine starts in PhaseReady or PhaseFailed;
// WaitSynced returns once it is complete. Afterwards a failing probe reports
// PhaseFailed (once per outage, with the error as message), which takes the
// engine out of service, and every successful probe reports PhaseReady. The
// repetition is deliberate and cheap for the manager: an engine the gateway
// itself took out of service (engine.Manager.MarkFailed, after the engine
// reported an invalidated database) comes back with its next successful
// probe, as the manager documents. PhaseGone is never reported: static
// engines are permanent.
package static

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quackmux/internal/config"
	"quackmux/internal/engine"
)

const (
	// DefaultHealthInterval is the probe period when none is configured.
	DefaultHealthInterval = 10 * time.Second
	// DefaultProbeTimeout bounds one health probe.
	DefaultProbeTimeout = 5 * time.Second

	// MessageHealthy is the message of engines whose last probe succeeded.
	MessageHealthy = "health check passed"

	// maxParallelProbes bounds the probes of one round that run at once.
	maxParallelProbes = 16
	minEventBuffer    = 64
)

// Option customizes a Provider.
type Option func(*Provider)

// WithProbeTimeout sets the timeout of one health probe.
func WithProbeTimeout(d time.Duration) Option {
	return func(p *Provider) {
		if d > 0 {
			p.timeout = d
		}
	}
}

// WithHTTPClient replaces the HTTP client of the health probes (for example
// to trust a private certificate authority in tests).
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provider) {
		if c != nil {
			p.client = c
		}
	}
}

// Provider routes to statically configured engines. It is safe for
// concurrent use.
type Provider struct {
	log      *slog.Logger
	interval time.Duration
	failures int // failed probes in a row that take an engine out of rotation
	timeout  time.Duration
	client   *http.Client

	// engines is fixed after New (configuration order); only the instances
	// inside change, guarded by mu.
	engines []*entry
	mu      sync.Mutex

	events  chan engine.Instance
	synced  chan struct{} // closed after the first probe round
	done    chan struct{} // closed when Run has returned
	running atomic.Bool
	seq     atomic.Uint64
}

type entry struct {
	probeURL string
	inst     engine.Instance
	failures int // probes failed in a row
}

var _ engine.Provider = (*Provider)(nil)

// New builds one engine per entry of cfg.Provider.Static.Engines. It fails if
// an entry is unusable: bad URL, missing or unreadable token, unknown share
// level or profile, an owner that does not fit the share level, a pool index
// outside the profile's pool, or a duplicate name.
func New(cfg *config.Config, log *slog.Logger, opts ...Option) (*Provider, error) {
	if cfg == nil {
		return nil, errors.New("static provider: no configuration")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	sc := cfg.Provider.Static
	p := &Provider{
		log:      log.With("component", "static-engines"),
		interval: sc.HealthInterval.Duration,
		failures: sc.FailureThreshold,
		timeout:  DefaultProbeTimeout,
		synced:   make(chan struct{}),
		done:     make(chan struct{}),
	}
	if p.interval <= 0 {
		p.interval = DefaultHealthInterval
	}
	if p.failures <= 0 {
		p.failures = 1
	}
	for _, o := range opts {
		o(p)
	}
	if p.client == nil {
		p.client = newProbeClient(p.timeout)
	}
	if len(sc.Engines) == 0 {
		return nil, errors.New("static provider: provider.static.engines must not be empty")
	}
	engines, err := newEntries(cfg, loadToken)
	if err != nil {
		return nil, err
	}
	p.engines = engines
	p.events = make(chan engine.Instance, max(minEventBuffer, 2*len(p.engines)))
	p.checkCoverage(cfg)
	return p, nil
}

// Validate checks provider.static as New does, but without reading token
// files (for "quackmux validate", which may run where they do not exist).
func Validate(cfg *config.Config) error {
	if len(cfg.Provider.Static.Engines) == 0 {
		return errors.New("static provider: provider.static.engines must not be empty")
	}
	_, err := newEntries(cfg, func(se config.StaticEngine) (string, error) {
		if se.TokenFile != "" && se.Token == "" {
			return "unread", nil
		}
		return loadToken(se)
	})
	return err
}

// newEntries builds the entries of provider.static.engines, getting their
// tokens from token.
func newEntries(cfg *config.Config, token func(config.StaticEngine) (string, error)) ([]*entry, error) {
	now := time.Now()
	seen := map[string]bool{}
	var entries []*entry
	var errs []error
	for i, se := range cfg.Provider.Static.Engines {
		e, err := newEntry(cfg, se, now, token)
		if err == nil && seen[se.Name] {
			err = errors.New("duplicate name")
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("provider.static.engines[%d] (%s): %w", i, se.Name, err))
			continue
		}
		seen[se.Name] = true
		entries = append(entries, e)
	}
	return entries, errors.Join(errs...)
}

func newEntry(cfg *config.Config, se config.StaticEngine, created time.Time, loadToken func(config.StaticEngine) (string, error)) (*entry, error) {
	if se.Name == "" {
		return nil, errors.New("name is required")
	}
	endpoint, err := parseEndpoint(se.URL)
	if err != nil {
		return nil, err
	}
	token, err := loadToken(se)
	if err != nil {
		return nil, err
	}
	level, err := engine.ParseShareLevel(se.ShareLevel)
	if err != nil {
		return nil, err
	}
	switch level {
	case engine.ShareConnection:
		return nil, errors.New("share level CONNECTION needs a provider that can launch engines")
	case engine.ShareUser, engine.ShareGroup:
		if se.Owner == "" {
			return nil, fmt.Errorf("owner (the %s) is required for share level %s", strings.ToLower(string(level)), level)
		}
	case engine.ShareServer:
		if se.Owner != "" {
			return nil, errors.New("owner must be empty for share level SERVER")
		}
	}
	subdomain := se.Subdomain
	if subdomain == "" {
		subdomain = cfg.Defaults.Subdomain
	}
	if subdomain == "" {
		subdomain = "default"
	}
	// Session subdomains are lower-cased before they are matched.
	subdomain = strings.ToLower(subdomain)
	if !engine.ValidSubdomain(subdomain) {
		return nil, fmt.Errorf("invalid subdomain %q (lower-case letters, digits and '-', at most 32 characters)", subdomain)
	}
	profile := se.Profile
	if profile == "" {
		profile = cfg.Defaults.Profile
	}
	if profile == "" {
		profile = config.DefaultProfileName
	}
	prof, ok := cfg.Profiles[profile]
	if !ok {
		return nil, fmt.Errorf("profile %q is not defined", profile)
	}
	if size := poolSize(prof); se.PoolIndex < 0 || se.PoolIndex >= size {
		return nil, fmt.Errorf("poolIndex %d is outside the pool of profile %q (pool.size %d)", se.PoolIndex, profile, size)
	}
	return &entry{
		probeURL: endpoint + "/",
		inst: engine.Instance{
			ID:          se.Name,
			Key:         engine.Key{ShareLevel: level, Owner: se.Owner, Subdomain: subdomain, Profile: profile, PoolIndex: se.PoolIndex},
			ProfileName: profile,
			Phase:       engine.PhaseStarting,
			Endpoint:    endpoint,
			Token:       token,
			CreatedAt:   created,
		},
	}, nil
}

func poolSize(prof config.Profile) int { return max(1, prof.Pool.Size) }

// parseEndpoint checks a static engine URL and returns it without trailing
// slashes.
func parseEndpoint(raw string) (string, error) {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	if s == "" {
		return "", errors.New("url is required")
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("invalid url: %w", err)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return "", fmt.Errorf("url %q must start with http:// or https://", s)
	case u.Host == "" || u.Hostname() == "":
		return "", fmt.Errorf("url %q has no host", s)
	case u.User != nil:
		return "", errors.New("url must not contain credentials (use token or tokenFile)")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return "", fmt.Errorf("url %q must not have a query or fragment", s)
	case u.Path == "/quack" || strings.HasSuffix(u.Path, "/quack"):
		return "", fmt.Errorf("url %q must not include /quack (the gateway appends it)", s)
	}
	return s, nil
}

func loadToken(se config.StaticEngine) (string, error) {
	switch {
	case se.Token != "" && se.TokenFile != "":
		return "", errors.New("set token or tokenFile, not both")
	case se.TokenFile != "":
		b, err := os.ReadFile(se.TokenFile)
		if err != nil {
			return "", fmt.Errorf("read tokenFile: %w", err)
		}
		tok := strings.TrimSpace(string(b))
		if tok == "" {
			return "", fmt.Errorf("tokenFile %s is empty", se.TokenFile)
		}
		return tok, nil
	case strings.TrimSpace(se.Token) != "":
		return se.Token, nil
	}
	return "", errors.New("token or tokenFile is required")
}

// checkCoverage warns about configurations that are valid but probably not
// meant: several engines with one key, and pools with missing indices.
func (p *Provider) checkCoverage(cfg *config.Config) {
	byKey := map[engine.Key][]string{}
	pools := map[engine.Key]map[int]bool{}
	for _, e := range p.engines {
		k := e.inst.Key
		byKey[k] = append(byKey[k], e.inst.ID)
		if pools[k.Base()] == nil {
			pools[k.Base()] = map[int]bool{}
		}
		pools[k.Base()][k.PoolIndex] = true
	}
	for _, k := range sortedKeys(byKey) {
		if ids := byKey[k]; len(ids) > 1 {
			p.log.Warn("several static engines serve the same engine key; new sessions go to any ready one of them",
				"key", k.String(), "engines", ids)
		}
	}
	for _, base := range sortedKeys(pools) {
		size := poolSize(cfg.Profiles[base.Profile])
		var missing []int
		for i := 0; i < size; i++ {
			if !pools[base][i] {
				missing = append(missing, i)
			}
		}
		if len(missing) > 0 {
			p.log.Warn("static engines do not cover the whole pool; sessions that select a missing pool index are rejected",
				"key", base.String(), "poolSize", size, "missingPoolIndices", missing)
		}
	}
}

func sortedKeys[V any](m map[engine.Key]V) []engine.Key {
	keys := make([]engine.Key, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	return keys
}

func newProbeClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               nil, // like the upstream client: engines are reached directly
			DialContext:         (&net.Dialer{Timeout: timeout}).DialContext,
			TLSHandshakeTimeout: timeout,
			// A fresh connection per probe: an idle keep-alive connection
			// would tie up one of the engine's per-connection threads, and a
			// broken one would make a healthy engine look failed.
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Name implements engine.Provider.
func (p *Provider) Name() string { return "static" }

// CanLaunch implements engine.Provider: static engines cannot be launched.
func (p *Provider) CanLaunch() bool { return false }

// NewID implements engine.Provider. The manager never launches engines of a
// provider that cannot launch; the id is unique nevertheless.
func (p *Provider) NewID(key engine.Key, _ bool) string {
	return fmt.Sprintf("static-unlaunchable-%s-%d", strings.ToLower(string(key.ShareLevel)), p.seq.Add(1))
}

// SpecHash implements engine.Provider. Static engines have no launch spec,
// so they never become outdated.
func (p *Provider) SpecHash(string, config.Profile) string { return "" }

// Run implements engine.Provider: it probes all engines every health
// interval until ctx is done, then closes the events channel.
func (p *Provider) Run(ctx context.Context) error {
	if !p.running.CompareAndSwap(false, true) {
		return errors.New("static provider: Run called twice")
	}
	defer close(p.done)
	defer close(p.events)

	evs, ok := p.probeRound(ctx)
	if !ok {
		return nil
	}
	close(p.synced)
	if !p.send(ctx, evs) {
		return nil
	}
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		evs, ok := p.probeRound(ctx)
		if !ok || !p.send(ctx, evs) {
			return nil
		}
	}
}

// probeRound probes every engine and applies the results. It returns the
// events to send, or false if ctx ended during the round.
func (p *Provider) probeRound(ctx context.Context) ([]engine.Instance, bool) {
	errs := make([]error, len(p.engines))
	sem := make(chan struct{}, maxParallelProbes)
	var wg sync.WaitGroup
	for i, e := range p.engines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			errs[i] = p.probe(ctx, e.probeURL)
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil, false
	}
	var evs []engine.Instance
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, e := range p.engines {
		if ev, ok := p.applyLocked(e, errs[i]); ok {
			evs = append(evs, ev)
		}
	}
	return evs, true
}

// applyLocked records a probe result and returns the event to send, if any.
func (p *Provider) applyLocked(e *entry, probeErr error) (engine.Instance, bool) {
	prev := e.inst.Phase
	if probeErr == nil {
		e.failures = 0
		e.inst.Phase, e.inst.Message = engine.PhaseReady, MessageHealthy
		switch prev {
		case engine.PhaseStarting:
			p.log.Info("static engine is healthy", "engine", e.inst.ID, "endpoint", e.inst.Endpoint)
		case engine.PhaseFailed:
			p.log.Info("static engine recovered", "engine", e.inst.ID, "endpoint", e.inst.Endpoint)
		}
		return e.inst, true // see the package documentation
	}
	e.failures++
	if prev != engine.PhaseFailed && e.failures < p.failures {
		// Not yet: a single failed probe does not take the engine (and all
		// its sessions) out of rotation.
		p.log.Warn("static engine missed a health check", "engine", e.inst.ID, "endpoint", e.inst.Endpoint,
			"failures", e.failures, "threshold", p.failures, "err", probeErr)
		return engine.Instance{}, false
	}
	e.inst.Phase, e.inst.Message = engine.PhaseFailed, "health check failed: "+probeErr.Error()
	if prev == engine.PhaseFailed {
		return engine.Instance{}, false
	}
	p.log.Warn("static engine failed its health check", "engine", e.inst.ID, "endpoint", e.inst.Endpoint, "err", probeErr)
	return e.inst, true
}

// probe sends GET / and expects HTTP 200.
func (p *Provider) probe(ctx context.Context, target string) error {
	pctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "quackmux-health")
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("GET %s: no answer within %s", target, p.timeout)
		}
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %s", target, resp.Status)
	}
	return nil
}

func (p *Provider) send(ctx context.Context, evs []engine.Instance) bool {
	for _, ev := range evs {
		select {
		case p.events <- ev:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// WaitSynced implements engine.Provider: it returns once the first probe
// round is complete, so that List reports every engine as ready or failed.
func (p *Provider) WaitSynced(ctx context.Context) error {
	select {
	case <-p.synced:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		select {
		case <-p.synced:
			return nil
		default:
			return errors.New("static provider stopped before its first health check round completed")
		}
	}
}

// List implements engine.Provider: all engines with their current phase, in
// configuration order.
func (p *Provider) List(context.Context) ([]engine.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]engine.Instance, len(p.engines))
	for i, e := range p.engines {
		out[i] = e.inst
	}
	return out, nil
}

// Launch implements engine.Provider: not supported.
func (p *Provider) Launch(context.Context, engine.LaunchRequest) (engine.Instance, error) {
	return engine.Instance{}, engine.ErrNotSupported
}

// Claim implements engine.Provider: not supported (there are no warm pools).
func (p *Provider) Claim(context.Context, string, engine.Key) error {
	return engine.ErrNotSupported
}

// Terminate implements engine.Provider. Static engines are managed elsewhere:
// the call changes nothing and the engine stays registered.
func (p *Provider) Terminate(_ context.Context, id, reason string) error {
	p.log.Debug("ignoring terminate request: static engines are managed externally", "engine", id, "reason", reason)
	return nil
}

// Events implements engine.Provider.
func (p *Provider) Events() <-chan engine.Instance { return p.events }
