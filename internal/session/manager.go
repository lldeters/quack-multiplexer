// Package session tracks client sessions (Quack connections) and enforces
// their lifecycle: heartbeat leases, idle timeouts, maximum lifetimes, and
// clean-up on engine loss.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"quackmux/internal/config"
	"quackmux/internal/engine"
	"quackmux/internal/metrics"
	"quackmux/internal/upstream"
	"quackmux/internal/wire"
)

// Engines is the part of the engine manager the session manager uses.
type Engines interface {
	IDByUID(uid engine.UID) (string, bool)
	Lookup(id string) (engine.Target, bool)
	Exists(id string) (engine.State, bool)
	AttachAdopted(id, sessionID, user string)
	DetachSession(id string)
}

// Backend sends control messages to engines.
type Backend interface {
	Disconnect(ctx context.Context, endpoint, backendConnID string) error
	CancelAll(ctx context.Context, endpoint, backendConnID string) error
	Heartbeat(ctx context.Context, endpoint, backendConnID string) error
	// Connections lists the connections an engine holds (see orphans.go).
	Connections(ctx context.Context, endpoint, token string) ([]string, error)
}

// State of a session.
type State string

const (
	StateOpen   State = "OPEN"
	StateClosed State = "CLOSED"
)

// Session is one client connection routed to one engine.
type Session struct {
	ID               string
	User             string
	Groups           []string
	EngineID         string
	EngineKey        engine.Key
	BackendConnID    string
	ClientID         string
	ClientVersion    string
	ClientPlatform   string
	RemoteAddr       string
	HeartbeatTimeout time.Duration
	CreatedAt        time.Time
	Adopted          bool

	lastSeen    atomic.Int64 // unix nanos, any message
	lastActive  atomic.Int64 // unix nanos, any message but heartbeats
	lastBackend atomic.Int64 // unix nanos, last request that reached the engine
	keepalive   atomic.Bool  // a backend keep-alive is in flight
	requests    atomic.Int64
	queries     atomic.Int64

	mu               sync.Mutex
	state            State
	inFlight         int
	closeReason      string
	closeInvalidates bool
	closedAt         time.Time
}

// Closed describes why a session is no longer usable.
type Closed struct {
	Reason string
	// MustInvalidate tells the client that its attached database is gone
	// (the engine died), not just this connection.
	MustInvalidate bool
	// Transient marks a temporary condition (the engine of an adopted
	// session is not ready yet); the session itself is not closed.
	Transient bool
	ClosedAt  time.Time
}

// View is a snapshot of a session.
type View struct {
	ID               string    `json:"id"`
	User             string    `json:"user"`
	Groups           []string  `json:"groups,omitempty"`
	State            State     `json:"state"`
	EngineID         string    `json:"engineId"`
	EngineKey        string    `json:"engineKey"`
	ShareLevel       string    `json:"shareLevel"`
	Subdomain        string    `json:"subdomain"`
	Profile          string    `json:"profile"`
	ClientID         string    `json:"clientId,omitempty"`
	ClientVersion    string    `json:"clientVersion,omitempty"`
	ClientPlatform   string    `json:"clientPlatform,omitempty"`
	RemoteAddr       string    `json:"remoteAddr,omitempty"`
	HeartbeatTimeout string    `json:"heartbeatTimeout"`
	CreatedAt        time.Time `json:"createdAt"`
	LastSeen         time.Time `json:"lastSeen"`
	LastActive       time.Time `json:"lastActive"`
	InFlight         int       `json:"inFlight"`
	Requests         int64     `json:"requests"`
	Queries          int64     `json:"queries"`
	Adopted          bool      `json:"adopted"`
	CloseReason      string    `json:"closeReason,omitempty"`
}

type tombstone struct {
	Closed
	user string
	// held marks a session whose DISCONNECT waits for the close hook: the
	// engine still has its connection, so the session must stay closed
	// whatever the age of the tombstone.
	held bool
}

// Errors.
var (
	ErrLimit    = errors.New("session limit reached")
	ErrNotFound = errors.New("session not found")
)

// Manager tracks all sessions of this gateway process.
type Manager struct {
	cfg     *config.Config
	engines Engines
	backend Backend
	sealer  *Sealer
	log     *slog.Logger
	met     *metrics.Metrics
	now     func() time.Time
	// fence, if set, must report true for Reap to act (see SetFence).
	fence func() bool
	// closeHook, if set, runs after every close (see SetCloseHook).
	closeHook func(s *Session, keep bool) <-chan struct{}

	mu         sync.RWMutex
	sessions   map[string]*Session
	tombstones map[string]tombstone
	perUser    map[string]int
	pending    map[string]int // connects in progress per user
	total      int
	wg         sync.WaitGroup // background control requests
	bgMu       sync.Mutex     // orders spawn against Shutdown
	bgClosed   bool
	stop       chan struct{} // closed by Shutdown
	// disconnectRetries are the waits before the retries of a DISCONNECT
	// that failed (see disconnect).
	disconnectRetries []time.Duration

	// adoptions confirms each adopted session with the engine once, also
	// when many requests of it arrive at the same time.
	adoptions singleflight.Group
}

// NewManager creates a session manager.
func NewManager(cfg *config.Config, engines Engines, backend Backend, sealer *Sealer, log *slog.Logger, met *metrics.Metrics) *Manager {
	return &Manager{
		cfg:        cfg,
		engines:    engines,
		backend:    backend,
		sealer:     sealer,
		log:        log.With("component", "sessions"),
		met:        met,
		now:        time.Now,
		sessions:   map[string]*Session{},
		tombstones: map[string]tombstone{},
		perUser:    map[string]int{},
		pending:    map[string]int{},
		stop:       make(chan struct{}),

		disconnectRetries: []time.Duration{time.Second, 3 * time.Second},
	}
}

// SetClock replaces the clock (tests).
func (m *Manager) SetClock(now func() time.Time) { m.now = now }

// SetFence makes Reap do nothing while fence reports false. With leader
// election it reports whether this process still holds a fresh lease
// (ha.Elector.Fresh): a leader that resumes after a freeze sees the
// heartbeats of its sessions missing, because the new leader received them,
// and must not close those sessions on the engines. Call it before Run.
func (m *Manager) SetFence(fence func() bool) { m.fence = fence }

// SetCloseHook makes f run after a session was closed, for whatever reason,
// with the closed session; the proxy drops the state it keeps per session
// there. f must not block. Call it before sessions are opened.
//
// keep tells f that the engine still has the session's connection and is
// about to get its DISCONNECT. f may then return a channel: the manager sends
// the DISCONNECT only once the channel is closed (or the manager shuts down),
// and keeps the lease of the connection alive until then. The Quack server
// lets the requests of a connection that are in flight finish after a
// DISCONNECT, and their statement runs on with them; the query time limit
// needs the connection to cancel it. Until the DISCONNECT the session stays
// closed, however old its tombstone: the engine would confirm the
// connection to an adoption.
func (m *Manager) SetCloseHook(f func(s *Session, keep bool) <-chan struct{}) { m.closeHook = f }

// Reservation holds a slot under the session limits while a connect is in
// progress. Exactly one of Open or Cancel must be called.
type Reservation struct {
	m    *Manager
	user string
	done atomic.Bool
}

// Reserve claims a session slot for user, enforcing limits.maxSessions and
// the per-user limit (maxPerUser <= 0 means unlimited).
func (m *Manager) Reserve(user string, maxPerUser int) (*Reservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if max := m.cfg.Limits.MaxSessions; max > 0 && m.total+m.pendingTotalLocked() >= max {
		return nil, fmt.Errorf("%w: %d sessions open (limits.maxSessions)", ErrLimit, m.total)
	}
	if maxPerUser > 0 && m.perUser[user]+m.pending[user] >= maxPerUser {
		return nil, fmt.Errorf("%w: user %q already has %d sessions", ErrLimit, user, m.perUser[user])
	}
	m.pending[user]++
	return &Reservation{m: m, user: user}, nil
}

func (m *Manager) pendingTotalLocked() int {
	n := 0
	for _, v := range m.pending {
		n += v
	}
	return n
}

// Cancel returns the slot.
func (r *Reservation) Cancel() {
	if r.done.Swap(true) {
		return
	}
	r.m.mu.Lock()
	r.m.dropPendingLocked(r.user)
	r.m.mu.Unlock()
}

func (m *Manager) dropPendingLocked(user string) {
	if m.pending[user] <= 1 {
		delete(m.pending, user)
	} else {
		m.pending[user]--
	}
}

// OpenParams describe a freshly connected session.
type OpenParams struct {
	SessionID        string
	User             string
	Groups           []string
	EngineID         string
	EngineKey        engine.Key
	BackendConnID    string
	HeartbeatTimeout time.Duration
	ClientID         string
	ClientVersion    string
	ClientPlatform   string
	RemoteAddr       string
}

// Open registers a session and returns the client-facing connection id.
func (r *Reservation) Open(p OpenParams) (*Session, string, error) {
	m := r.m
	now := m.now()
	route := Route{
		SessionID:        p.SessionID,
		EngineUID:        engine.UIDOf(p.EngineID),
		BackendConnID:    p.BackendConnID,
		User:             p.User,
		CreatedUnix:      now.Unix(),
		HeartbeatTimeout: uint64(p.HeartbeatTimeout / time.Second),
	}
	id, err := m.sealer.Seal(route)
	if err != nil {
		return nil, "", err
	}
	s := &Session{
		ID: p.SessionID, User: p.User, Groups: p.Groups, EngineID: p.EngineID, EngineKey: p.EngineKey,
		BackendConnID: p.BackendConnID, ClientID: p.ClientID, ClientVersion: p.ClientVersion,
		ClientPlatform: p.ClientPlatform, RemoteAddr: p.RemoteAddr, HeartbeatTimeout: p.HeartbeatTimeout,
		CreatedAt: now, state: StateOpen,
	}
	s.lastSeen.Store(now.UnixNano())
	s.lastActive.Store(now.UnixNano())
	s.lastBackend.Store(now.UnixNano())
	if r.done.Swap(true) {
		return nil, "", errors.New("reservation already used")
	}
	m.mu.Lock()
	m.dropPendingLocked(r.user)
	m.sessions[s.ID] = s
	m.perUser[s.User]++
	m.total++
	m.mu.Unlock()
	m.metricOpened(s)
	m.log.Info("session opened", "session", s.ID, "user", s.User, "engine", s.EngineID, "key", s.EngineKey.String(),
		"client", s.ClientVersion, "remote", s.RemoteAddr)
	return s, id, nil
}

func (m *Manager) metricOpened(s *Session) {
	if m.met == nil {
		return
	}
	m.met.SessionsOpen.WithLabelValues(string(s.EngineKey.ShareLevel), s.EngineKey.Profile).Inc()
	m.met.SessionsOpened.WithLabelValues(string(s.EngineKey.ShareLevel), s.EngineKey.Profile, fmt.Sprint(s.Adopted)).Inc()
}

// Resolve maps a client connection id to an open session. It returns
// (nil, closed, nil) for sessions the gateway closed, adopts sessions it does
// not know yet (after a restart) and fails with ErrInvalidID for ids it did
// not mint.
func (m *Manager) Resolve(connID string) (*Session, *Closed, error) {
	route, err := m.sealer.Open(connID)
	if err != nil {
		return nil, nil, err
	}
	m.mu.RLock()
	s := m.sessions[route.SessionID]
	t, dead := m.tombstones[route.SessionID]
	m.mu.RUnlock()
	if s != nil {
		return s, nil, nil
	}
	if dead {
		c := t.Closed
		return nil, &c, nil
	}
	return m.adopt(route)
}

func (m *Manager) adopt(route Route) (*Session, *Closed, error) {
	engineID, known := m.engines.IDByUID(route.EngineUID)
	if !known {
		return nil, &Closed{Reason: "engine no longer exists", MustInvalidate: true, ClosedAt: m.now()}, nil
	}
	target, ok := m.engines.Lookup(engineID)
	if !ok {
		st, _ := m.engines.Exists(engineID)
		if st == engine.StateStarting {
			// A recovered engine whose pod is momentarily not ready: the
			// backend connection may well be alive, so do not give up on it.
			return nil, &Closed{Reason: "engine " + engineID + " is not ready yet, try again shortly",
				Transient: true, ClosedAt: m.now()}, nil
		}
		return nil, &Closed{Reason: fmt.Sprintf("engine is %s", st), MustInvalidate: true, ClosedAt: m.now()}, nil
	}
	// Adopt only connections the engine still has. The gateway disconnects
	// every session it closes, so this keeps a closed session from coming
	// back once its tombstone expired or a restart forgot it (DuckDB clients
	// keep heartbeating while attached). It also renews the engine lease.
	v, err, _ := m.adoptions.Do(route.SessionID, func() (any, error) {
		at := m.now()
		return at, m.backend.Heartbeat(context.Background(), target.Endpoint, route.BackendConnID)
	})
	if err != nil {
		var ee *upstream.EngineError
		if errors.As(err, &ee) && upstream.IsSessionGone(ee.Response.Message) {
			c := Closed{Reason: "the engine no longer knows this connection", ClosedAt: m.now()}
			m.mu.Lock()
			if m.sessions[route.SessionID] == nil {
				m.tombstones[route.SessionID] = tombstone{Closed: c, user: route.User}
			}
			m.mu.Unlock()
			return nil, &c, nil
		}
		m.log.Warn("cannot confirm a session with its engine", "session", route.SessionID, "engine", engineID, "err", err)
		return nil, &Closed{Reason: "engine " + engineID + " is not reachable, try again shortly",
			Transient: true, ClosedAt: m.now()}, nil
	}
	confirmedAt := v.(time.Time)
	now := m.now()
	key := target.Key
	if key.ShareLevel == engine.ShareConnection && key.Owner == "" {
		// An unclaimed warm engine that carries this session (the claim was
		// not persisted before the previous gateway process died).
		key.Owner = route.SessionID
	}
	s := &Session{
		ID: route.SessionID, User: route.User, EngineID: engineID, EngineKey: key,
		BackendConnID: route.BackendConnID, HeartbeatTimeout: time.Duration(route.HeartbeatTimeout) * time.Second,
		CreatedAt: time.Unix(route.CreatedUnix, 0), Adopted: true, state: StateOpen,
	}
	if s.HeartbeatTimeout <= 0 {
		s.HeartbeatTimeout = m.cfg.Session.MaxHeartbeatTimeout.Duration
	}
	s.lastSeen.Store(now.UnixNano())
	s.lastActive.Store(now.UnixNano())
	s.lastBackend.Store(confirmedAt.UnixNano()) // the confirming heartbeat renewed the engine lease
	// Attach to the engine before the session becomes visible: a close right
	// after (engine gone, admin) detaches it, which must not come first.
	m.engines.AttachAdopted(s.EngineID, s.ID, s.User)
	m.mu.Lock()
	if existing := m.sessions[s.ID]; existing != nil { // lost a race with a concurrent adoption
		m.mu.Unlock()
		m.engines.DetachSession(s.EngineID)
		return existing, nil, nil
	}
	if t, dead := m.tombstones[s.ID]; dead {
		m.mu.Unlock()
		m.engines.DetachSession(s.EngineID)
		c := t.Closed
		return nil, &c, nil
	}
	m.sessions[s.ID] = s
	m.perUser[s.User]++
	m.total++
	m.mu.Unlock()
	m.metricOpened(s)
	m.log.Info("session adopted", "session", s.ID, "user", s.User, "engine", s.EngineID)
	return s, nil, nil
}

// Closed reports why the session was closed, and false while it is open.
func (s *Session) Closed() (*Closed, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateClosed {
		return nil, false
	}
	return &Closed{Reason: s.closeReason, MustInvalidate: s.closeInvalidates, ClosedAt: s.closedAt}, true
}

// Begin marks a request of the session as in flight. It fails if the
// session was closed concurrently.
func (s *Session) Begin(now time.Time, t wire.MessageType) (*Closed, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateOpen {
		return &Closed{Reason: s.closeReason, ClosedAt: s.closedAt}, false
	}
	s.inFlight++
	n := now.UnixNano()
	s.lastSeen.Store(n)
	if t != wire.TypeHeartbeatRequest {
		s.lastActive.Store(n)
	}
	s.requests.Add(1)
	if t == wire.TypePrepareRequest {
		s.queries.Add(1)
	}
	return nil, true
}

// TouchBackend records that a message of this session reached the engine at
// or after t: the engine renews its lease when a message arrives, so t is the
// time the message was sent, not when it was answered. The stamp only moves
// forward (a long request may be answered after a later keep-alive).
func (s *Session) TouchBackend(t time.Time) {
	n := t.UnixNano()
	for {
		old := s.lastBackend.Load()
		if n <= old || s.lastBackend.CompareAndSwap(old, n) {
			return
		}
	}
}

// End marks the end of a request started with Begin.
func (s *Session) End(now time.Time, t wire.MessageType) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight--
	n := now.UnixNano()
	s.lastSeen.Store(n)
	if t != wire.TypeHeartbeatRequest {
		s.lastActive.Store(n)
	}
}

// CloseMode controls what Close does besides forgetting the session.
type CloseMode struct {
	// Disconnect sends DISCONNECT to the engine (frees the backend
	// connection; the stock server never expires leases by itself).
	Disconnect bool
	// Cancel cancels a running query first.
	Cancel bool
	// MustInvalidate is reported to the client on its next request.
	MustInvalidate bool
	// Force closes even with requests in flight.
	Force bool
	// gone marks a connection the engine no longer has.
	gone bool
}

// Close ends a session. It returns false if the session was not open or had
// requests in flight (unless mode.Force).
func (m *Manager) Close(id, reason string, mode CloseMode) bool {
	m.mu.RLock()
	s := m.sessions[id]
	m.mu.RUnlock()
	if s == nil {
		return false
	}
	return m.close(s, reason, mode, nil)
}

// close transitions s to CLOSED. check, if set, runs under the session lock
// and may veto the close (used by the reaper to re-check idleness).
func (m *Manager) close(s *Session, reason string, mode CloseMode, check func(*Session) bool) bool {
	now := m.now()
	s.mu.Lock()
	if s.state != StateOpen || (s.inFlight > 0 && !mode.Force) || (check != nil && !check(s)) {
		s.mu.Unlock()
		return false
	}
	s.state, s.closeReason, s.closeInvalidates, s.closedAt = StateClosed, reason, mode.MustInvalidate, now
	s.mu.Unlock()

	m.mu.Lock()
	delete(m.sessions, s.ID)
	m.tombstones[s.ID] = tombstone{Closed: Closed{Reason: reason, MustInvalidate: mode.MustInvalidate, ClosedAt: now}, user: s.User}
	if m.perUser[s.User] <= 1 {
		delete(m.perUser, s.User)
	} else {
		m.perUser[s.User]--
	}
	m.total--
	m.mu.Unlock()

	m.engines.DetachSession(s.EngineID)
	if m.met != nil {
		m.met.SessionsOpen.WithLabelValues(string(s.EngineKey.ShareLevel), s.EngineKey.Profile).Dec()
		m.met.SessionsClosed.WithLabelValues(reasonLabel(reason)).Inc()
	}
	m.log.Info("session closed", "session", s.ID, "user", s.User, "engine", s.EngineID, "reason", reason,
		"age", now.Sub(s.CreatedAt).Round(time.Second))
	var target engine.Target
	reachable := false
	if mode.Disconnect || mode.Cancel {
		target, reachable = m.engines.Lookup(s.EngineID)
	}
	keep := mode.Disconnect && !mode.gone && reachable
	var hold <-chan struct{}
	if m.closeHook != nil {
		hold = m.closeHook(s, keep)
	}
	if !keep {
		hold = nil // only a DISCONNECT that is about to go out can wait
	}
	if hold != nil {
		m.setHeld(s.ID, true)
	}

	if (mode.Disconnect || mode.Cancel) && reachable {
		m.spawn(func() {
			ctx := context.Background()
			if mode.Cancel {
				if err := m.backend.CancelAll(ctx, target.Endpoint, s.BackendConnID); err != nil && !sessionGone(err) {
					m.log.Warn("cancelling the query of a closed session failed; it may keep running on the engine",
						"session", s.ID, "engine", s.EngineID, "err", err)
				}
			}
			if mode.Disconnect {
				if hold != nil {
					m.awaitRelease(s, target, hold)
				}
				m.disconnect(s, target)
				if hold != nil {
					m.setHeld(s.ID, false)
				}
			}
		})
	}
	return true
}

// disconnect sends the DISCONNECT of a closed session to its engine and
// retries it after a failure (m.disconnectRetries). A connection the engine
// keeps would bring the session back if the client is still attached when
// a gateway restart forgets the tombstone (adoption confirms the session
// with the engine), until its engine lease runs out. A connection the engine
// no longer knows counts as closed. A shutdown ends the retries.
func (m *Manager) disconnect(s *Session, target engine.Target) {
	var err error
	for attempt := 0; ; attempt++ {
		err = m.backend.Disconnect(context.Background(), target.Endpoint, s.BackendConnID)
		if err == nil || sessionGone(err) {
			return
		}
		if attempt >= len(m.disconnectRetries) {
			break
		}
		select {
		case <-time.After(m.disconnectRetries[attempt]):
		case <-m.stop:
			m.log.Warn("closing the engine connection of a closed session was cut short by the shutdown",
				"session", s.ID, "engine", s.EngineID, "err", err)
			return
		}
	}
	m.log.Warn("closing the engine connection of a closed session failed; the engine keeps it until its lease runs out",
		"session", s.ID, "engine", s.EngineID, "attempts", len(m.disconnectRetries)+1, "err", err)
}

// sessionGone reports whether err is the engine's answer for a connection
// it no longer has.
func sessionGone(err error) bool {
	var ee *upstream.EngineError
	return errors.As(err, &ee) && upstream.IsSessionGone(ee.Response.Message)
}

// setHeld marks the tombstone of a closed session as held (see tombstone).
func (m *Manager) setHeld(id string, held bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tombstones[id]; ok {
		t.held = held
		m.tombstones[id] = t
	}
}

// awaitRelease waits until release is closed or the manager shuts down, and
// keeps the lease of the closed session's engine connection alive meanwhile,
// as Reap does for open sessions.
func (m *Manager) awaitRelease(s *Session, target engine.Target, release <-chan struct{}) {
	var tick <-chan time.Time
	if every := m.cfg.Session.BackendHeartbeatTimeout.Duration / 3; every > 0 {
		t := time.NewTicker(every)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-release:
			return
		case <-m.stop:
			return
		case <-tick:
			if err := m.backend.Heartbeat(context.Background(), target.Endpoint, s.BackendConnID); err != nil {
				m.log.Debug("backend keep-alive of a closed session failed", "session", s.ID, "engine", s.EngineID, "err", err)
			}
		}
	}
}

// spawn runs f in the background unless the manager is shutting down.
func (m *Manager) spawn(f func()) bool {
	m.bgMu.Lock()
	defer m.bgMu.Unlock()
	if m.bgClosed {
		return false
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		f()
	}()
	return true
}

// Wait blocks until background control requests finished (tests).
func (m *Manager) Wait() { m.wg.Wait() }

// Shutdown starts no more background control requests and waits for the
// running ones. Handlers may still be running (the HTTP shutdown can time
// out), so starting new ones must stop before the wait. The DISCONNECTs that
// wait for a close hook go out at once.
func (m *Manager) Shutdown() {
	m.bgMu.Lock()
	if !m.bgClosed {
		m.bgClosed = true
		close(m.stop)
	}
	m.bgMu.Unlock()
	m.wg.Wait()
}

func reasonLabel(reason string) string {
	for _, p := range []string{"client disconnect", "idle timeout", "max lifetime", "heartbeat lease expired",
		"engine", "admin", "backend"} {
		if len(reason) >= len(p) && reason[:len(p)] == p {
			return p
		}
	}
	return "other"
}

// ClosedByClient records a successful client DISCONNECT.
func (m *Manager) ClosedByClient(s *Session) {
	m.close(s, "client disconnect", CloseMode{Force: true}, nil)
}

// ClosedByBackend records that the engine no longer knows the connection.
// The DISCONNECT is best effort: should the engine still have the
// connection after all, it is not left behind.
func (m *Manager) ClosedByBackend(s *Session, msg string) {
	m.close(s, "backend closed the connection: "+msg, CloseMode{Force: true, Disconnect: true, gone: true}, nil)
}

// EngineGone implements engine.SessionCloser.
func (m *Manager) EngineGone(engineID, reason string) {
	for _, s := range m.byEngine(engineID) {
		m.close(s, reason, CloseMode{Force: true, MustInvalidate: true}, nil)
	}
}

// DrainEngine implements engine.SessionCloser.
func (m *Manager) DrainEngine(engineID, reason string) {
	for _, s := range m.byEngine(engineID) {
		m.close(s, reason, CloseMode{Force: true, Cancel: true, Disconnect: true, MustInvalidate: true}, nil)
	}
}

func (m *Manager) byEngine(engineID string) []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Session
	for _, s := range m.sessions {
		if s.EngineID == engineID {
			out = append(out, s)
		}
	}
	return out
}

// Reap applies session timeouts. It runs periodically (Run) and may be
// called directly (tests).
func (m *Manager) Reap() {
	if m.fence != nil && !m.fence() {
		m.log.Debug("not applying session timeouts: the leader lease is not fresh")
		return
	}
	now := m.now()
	sc := m.cfg.Session
	m.mu.RLock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.RUnlock()

	for _, s := range all {
		lease := s.HeartbeatTimeout + sc.HeartbeatGrace.Duration
		if now.Sub(time.Unix(0, s.lastSeen.Load())) > lease {
			m.close(s, fmt.Sprintf("heartbeat lease expired (no message for %s)", lease), CloseMode{Disconnect: true},
				func(s *Session) bool { return now.Sub(time.Unix(0, s.lastSeen.Load())) > lease })
			continue
		}
		if idle := sc.IdleTimeout.Duration; idle > 0 && now.Sub(time.Unix(0, s.lastActive.Load())) > idle {
			m.close(s, fmt.Sprintf("idle timeout (%s)", idle), CloseMode{Disconnect: true},
				func(s *Session) bool { return now.Sub(time.Unix(0, s.lastActive.Load())) > idle })
			continue
		}
		if max := sc.MaxLifetime.Duration; max > 0 && now.Sub(s.CreatedAt) > max {
			// Wait for running requests, but not forever.
			force := now.Sub(s.CreatedAt) > max+m.cfg.Engine.DrainTimeout.Duration
			if m.close(s, fmt.Sprintf("max lifetime (%s)", max), CloseMode{Disconnect: true, Cancel: force, Force: force}, nil) {
				continue
			}
		}
		// The gateway answers client heartbeats itself, so it keeps the
		// engine-side lease (session.backendHeartbeatTimeout) alive. The
		// engine renews the lease only when a message arrives, and a long
		// PREPARE or FETCH carries none while it runs: keep-alives go out
		// with requests in flight as well, like the stock client's
		// heartbeats.
		if every := sc.BackendHeartbeatTimeout.Duration / 3; every > 0 &&
			now.Sub(time.Unix(0, s.lastBackend.Load())) > every {
			m.keepAlive(s, now)
		}
	}

	ttl := sc.TombstoneTTL.Duration
	m.mu.Lock()
	for id, t := range m.tombstones {
		if !t.held && now.Sub(t.ClosedAt) > ttl {
			delete(m.tombstones, id)
		}
	}
	m.mu.Unlock()
}

// InFlight returns the number of requests in flight.
func (s *Session) InFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inFlight
}

// keepAlive sends a HEARTBEAT to the engine for a session (asynchronously,
// at most one at a time per session).
func (m *Manager) keepAlive(s *Session, now time.Time) {
	target, ok := m.engines.Lookup(s.EngineID)
	if !ok || !s.keepalive.CompareAndSwap(false, true) {
		return
	}
	if !m.spawn(func() {
		defer s.keepalive.Store(false)
		err := m.backend.Heartbeat(context.Background(), target.Endpoint, s.BackendConnID)
		var ee *upstream.EngineError
		switch {
		case err == nil:
			s.TouchBackend(now)
		case errors.As(err, &ee) && upstream.IsSessionGone(ee.Response.Message):
			m.ClosedByBackend(s, ee.Response.Message)
		default:
			m.log.Debug("backend keep-alive failed", "session", s.ID, "engine", s.EngineID, "err", err)
		}
	}) {
		s.keepalive.Store(false)
	}
}

// Run reaps sessions until ctx is cancelled.
//
// After a pause of the whole process (a stopped container or VM, a stalled
// node) one round is skipped, so that the heartbeats that queued up in the
// meantime are processed before the sessions are judged by them.
func (m *Manager) Run(ctx context.Context) {
	every := m.cfg.Session.CheckInterval.Duration
	t := time.NewTicker(every)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Both cases may be ready at once, and select picks at random.
			if ctx.Err() != nil {
				return
			}
			if gap := time.Since(last); paused(gap, every) {
				m.log.Warn("the gateway was paused, skipping one round of session timeouts", "paused", gap.Round(time.Second))
			} else {
				m.Reap()
			}
			last = time.Now()
		}
	}
}

// paused reports whether gap, the time between the end of a Run round and
// the next tick of its ticker (period every), means that the process stood
// still rather than that it was busy.
func paused(gap, every time.Duration) bool { return gap > 2*every+time.Second }

// Get returns a snapshot of an open session.
func (m *Manager) Get(id string) (View, bool) {
	m.mu.RLock()
	s := m.sessions[id]
	m.mu.RUnlock()
	if s == nil {
		return View{}, false
	}
	return s.view(), true
}

// List returns snapshots of open sessions, optionally filtered by user or
// engine, newest first.
func (m *Manager) List(user, engineID string) []View {
	m.mu.RLock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		if (user == "" || s.User == user) && (engineID == "" || s.EngineID == engineID) {
			all = append(all, s)
		}
	}
	m.mu.RUnlock()
	out := make([]View, 0, len(all))
	for _, s := range all {
		out = append(out, s.view())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Count returns the number of open sessions.
func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.total
}

// CountUser returns the number of open sessions of a user.
func (m *Manager) CountUser(user string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.perUser[user]
}

// Cancel cancels the running query of a session (admin).
func (m *Manager) Cancel(ctx context.Context, id string) error {
	m.mu.RLock()
	s := m.sessions[id]
	m.mu.RUnlock()
	if s == nil {
		return ErrNotFound
	}
	target, ok := m.engines.Lookup(s.EngineID)
	if !ok {
		return fmt.Errorf("engine %s is not available", s.EngineID)
	}
	return m.backend.CancelAll(ctx, target.Endpoint, s.BackendConnID)
}

func (s *Session) view() View {
	s.mu.Lock()
	state, inFlight, reason := s.state, s.inFlight, s.closeReason
	s.mu.Unlock()
	return View{
		ID: s.ID, User: s.User, Groups: s.Groups, State: state, EngineID: s.EngineID,
		EngineKey: s.EngineKey.String(), ShareLevel: string(s.EngineKey.ShareLevel),
		Subdomain: s.EngineKey.Subdomain, Profile: s.EngineKey.Profile, ClientID: s.ClientID,
		ClientVersion: s.ClientVersion, ClientPlatform: s.ClientPlatform, RemoteAddr: s.RemoteAddr,
		HeartbeatTimeout: s.HeartbeatTimeout.String(), CreatedAt: s.CreatedAt,
		LastSeen: time.Unix(0, s.lastSeen.Load()), LastActive: time.Unix(0, s.lastActive.Load()),
		InFlight: inFlight, Requests: s.requests.Load(), Queries: s.queries.Load(), Adopted: s.Adopted,
		CloseReason: reason,
	}
}

// Ensure the upstream client satisfies Backend.
var _ Backend = (*upstream.Client)(nil)
