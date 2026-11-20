package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quackmux/internal/config"
	"quackmux/internal/engine"
)

const (
	// DefaultProbeInterval is the period of the readiness probe (GET /) while
	// an engine starts.
	DefaultProbeInterval = 200 * time.Millisecond
	// DefaultStopTimeout is how long Terminate waits for the CLI to exit after
	// closing its standard input before it kills the process.
	DefaultStopTimeout = 5 * time.Second

	// Defaults of provider.process.
	defaultDuckDBPath = "duckdb"
	defaultBindHost   = "127.0.0.1"
	defaultPortStart  = 19500
	defaultPortEnd    = 19999
)

const (
	// probeTimeout bounds one readiness probe.
	probeTimeout = 2 * time.Second
	// waitDelay bounds how long reaping an exited process may wait for its
	// output pipes (held open by descendants, if any).
	waitDelay = time.Second
	// killWait bounds the wait for a killed process to be reaped.
	killWait = 10 * time.Second
	// eventBuffer is the capacity of the events channel.
	eventBuffer = 1024
	// terminatedTTL is how long Terminate remembers ids it did not know, so
	// that a launch that loses the race against its own termination is
	// refused instead of leaving an engine nobody tracks.
	terminatedTTL = 10 * time.Minute
)

var errStopped = errors.New("process provider is shut down")

// CommandFunc creates the command of an engine process without starting it;
// exec.Command is the default. The provider sets Dir, Stdin, Stdout, Stderr,
// WaitDelay and SysProcAttr itself and extends Env (the gateway's environment
// if Env is nil). Tests use it to substitute a fake engine.
type CommandFunc func(name string, args ...string) *exec.Cmd

// Option customizes a Provider.
type Option func(*Provider)

// WithCommand replaces exec.Command for starting engine processes.
func WithCommand(f CommandFunc) Option {
	return func(p *Provider) {
		if f != nil {
			p.command, p.customCommand = f, true
		}
	}
}

// WithProbeInterval sets the period of the readiness probe.
func WithProbeInterval(d time.Duration) Option {
	return func(p *Provider) {
		if d > 0 {
			p.probeEvery = d
		}
	}
}

// WithStopTimeout sets how long Terminate waits for an engine to exit after
// closing its standard input before it kills the process.
func WithStopTimeout(d time.Duration) Option {
	return func(p *Provider) {
		if d > 0 {
			p.stopAfter = d
		}
	}
}

// Provider runs engines as local DuckDB CLI processes. It is safe for
// concurrent use.
type Provider struct {
	log           *slog.Logger
	duckdbPath    string // as configured; part of the spec hash
	workDir       string // absolute
	bindHost      string // the address engines listen on
	dialHost      string // the address the gateway reaches them at
	portStart     int
	portEnd       int
	command       CommandFunc
	customCommand bool
	probeEvery    time.Duration
	stopAfter     time.Duration
	client        *http.Client

	events   chan engine.Instance
	stopping chan struct{} // closed when Run begins to shut down
	done     chan struct{} // closed when Run has returned
	running  atomic.Bool

	// emitMu orders sends on events against closing the channel.
	emitMu       sync.RWMutex
	eventsClosed bool

	// wg counts registered engines whose records are not gone yet. Every
	// event is sent before the count of its engine drops, so Run can close
	// the events channel once the count is zero.
	wg sync.WaitGroup

	warnedEnv sync.Map // profile name -> struct{}

	mu         sync.Mutex
	engines    map[string]*proc
	ports      map[int]string // port -> id of the engine holding it
	nextPort   int
	terminated map[string]time.Time
	shutdown   bool
}

// proc is the record of one engine process.
type proc struct {
	id       string
	scratch  string
	port     int
	endpoint string
	output   *output

	// Set before the supervisor starts and immutable afterwards.
	ownsDir bool
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	waitErr error // valid once exited is closed

	stop     chan struct{} // closed when the engine is asked to stop
	stopOnce sync.Once
	exited   chan struct{} // closed when the process has exited and was reaped
	done     chan struct{} // closed when the record is gone

	// Guarded by Provider.mu.
	inst   engine.Instance
	reason string // why the engine is stopped (the message of PhaseGone)
}

func (pr *proc) requestStop() { pr.stopOnce.Do(func() { close(pr.stop) }) }

func (pr *proc) stopRequested() bool {
	select {
	case <-pr.stop:
		return true
	default:
		return false
	}
}

var _ engine.Provider = (*Provider)(nil)

// New creates a process provider from cfg.Provider.Process. Unset fields get
// the configuration defaults (DuckDB CLI "duckdb" from PATH, 127.0.0.1,
// ports 19500-19999, work directory <os.TempDir()>/quackmux-engines).
func New(cfg *config.Config, log *slog.Logger, opts ...Option) (*Provider, error) {
	if cfg == nil {
		return nil, errors.New("process provider: no configuration")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	pc := cfg.Provider.Process
	p := &Provider{
		log:        log.With("component", "process-engines"),
		duckdbPath: pc.DuckDBPath,
		workDir:    pc.WorkDir,
		bindHost:   strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(pc.BindHost), "["), "]"),
		portStart:  pc.PortRangeStart,
		portEnd:    pc.PortRangeEnd,
		command:    exec.Command,
		probeEvery: DefaultProbeInterval,
		stopAfter:  DefaultStopTimeout,
		events:     make(chan engine.Instance, eventBuffer),
		stopping:   make(chan struct{}),
		done:       make(chan struct{}),
		engines:    map[string]*proc{},
		ports:      map[int]string{},
		terminated: map[string]time.Time{},
	}
	if p.duckdbPath == "" {
		p.duckdbPath = defaultDuckDBPath
	}
	if p.bindHost == "" {
		p.bindHost = defaultBindHost
	}
	if p.portStart == 0 && p.portEnd == 0 {
		p.portStart, p.portEnd = defaultPortStart, defaultPortEnd
	}
	if p.portStart < 1 || p.portEnd > 65535 || p.portStart > p.portEnd {
		return nil, fmt.Errorf("process provider: port range %d-%d is invalid", p.portStart, p.portEnd)
	}
	p.nextPort = p.portStart
	if p.workDir == "" {
		p.workDir = filepath.Join(os.TempDir(), "quackmux-engines")
	}
	wd, err := filepath.Abs(p.workDir)
	if err != nil {
		return nil, fmt.Errorf("process provider: work directory %q: %w", p.workDir, err)
	}
	p.workDir = wd
	p.dialHost = dialHost(p.bindHost)
	for _, o := range opts {
		o(p)
	}
	p.client = &http.Client{
		Transport: &http.Transport{
			Proxy:       nil, // engines are local: never ask an HTTP proxy
			DialContext: (&net.Dialer{Timeout: probeTimeout}).DialContext,
			// The engine serves every TCP connection with a thread of its
			// own; an idle keep-alive probe connection would tie one up.
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if !p.customCommand {
		if _, err := exec.LookPath(p.duckdbPath); err != nil {
			p.log.Warn("DuckDB CLI not found: launching engines fails until provider.process.duckdbPath names it",
				"duckdbPath", p.duckdbPath, "err", err)
		}
	}
	return p, nil
}

// Name implements engine.Provider.
func (p *Provider) Name() string { return "process" }

// CanLaunch implements engine.Provider.
func (p *Provider) CanLaunch() bool { return true }

// NewID implements engine.Provider: proc-<level>-<owner>-<random> with the
// owner reduced to [a-z0-9-], or proc-warm-<profile>-<random> for warm-pool
// engines. The id is also the name of the engine's scratch directory.
func (p *Provider) NewID(key engine.Key, warm bool) string {
	parts := []string{string(key.ShareLevel), key.Owner}
	if warm {
		parts = []string{"warm", key.Profile}
	}
	head := idHead(parts...)
	for {
		id := head + "-" + randomSuffix()
		p.mu.Lock()
		_, live := p.engines[id]
		_, gone := p.terminated[id]
		p.mu.Unlock()
		if !live && !gone {
			return id
		}
	}
}

// SpecHash implements engine.Provider. It covers the profile and the DuckDB
// binary, the inputs of what Launch runs.
func (p *Provider) SpecHash(profileName string, profile config.Profile) string {
	return engine.HashJSON(struct {
		Name       string
		Profile    config.Profile
		DuckDBPath string
	}{profileName, profile, p.duckdbPath})
}

// Run implements engine.Provider. It blocks until ctx is done, then
// terminates all engines and closes the events channel.
func (p *Provider) Run(ctx context.Context) error {
	if !p.running.CompareAndSwap(false, true) {
		return errors.New("process provider: Run called twice")
	}
	defer close(p.done)
	<-ctx.Done()

	p.mu.Lock()
	p.shutdown = true
	close(p.stopping)
	live := make([]*proc, 0, len(p.engines))
	for _, pr := range p.engines {
		if pr.reason == "" {
			pr.reason = "gateway shutdown"
		}
		live = append(live, pr)
	}
	p.mu.Unlock()
	if len(live) > 0 {
		p.log.Info("stopping engine processes", "count", len(live))
	}
	for _, pr := range live {
		pr.requestStop()
	}
	p.wg.Wait()

	p.emitMu.Lock()
	p.eventsClosed = true
	close(p.events)
	p.emitMu.Unlock()
	return nil
}

// Done is closed when Run has returned, that is after every engine process
// was stopped and its scratch directory removed.
func (p *Provider) Done() <-chan struct{} { return p.done }

// WaitSynced implements engine.Provider. There is nothing to discover: the
// provider only knows the engines it started itself.
func (p *Provider) WaitSynced(context.Context) error { return nil }

// List implements engine.Provider: the engines of this provider instance that
// were not terminated yet, oldest first.
func (p *Provider) List(context.Context) ([]engine.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]engine.Instance, 0, len(p.engines))
	for _, pr := range p.engines {
		out = append(out, pr.inst)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Launch implements engine.Provider. It starts the process and returns
// without waiting for readiness; PhaseReady or PhaseFailed follow as events.
func (p *Provider) Launch(ctx context.Context, req engine.LaunchRequest) (engine.Instance, error) {
	if err := ctx.Err(); err != nil {
		return engine.Instance{}, err
	}
	if !validID(req.ID) {
		return engine.Instance{}, fmt.Errorf("invalid engine id %q", req.ID)
	}
	if req.Token == "" {
		return engine.Instance{}, errors.New("launch request without engine token")
	}
	pr := &proc{
		id:      req.ID,
		scratch: filepath.Join(p.workDir, req.ID),
		output:  newOutput(req.Token, p.log.With("engine", req.ID)),
		stop:    make(chan struct{}),
		exited:  make(chan struct{}),
		done:    make(chan struct{}),
	}

	p.mu.Lock()
	if p.shutdown {
		p.mu.Unlock()
		return engine.Instance{}, errStopped
	}
	if _, ok := p.engines[req.ID]; ok {
		p.mu.Unlock()
		return engine.Instance{}, fmt.Errorf("engine %s exists already", req.ID)
	}
	if _, ok := p.terminated[req.ID]; ok {
		p.mu.Unlock()
		return engine.Instance{}, fmt.Errorf("engine %s was terminated before it was launched", req.ID)
	}
	port, err := p.allocatePortLocked()
	if err != nil {
		p.mu.Unlock()
		return engine.Instance{}, err
	}
	pr.port = port
	pr.endpoint = "http://" + net.JoinHostPort(p.dialHost, strconv.Itoa(port))
	pr.inst = engine.Instance{
		ID:          req.ID,
		Key:         req.Key,
		Warm:        req.Warm,
		ProfileName: req.ProfileName,
		SpecHash:    req.SpecHash,
		Phase:       engine.PhaseStarting,
		Token:       req.Token,
		User:        req.User,
		CreatedAt:   time.Now(),
	}
	p.engines[req.ID] = pr
	p.ports[port] = req.ID
	p.wg.Add(1)
	p.mu.Unlock()

	if err := p.start(pr, req); err != nil {
		p.release(pr, false)
		return engine.Instance{}, fmt.Errorf("start engine process: %w", err)
	}
	pid := pr.cmd.Process.Pid
	p.mu.Lock()
	pr.inst.Message = fmt.Sprintf("pid %d, port %d", pid, port)
	inst := pr.inst
	p.mu.Unlock()
	p.log.Info("engine process started", "engine", req.ID, "pid", pid, "port", port, "dir", pr.scratch)
	go p.supervise(pr, serveScript(req.Profile, p.bindHost, port, req.Token))
	return inst, nil
}

// start creates the scratch directory and starts the process.
func (p *Provider) start(pr *proc, req engine.LaunchRequest) error {
	if err := os.MkdirAll(p.workDir, 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(pr.scratch, 0o700); err != nil {
		return err
	}
	pr.ownsDir = true

	database := databaseArg(req.Profile.Database)
	cmd := p.command(p.duckdbPath, "-bail", database)
	env, skipped := engineEnv(cmd.Env, req, database, pr.port)
	if len(skipped) > 0 {
		if _, warned := p.warnedEnv.LoadOrStore(req.ProfileName, struct{}{}); !warned {
			p.log.Warn("skipping profile env entries the process provider cannot apply (valueFrom, reserved QUACK_TOKEN/QUACKMUX_* or invalid names)",
				"profile", req.ProfileName, "names", skipped)
		}
	}
	if runtime.GOOS != "windows" && runtime.GOOS != "plan9" {
		// What exec.Cmd does for a nil Env: PWD names the new working
		// directory instead of the gateway's (the last entry wins).
		env = append(env, "PWD="+pr.scratch)
	}
	cmd.Env = env
	cmd.Dir = pr.scratch
	cmd.Stdout = pr.output // the same writer: one pipe, output in order
	cmd.Stderr = pr.output
	cmd.WaitDelay = waitDelay
	setProcAttr(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return err
	}
	pr.cmd, pr.stdin = cmd, stdin
	go func() {
		pr.waitErr = cmd.Wait()
		close(pr.exited)
	}()
	return nil
}

// readiness is the outcome of awaitReady.
type readiness int

const (
	becameReady readiness = iota
	exitedEarly
	stopRequested
)

// supervise owns an engine after its start: it sends the SQL, reports the
// engine's phases in order and stops and removes it on request.
func (p *Provider) supervise(pr *proc, script string) {
	go func() {
		// An error means the process is gone, which the supervisor reports.
		_, _ = io.WriteString(pr.stdin, script)
	}()

	switch p.awaitReady(pr) {
	case becameReady:
		p.mu.Lock()
		pr.inst.Phase, pr.inst.Endpoint = engine.PhaseReady, pr.endpoint
		inst := pr.inst
		p.mu.Unlock()
		p.log.Debug("engine process ready", "engine", pr.id, "endpoint", pr.endpoint)
		p.emit(inst)
		select {
		case <-pr.stop:
		case <-pr.exited:
			if !pr.stopRequested() {
				p.failed(pr, "exited unexpectedly")
				<-pr.stop
			}
		}
	case exitedEarly:
		p.failed(pr, "exited before the Quack server was ready")
		<-pr.stop
	case stopRequested:
	}
	p.stopProcess(pr)
	p.release(pr, true)
}

// awaitReady probes the engine until it answers, exits or is asked to stop.
func (p *Provider) awaitReady(pr *proc) readiness {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// Abort a probe in flight as soon as it no longer matters.
		select {
		case <-pr.stop:
		case <-pr.exited:
		case <-ctx.Done():
		}
		cancel()
	}()
	ticker := time.NewTicker(p.probeEvery)
	defer ticker.Stop()
	for {
		if pr.stopRequested() {
			return stopRequested
		}
		select {
		case <-pr.exited:
			return exitedEarly
		default:
		}
		if p.probe(ctx, pr.endpoint) {
			return becameReady
		}
		select {
		case <-pr.stop:
			return stopRequested
		case <-pr.exited:
			if pr.stopRequested() {
				return stopRequested
			}
			return exitedEarly
		case <-ticker.C:
		}
	}
}

// probe reports whether GET / on the engine answers with HTTP 200.
func (p *Provider) probe(ctx context.Context, endpoint string) bool {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/", nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "quackmux-probe")
	resp, err := p.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode == http.StatusOK
}

// failed reports an engine whose process exited on its own. Its port is free
// again; the record and the scratch directory stay until Terminate.
func (p *Provider) failed(pr *proc, what string) {
	msg := fmt.Sprintf("%s %s (%s)", filepath.Base(p.duckdbPath), what, exitStatus(pr.waitErr))
	if tail := pr.output.Tail(); tail != "" {
		msg += ": " + tail
	}
	p.mu.Lock()
	p.releasePortLocked(pr)
	pr.inst.Phase, pr.inst.Message = engine.PhaseFailed, msg
	inst := pr.inst
	p.mu.Unlock()
	p.log.Debug("engine process failed", "engine", pr.id, "status", exitStatus(pr.waitErr))
	p.emit(inst)
}

func exitStatus(err error) string {
	if err == nil {
		return "exit status 0"
	}
	return err.Error()
}

// stopProcess closes the engine's input and kills it if it does not exit in
// time. It returns once the process is gone (or could not be reaped).
func (p *Provider) stopProcess(pr *proc) {
	select {
	case <-pr.exited:
		return
	default:
	}
	_ = pr.stdin.Close() // end of input: the CLI exits and stops serving
	timer := time.NewTimer(p.stopAfter)
	defer timer.Stop()
	select {
	case <-pr.exited:
		return
	case <-timer.C:
	}
	p.log.Warn("engine process did not exit after its input was closed, killing it",
		"engine", pr.id, "pid", pr.cmd.Process.Pid, "timeout", p.stopAfter)
	if err := kill(pr.cmd); err != nil && !errors.Is(err, os.ErrProcessDone) {
		p.log.Error("killing engine process failed", "engine", pr.id, "pid", pr.cmd.Process.Pid, "err", err)
	}
	select {
	case <-pr.exited:
	case <-time.After(killWait):
		p.log.Error("engine process did not exit after it was killed", "engine", pr.id, "pid", pr.cmd.Process.Pid)
	}
}

// release removes an engine's record, port and scratch directory. Every
// registered engine ends here exactly once. gone reports PhaseGone; an
// engine whose start failed only reports it if it was asked to stop
// meanwhile (otherwise its launch fails and nobody knows it).
func (p *Provider) release(pr *proc, gone bool) {
	if pr.ownsDir {
		if err := removeAll(pr.scratch); err != nil {
			p.log.Warn("removing engine directory failed", "engine", pr.id, "dir", pr.scratch, "err", err)
		}
	}
	p.mu.Lock()
	delete(p.engines, pr.id)
	p.releasePortLocked(pr)
	inst, reason := pr.inst, pr.reason
	p.mu.Unlock()
	if gone || reason != "" {
		if gone {
			p.log.Info("engine process stopped", "engine", pr.id, "reason", reason)
		}
		inst.Phase, inst.Message = engine.PhaseGone, reason
		p.emit(inst)
	}
	close(pr.done)
	p.wg.Done()
}

func (p *Provider) releasePortLocked(pr *proc) {
	if p.ports[pr.port] == pr.id {
		delete(p.ports, pr.port)
	}
}

// removeAll removes a directory tree, retrying for a moment: on Windows
// files may stay locked briefly after their process exited (virus scanners,
// the search indexer).
func removeAll(dir string) error {
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

// Claim implements engine.Provider. Keys only live in memory.
func (p *Provider) Claim(_ context.Context, id string, key engine.Key, user string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	pr := p.engines[id]
	if pr == nil {
		return fmt.Errorf("engine %s not found", id)
	}
	pr.inst.Key, pr.inst.Warm, pr.inst.User = key, false, user
	return nil
}

// Terminate implements engine.Provider: it stops the process (see the
// package documentation) and returns once the engine is gone, or with an
// error when ctx ends first; the engine is removed in the background then.
// For an unknown id it reports PhaseGone right away: the manager waits for
// that event before it forgets an engine, also one whose launch failed.
func (p *Provider) Terminate(ctx context.Context, id, reason string) error {
	if reason == "" {
		reason = "terminated"
	}
	p.mu.Lock()
	pr := p.engines[id]
	if pr == nil {
		p.rememberTerminatedLocked(id)
		p.mu.Unlock()
		p.emit(engine.Instance{ID: id, Phase: engine.PhaseGone, Message: reason})
		return nil
	}
	if pr.reason == "" {
		pr.reason = reason
	}
	p.mu.Unlock()
	pr.requestStop()
	select {
	case <-pr.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("engine %s is still stopping: %w", id, ctx.Err())
	}
}

func (p *Provider) rememberTerminatedLocked(id string) {
	now := time.Now()
	for k, at := range p.terminated {
		if now.Sub(at) > terminatedTTL {
			delete(p.terminated, k)
		}
	}
	p.terminated[id] = now
}

// Events implements engine.Provider.
func (p *Provider) Events() <-chan engine.Instance { return p.events }

// emit delivers an event. It blocks while the channel is full, except during
// shutdown, when nobody may be reading any more: then the event is only
// delivered if it fits into the buffer.
func (p *Provider) emit(inst engine.Instance) {
	p.emitMu.RLock()
	defer p.emitMu.RUnlock()
	if p.eventsClosed {
		return
	}
	select {
	case p.events <- inst:
		return
	case <-p.stopping:
	}
	select {
	case p.events <- inst:
	default:
	}
}

// allocatePortLocked returns a port of the range that no engine of this
// provider holds and that can be bound right now. It continues after the
// port handed out last, so that ports are not reused immediately.
func (p *Provider) allocatePortLocked() (int, error) {
	n := p.portEnd - p.portStart + 1
	for i := 0; i < n; i++ {
		port := p.portStart + (p.nextPort-p.portStart+i)%n
		if _, taken := p.ports[port]; taken {
			continue
		}
		if !portFree(p.bindHost, port) {
			continue
		}
		p.nextPort = port + 1
		if p.nextPort > p.portEnd {
			p.nextPort = p.portStart
		}
		return port, nil
	}
	return 0, fmt.Errorf("no free TCP port in %d-%d on %s (provider.process.portRangeStart/portRangeEnd)",
		p.portStart, p.portEnd, p.bindHost)
}

// portFree reports whether host:port can be bound (the listener is released
// at once; the engine binds the port itself).
func portFree(host string, port int) bool {
	l, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}
