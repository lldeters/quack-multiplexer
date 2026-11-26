// Package admin serves the gateway's operator interface on the admin
// listener: liveness and readiness probes, Prometheus metrics and a small
// JSON API to inspect and manage sessions and engines.
//
// Endpoints (everything under /api requires "Authorization: Bearer <token>"
// when a token is configured; probes and metrics never do):
//
//	GET    /healthz                          liveness: 200 "ok", 503 when not live
//	GET    /readyz                           readiness: 200 "ok", 503 with the reason
//	GET    /metrics                          Prometheus exposition format
//	GET    /api/v1/info                      version, session and engine counts by state
//	GET    /api/v1/sessions[?user=][&engine=] {"sessions": [...], "count": n}
//	GET    /api/v1/sessions/{id}             {"session": {...}, "engine": {...}}
//	DELETE /api/v1/sessions/{id}[?force=true] close a session (409 while requests run, unless forced)
//	POST   /api/v1/sessions/{id}/cancel      cancel the running query of a session
//	GET    /api/v1/engines                   {"engines": [...], "count": n}
//	GET    /api/v1/engines/{id}              {"engine": {...}, "sessions": [...]}
//	POST   /api/v1/engines/{id}/drain        take an engine out of rotation (202)
//	DELETE /api/v1/engines/{id}              terminate an engine and close its sessions (202)
//	GET    /api/v1/volumes[?owner=]          {"volumes": [...], "count": n} (persistent profiles)
//	DELETE /api/v1/volumes/{name}            delete a data volume whose key has no engine (202)
//
// API errors are JSON objects of the form {"error": "..."} with a matching
// HTTP status code. GET requests never change any state.
package admin

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"quackmux/internal/engine"
	"quackmux/internal/metrics"
	"quackmux/internal/session"
)

// Sessions is the part of the session manager the admin API uses.
type Sessions interface {
	// List returns open sessions, optionally filtered by user and engine.
	List(user, engineID string) []session.View
	// Get returns one open session.
	Get(id string) (session.View, bool)
	// Close ends a session. It returns false if the session is not open or,
	// without mode.Force, has requests in flight.
	Close(id, reason string, mode session.CloseMode) bool
	// Cancel cancels the running query of a session.
	Cancel(ctx context.Context, id string) error
	// Count returns the number of open sessions.
	Count() int
}

// Engines is the part of the engine manager the admin API uses.
type Engines interface {
	// List returns all known engines.
	List() []engine.View
	// Get returns one engine.
	Get(id string) (engine.View, bool)
	// Drain stops routing new sessions to an engine.
	Drain(id, reason string) error
	// Terminate stops an engine immediately and closes its sessions.
	Terminate(id, reason string) error
}

// The concrete managers must satisfy the interfaces.
var (
	_ Sessions = (*session.Manager)(nil)
	_ Engines  = (*engine.Manager)(nil)
)

// Reasons recorded for operator actions. They start with "admin" so that the
// session and engine managers attribute them to the "admin" metric label.
const (
	ReasonSessionClosed    = "admin: closed by operator"
	ReasonEngineDrained    = "admin: drained by operator"
	ReasonEngineTerminated = "admin: terminated by operator"
)

// cancelTimeout bounds a CANCEL sent to an engine on behalf of an operator.
// The upstream client applies its own (shorter) control timeout as well.
const cancelTimeout = 30 * time.Second

// MaxRequestDuration is the longest an admin request takes (cancelTimeout,
// volumeTimeout). The ReadTimeout of the admin listener must be longer:
// net/http cancels the request context when the read deadline passes, also
// while the handler runs.
const MaxRequestDuration = max(cancelTimeout, volumeTimeout)

// Options wires a Server.
type Options struct {
	// Sessions and Engines are the managers the API inspects and controls.
	// A nil manager is treated as one without any sessions or engines.
	Sessions Sessions
	Engines  Engines
	// Volumes manages the data volumes of persistent profiles; nil means
	// there are none.
	Volumes Volumes
	// Metrics is exposed on /metrics; nil disables the endpoint.
	Metrics *metrics.Metrics
	// Token is the bearer token required for /api. Empty disables
	// authentication (only acceptable on a private network). Surrounding
	// white space (such as the newline of a token file) is ignored.
	Token string
	// Ready reports readiness and, if not ready, why (e.g. "not the
	// leader"). nil means always ready.
	Ready func() (bool, string)
	// Live reports liveness. nil means always live.
	Live func() bool
	// Log receives operator actions and authentication failures.
	Log *slog.Logger
	// Version is reported by /api/v1/info.
	Version string
}

// Server is the http.Handler of the admin listener.
type Server struct {
	sessions  Sessions
	engines   Engines
	volumes   Volumes
	tokenHash *[sha256.Size]byte // nil: no authentication
	ready     func() (bool, string)
	live      func() bool
	log       *slog.Logger
	version   string
	startedAt time.Time
	now       func() time.Time
	handler   http.Handler
}

// New creates the admin handler.
func New(o Options) *Server {
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Server{
		sessions:  o.Sessions,
		engines:   o.Engines,
		volumes:   o.Volumes,
		ready:     o.Ready,
		live:      o.Live,
		log:       log.With("component", "admin"),
		version:   o.Version,
		startedAt: time.Now(),
		now:       time.Now,
	}
	if s.sessions == nil {
		s.log.Warn("admin API has no session manager; it reports no sessions")
		s.sessions = noSessions{}
	}
	if s.engines == nil {
		s.log.Warn("admin API has no engine manager; it reports no engines")
		s.engines = noEngines{}
	}
	if s.version == "" {
		s.version = "unknown"
	}
	if tok := strings.TrimSpace(o.Token); tok != "" {
		h := sha256.Sum256([]byte(tok))
		s.tokenHash = &h
	} else {
		s.log.Warn("admin API is not protected by a bearer token; anyone who can reach the admin listener can manage sessions and engines")
	}

	api := http.NewServeMux()
	s.route(api, "/api/v1/info", methods{http.MethodGet: s.handleInfo})
	s.route(api, "/api/v1/sessions", methods{http.MethodGet: s.handleListSessions})
	s.route(api, "/api/v1/sessions/{id}", methods{
		http.MethodGet:    s.handleGetSession,
		http.MethodDelete: s.handleCloseSession,
	})
	s.route(api, "/api/v1/sessions/{id}/cancel", methods{http.MethodPost: s.handleCancelSession})
	s.route(api, "/api/v1/engines", methods{http.MethodGet: s.handleListEngines})
	s.route(api, "/api/v1/engines/{id}", methods{
		http.MethodGet:    s.handleGetEngine,
		http.MethodDelete: s.handleTerminateEngine,
	})
	s.route(api, "/api/v1/engines/{id}/drain", methods{http.MethodPost: s.handleDrainEngine})
	s.route(api, "/api/v1/volumes", methods{http.MethodGet: s.handleListVolumes})
	s.route(api, "/api/v1/volumes/{name}", methods{http.MethodDelete: s.handleDeleteVolume})
	api.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.writeError(w, http.StatusNotFound, "no such endpoint: "+r.URL.Path)
	})

	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", s.handleHealthz)
	root.HandleFunc("GET /readyz", s.handleReadyz)
	if o.Metrics != nil && o.Metrics.Registry != nil {
		reg := o.Metrics.Registry
		root.Handle("GET /metrics", promhttp.InstrumentMetricHandler(reg, promhttp.HandlerFor(reg, promhttp.HandlerOpts{
			ErrorLog:      promLogger{s.log},
			ErrorHandling: promhttp.ContinueOnError,
			Registry:      reg,
		})))
	}
	root.Handle("/api/", s.authenticate(api))
	s.handler = root
	return s
}

// SetClock replaces the clock used for uptime reporting (tests).
func (s *Server) SetClock(now func() time.Time) {
	s.now = now
	s.startedAt = now()
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// methods maps HTTP methods to the handlers of one path.
type methods map[string]http.HandlerFunc

// route registers the handlers of a path and a fallback that answers every
// other method with a JSON 405 (the mux's own 405 would be plain text).
func (s *Server) route(mux *http.ServeMux, path string, ms methods) {
	allowed := make([]string, 0, len(ms)+1)
	for m, h := range ms {
		mux.HandleFunc(m+" "+path, h)
		allowed = append(allowed, m)
		if m == http.MethodGet {
			allowed = append(allowed, http.MethodHead) // GET patterns also match HEAD
		}
	}
	sort.Strings(allowed)
	allow := strings.Join(allowed, ", ")
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		s.writeError(w, http.StatusMethodNotAllowed, fmt.Sprintf("method %s is not allowed here (allowed: %s)", r.Method, allow))
	})
}

// authenticate enforces the bearer token, if one is configured.
func (s *Server) authenticate(next http.Handler) http.Handler {
	if s.tokenHash == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		given, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="quackmux-admin"`)
			s.writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		// Comparing digests keeps the comparison constant-time regardless of
		// the length of the presented token.
		h := sha256.Sum256([]byte(given))
		if subtle.ConstantTimeCompare(h[:], s.tokenHash[:]) != 1 {
			s.log.Warn("rejected admin request with invalid bearer token",
				"remote", r.RemoteAddr, "method", r.Method, "path", r.URL.Path)
			w.Header().Set("WWW-Authenticate", `Bearer realm="quackmux-admin", error="invalid_token"`)
			s.writeError(w, http.StatusUnauthorized, "invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken extracts the credentials of an "Authorization: Bearer <token>"
// header. The scheme is case-insensitive (RFC 9110, section 11.1).
func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	if s.live != nil && !s.live() {
		writeText(w, http.StatusServiceUnavailable, "unhealthy")
		return
	}
	writeText(w, http.StatusOK, "ok")
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if s.ready != nil {
		if ok, reason := s.ready(); !ok {
			if reason == "" {
				reason = "not ready"
			}
			writeText(w, http.StatusServiceUnavailable, reason)
			return
		}
	}
	writeText(w, http.StatusOK, "ok")
}

// query returns the query parameters of r after checking that it names only
// the allowed ones, so that a typo such as "?usr=alice" or "?forse=true" is
// reported instead of silently ignored.
func (s *Server) query(w http.ResponseWriter, r *http.Request, allowed ...string) (url.Values, bool) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed query string: "+err.Error())
		return nil, false
	}
	for name := range q {
		known := false
		for _, a := range allowed {
			if name == a {
				known = true
				break
			}
		}
		if !known {
			msg := fmt.Sprintf("unknown query parameter %q", name)
			if len(allowed) > 0 {
				msg += " (supported: " + strings.Join(allowed, ", ") + ")"
			}
			s.writeError(w, http.StatusBadRequest, msg)
			return nil, false
		}
	}
	return q, true
}

// errorBody is the JSON shape of every API error.
type errorBody struct {
	Error string `json:"error"`
}

func (s *Server) writeError(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, errorBody{Error: msg})
}

// writeJSON encodes v completely before sending anything, so that a client
// never receives a success status with a truncated document.
func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		s.log.Error("encoding admin response failed", "err", err)
		status = http.StatusInternalServerError
		body = []byte(`{"error": "internal error: response could not be encoded"}`)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func writeText(w http.ResponseWriter, status int, msg string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg)
}

// promLogger forwards errors of the metrics handler to slog.
type promLogger struct{ log *slog.Logger }

func (l promLogger) Println(v ...any) {
	l.log.Error("serving metrics failed", "err", strings.TrimSuffix(fmt.Sprintln(v...), "\n"))
}

// noSessions stands in for a missing session manager.
type noSessions struct{}

func (noSessions) List(string, string) []session.View           { return nil }
func (noSessions) Get(string) (session.View, bool)              { return session.View{}, false }
func (noSessions) Close(string, string, session.CloseMode) bool { return false }
func (noSessions) Cancel(context.Context, string) error         { return session.ErrNotFound }
func (noSessions) Count() int                                   { return 0 }

// noEngines stands in for a missing engine manager.
type noEngines struct{}

func (noEngines) List() []engine.View            { return nil }
func (noEngines) Get(string) (engine.View, bool) { return engine.View{}, false }
func (noEngines) Drain(id, _ string) error       { return fmt.Errorf("engine %q not found", id) }
func (noEngines) Terminate(id, _ string) error   { return fmt.Errorf("engine %q not found", id) }
