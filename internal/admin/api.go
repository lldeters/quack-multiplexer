package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"quackmux/internal/engine"
	"quackmux/internal/session"
)

// sessionList is the response of GET /api/v1/sessions.
type sessionList struct {
	Sessions []session.View `json:"sessions"`
	Count    int            `json:"count"`
}

// sessionDetail is the response of GET /api/v1/sessions/{id}. Engine is
// omitted when the session's engine is no longer known.
type sessionDetail struct {
	Session session.View `json:"session"`
	Engine  *engine.View `json:"engine,omitempty"`
}

// sessionClosed is the response of DELETE /api/v1/sessions/{id}.
type sessionClosed struct {
	ID     string `json:"id"`
	Closed bool   `json:"closed"`
	Forced bool   `json:"forced"`
	Reason string `json:"reason"`
}

// sessionCancelled is the response of POST /api/v1/sessions/{id}/cancel.
type sessionCancelled struct {
	ID        string `json:"id"`
	Cancelled bool   `json:"cancelled"`
}

// engineList is the response of GET /api/v1/engines.
type engineList struct {
	Engines []engine.View `json:"engines"`
	Count   int           `json:"count"`
}

// engineDetail is the response of GET /api/v1/engines/{id}.
type engineDetail struct {
	Engine   engine.View    `json:"engine"`
	Sessions []session.View `json:"sessions"`
}

// engineAction is the response of the asynchronous engine operations (drain,
// terminate): the engine as it is right after the request was accepted.
type engineAction struct {
	Engine engine.View `json:"engine"`
}

// info is the response of GET /api/v1/info.
type info struct {
	Version     string         `json:"version"`
	StartedAt   time.Time      `json:"startedAt"`
	Uptime      string         `json:"uptime"`
	Ready       bool           `json:"ready"`
	ReadyReason string         `json:"readyReason,omitempty"`
	Sessions    sessionSummary `json:"sessions"`
	Engines     engineSummary  `json:"engines"`
}

type sessionSummary struct {
	Total   int            `json:"total"`
	ByState map[string]int `json:"byState"`
}

type engineSummary struct {
	Total   int            `json:"total"`
	ByState map[string]int `json:"byState"`
	// Warm counts unclaimed warm-pool engines (included in ByState).
	Warm int `json:"warm"`
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.query(w, r); !ok {
		return
	}
	resp := info{
		Version:   s.version,
		StartedAt: s.startedAt,
		Uptime:    s.now().Sub(s.startedAt).Round(time.Second).String(),
		Ready:     true,
		Sessions:  sessionSummary{Total: s.sessions.Count(), ByState: map[string]int{}},
		Engines:   engineSummary{ByState: map[string]int{}},
	}
	if s.ready != nil {
		ok, reason := s.ready()
		resp.Ready = ok
		if !ok {
			resp.ReadyReason = reason
		}
	}
	for _, v := range s.sessions.List("", "") {
		resp.Sessions.ByState[string(v.State)]++
	}
	for _, v := range s.engines.List() {
		resp.Engines.Total++
		resp.Engines.ByState[string(v.State)]++
		if v.Warm {
			resp.Engines.Warm++
		}
	}
	s.writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	q, ok := s.query(w, r, "user", "engine")
	if !ok {
		return
	}
	list := nonNil(s.sessions.List(q.Get("user"), q.Get("engine")))
	s.writeJSON(w, http.StatusOK, sessionList{Sessions: list, Count: len(list)})
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.query(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	v, ok := s.sessions.Get(id)
	if !ok {
		s.sessionNotFound(w, id)
		return
	}
	resp := sessionDetail{Session: v}
	if ev, ok := s.engines.Get(v.EngineID); ok {
		resp.Engine = &ev
	}
	s.writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleCloseSession(w http.ResponseWriter, r *http.Request) {
	q, ok := s.query(w, r, "force")
	if !ok {
		return
	}
	force, err := boolParam(q.Get("force"))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid value for force: "+err.Error())
		return
	}
	id := r.PathValue("id")
	v, ok := s.sessions.Get(id)
	if !ok {
		s.sessionNotFound(w, id)
		return
	}
	mode := session.CloseMode{Disconnect: true, Cancel: force, Force: force, MustInvalidate: false}
	if s.sessions.Close(id, ReasonSessionClosed, mode) {
		s.log.Info("session closed by operator", "session", id, "user", v.User, "engine", v.EngineID,
			"force", force, "remote", r.RemoteAddr)
		s.writeJSON(w, http.StatusOK, sessionClosed{ID: id, Closed: true, Forced: force, Reason: ReasonSessionClosed})
		return
	}
	// Close refuses sessions that are no longer open and, unless forced,
	// sessions with requests in flight.
	cur, still := s.sessions.Get(id)
	switch {
	case !still || cur.State != session.StateOpen:
		s.sessionNotFound(w, id)
	case !force:
		msg := fmt.Sprintf("session %q has requests in flight; retry later or use force=true to cancel them and close the session", id)
		if cur.InFlight > 0 {
			msg = fmt.Sprintf("session %q has %d request(s) in flight; retry later or use force=true to cancel them and close the session", id, cur.InFlight)
		}
		s.writeError(w, http.StatusConflict, msg)
	default:
		s.writeError(w, http.StatusConflict, fmt.Sprintf("session %q could not be closed", id))
	}
}

func (s *Server) handleCancelSession(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.query(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	v, ok := s.sessions.Get(id)
	if !ok {
		s.sessionNotFound(w, id)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), cancelTimeout)
	defer cancel()
	err := s.sessions.Cancel(ctx, id)
	switch {
	case err == nil:
		s.log.Info("query cancelled by operator", "session", id, "user", v.User, "engine", v.EngineID, "remote", r.RemoteAddr)
		s.writeJSON(w, http.StatusOK, sessionCancelled{ID: id, Cancelled: true})
	case errors.Is(err, session.ErrNotFound):
		s.sessionNotFound(w, id)
	case r.Context().Err() != nil:
		// The operator went away; nobody reads the answer.
		s.log.Debug("cancel request abandoned by client", "session", id, "err", err)
	case errors.Is(err, context.DeadlineExceeded):
		s.log.Warn("cancel by operator timed out", "session", id, "engine", v.EngineID, "err", err)
		s.writeError(w, http.StatusGatewayTimeout, fmt.Sprintf("cancelling the query of session %q timed out: %v", id, err))
	default:
		s.log.Warn("cancel by operator failed", "session", id, "engine", v.EngineID, "err", err)
		s.writeError(w, http.StatusBadGateway, fmt.Sprintf("cancelling the query of session %q failed: %v", id, err))
	}
}

func (s *Server) handleListEngines(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.query(w, r); !ok {
		return
	}
	list := s.engines.List()
	if list == nil {
		list = []engine.View{}
	}
	s.writeJSON(w, http.StatusOK, engineList{Engines: list, Count: len(list)})
}

func (s *Server) handleGetEngine(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.query(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	v, ok := s.engines.Get(id)
	if !ok {
		s.engineNotFound(w, id)
		return
	}
	s.writeJSON(w, http.StatusOK, engineDetail{Engine: v, Sessions: nonNil(s.sessions.List("", id))})
}

func (s *Server) handleDrainEngine(w http.ResponseWriter, r *http.Request) {
	s.engineAction(w, r, "drain", ReasonEngineDrained, s.engines.Drain)
}

func (s *Server) handleTerminateEngine(w http.ResponseWriter, r *http.Request) {
	s.engineAction(w, r, "terminate", ReasonEngineTerminated, s.engines.Terminate)
}

// engineAction runs an asynchronous engine operation and answers 202 with
// the engine's state right afterwards.
func (s *Server) engineAction(w http.ResponseWriter, r *http.Request, name, reason string, op func(id, reason string) error) {
	if _, ok := s.query(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	before, ok := s.engines.Get(id)
	if !ok {
		s.engineNotFound(w, id)
		return
	}
	if err := op(id, reason); err != nil {
		if _, still := s.engines.Get(id); !still {
			s.engineNotFound(w, id)
			return
		}
		s.log.Warn("engine operation by operator failed", "op", name, "engine", id, "err", err)
		s.writeError(w, http.StatusConflict, fmt.Sprintf("%s engine %q: %v", name, id, err))
		return
	}
	s.log.Info("engine operation by operator", "op", name, "engine", id, "key", before.Key,
		"state", before.State, "sessions", before.Sessions, "remote", r.RemoteAddr)
	after, ok := s.engines.Get(id)
	if !ok {
		// Already gone (e.g. an idle engine terminated at once).
		after = before
	}
	s.writeJSON(w, http.StatusAccepted, engineAction{Engine: after})
}

func (s *Server) sessionNotFound(w http.ResponseWriter, id string) {
	s.writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", id))
}

func (s *Server) engineNotFound(w http.ResponseWriter, id string) {
	s.writeError(w, http.StatusNotFound, fmt.Sprintf("engine %q not found", id))
}

// boolParam parses an optional boolean query parameter; empty means false.
func boolParam(v string) (bool, error) {
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%q is not a boolean", v)
	}
	return b, nil
}

// nonNil makes empty lists encode as [] instead of null.
func nonNil(v []session.View) []session.View {
	if v == nil {
		return []session.View{}
	}
	return v
}
