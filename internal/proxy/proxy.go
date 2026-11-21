// Package proxy implements the Quack-facing HTTP endpoint of the gateway:
// it authenticates connection handshakes, picks engines, rewrites connection
// ids and auth strings, and streams all other messages to the engine that
// owns the session.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"

	"quackmux/internal/auth"
	"quackmux/internal/config"
	"quackmux/internal/engine"
	"quackmux/internal/metrics"
	"quackmux/internal/policy"
	"quackmux/internal/session"
	"quackmux/internal/upstream"
	"quackmux/internal/wire"
)

// maxHeaderBytes bounds the header object (it holds the connection id only).
const maxHeaderBytes = 64 << 10

// maxInspectedErrorBytes bounds an engine ERROR_RESPONSE the gateway reads
// to look at it (must_invalidate, gone connections). Larger ones pass
// through like any other response.
const maxInspectedErrorBytes = 64 << 10

// abortCancelTimeout bounds the CANCEL the gateway sends for a query it
// answers itself while shutting down.
const abortCancelTimeout = 2 * time.Second

// Gateway is the http.Handler for the Quack endpoint.
type Gateway struct {
	cfg      *config.Config
	auth     auth.Authenticator
	engines  *engine.Manager
	sessions *session.Manager
	up       *upstream.Client
	log      *slog.Logger
	audit    *slog.Logger
	met      *metrics.Metrics
	now      func() time.Time
	ready    func() (bool, string)
	buffers  *semaphore.Weighted

	// abortCtx ends when the gateway shuts down: queries still waiting for
	// their engine are then answered with an error (see AbortQueries).
	abortCtx context.Context
	abort    context.CancelFunc

	inFlight atomic.Int64
}

// Options wires a Gateway.
type Options struct {
	Config   *config.Config
	Auth     auth.Authenticator
	Engines  *engine.Manager
	Sessions *session.Manager
	Upstream *upstream.Client
	Log      *slog.Logger
	Metrics  *metrics.Metrics
	// Ready reports whether this replica may serve (synced and leader).
	Ready func() (bool, string)
}

// New creates the gateway handler.
func New(o Options) *Gateway {
	budget := o.Config.Server.Upstream.BufferBudgetBytes
	if budget <= 0 {
		budget = 0
	}
	g := &Gateway{
		cfg: o.Config, auth: o.Auth, engines: o.Engines, sessions: o.Sessions, up: o.Upstream,
		log: o.Log.With("component", "proxy"), met: o.Metrics, now: time.Now, ready: o.Ready,
		buffers: semaphore.NewWeighted(budget),
	}
	g.abortCtx, g.abort = context.WithCancel(context.Background())
	if o.Config.Audit.Enabled {
		g.audit = o.Log.With("component", "audit")
	}
	if g.ready == nil {
		g.ready = func() (bool, string) { return o.Engines.Synced(), "starting up" }
	}
	return g
}

// SetClock replaces the clock (tests).
func (g *Gateway) SetClock(now func() time.Time) { g.now = now }

// AbortQueries answers every query that still waits for its engine with an
// error and cancels it on the engine. Call it shortly before the shutdown
// deadline: a request that ends without an answer when the process exits
// can be sent again by libcurl on a new connection, and the query would run
// twice. FETCH, SEND_DATA and the other messages are left alone; repeating
// them is harmless (the engine keeps fetched batches and drops duplicate
// uploads).
func (g *Gateway) AbortQueries() { g.abort() }

// InFlight returns the number of requests being proxied.
func (g *Gateway) InFlight() int64 { return g.inFlight.Load() }

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/quack" && r.Method == http.MethodPost:
		g.setCORS(w, r)
		if origin := r.Header.Get("Origin"); origin != "" && !g.originAllowed(origin) {
			// Browsers send Origin with every POST, DuckDB's own clients
			// never: a page from another site (or a DNS rebinding) must not
			// be able to run queries, least of all with auth.mode none.
			writeError(w, "quackmux: requests from origin "+strconv.Quote(origin)+" are not allowed (server.allowedOrigins)",
				wire.ExceptionPermission, false)
			g.count(wire.TypeInvalid, "origin", g.now())
			return
		}
		g.handleQuack(w, r)
	case r.URL.Path == "/quack" && r.Method == http.MethodOptions:
		g.setCORS(w, r)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, g.cfg.Server.Banner)
	default:
		http.NotFound(w, r)
	}
}

// originAllowed reports whether server.allowedOrigins admits origin.
func (g *Gateway) originAllowed(origin string) bool {
	for _, o := range g.cfg.Server.AllowedOrigins {
		if o == "*" || o == origin {
			return true
		}
	}
	return false
}

func (g *Gateway) setCORS(w http.ResponseWriter, r *http.Request) {
	origins := g.cfg.Server.AllowedOrigins
	if len(origins) == 0 {
		return
	}
	if len(origins) == 1 && origins[0] == "*" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		return
	}
	origin := r.Header.Get("Origin")
	for _, o := range origins {
		if o == origin {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			return
		}
	}
}

// writeError answers with a Quack ERROR_RESPONSE (always HTTP 200: the DuckDB
// client reports anything else only as "Failed to send message").
func writeError(w http.ResponseWriter, msg, exceptionType string, mustInvalidate bool) {
	body := wire.ErrorResponse{Message: msg, ExceptionType: exceptionType, MustInvalidate: mustInvalidate}.Encode()
	writeBody(w, body)
}

func writeBody(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", wire.ContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (g *Gateway) count(t wire.MessageType, outcome string, start time.Time) {
	if g.met == nil {
		return
	}
	g.met.Requests.WithLabelValues(t.String(), outcome).Inc()
	g.met.RequestSeconds.WithLabelValues(t.String()).Observe(g.now().Sub(start).Seconds())
}

func (g *Gateway) handleQuack(w http.ResponseWriter, r *http.Request) {
	start := g.now()
	g.inFlight.Add(1)
	defer g.inFlight.Add(-1)
	if g.met != nil {
		g.met.InFlight.Inc()
		defer g.met.InFlight.Dec()
	}
	if ok, why := g.ready(); !ok {
		writeError(w, "quackmux: gateway is not ready: "+why, wire.ExceptionConnection, false)
		g.count(wire.TypeInvalid, "not_ready", start)
		return
	}
	// Until the client has shown a valid session id or delivered its whole
	// handshake, the request must arrive within server.readHeaderTimeout:
	// unauthenticated clients cannot park requests (and their buffers)
	// forever. handleConnect and handleRouted lift the deadline before they
	// wait for anything else.
	if d := g.cfg.Server.ReadHeaderTimeout.Duration; d > 0 {
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(d))
	}
	br := bufio.NewReaderSize(r.Body, g.readBufferSize())
	h, hlen, err := peekHeader(br)
	if err != nil {
		writeError(w, "Failed to deserialize: "+err.Error(), wire.ExceptionInvalidInput, false)
		g.count(wire.TypeInvalid, "malformed", start)
		return
	}
	if !h.Type.IsClientRequest() {
		writeError(w, "Unsupported message type for server", wire.ExceptionInvalidInput, false)
		g.count(h.Type, "unsupported", start)
		return
	}
	if h.Type == wire.TypeConnectionRequest {
		outcome := g.handleConnect(w, r, br, h)
		g.count(h.Type, outcome, start)
		return
	}
	outcome := g.handleRouted(w, r, br, h, hlen)
	g.count(h.Type, outcome, start)
}

// readBufferSize is the read buffer of a request: enough for the header
// object (a valid connection id has at most 156 bytes) and for the SQL text
// the audit log records, not more (it is allocated for every request).
func (g *Gateway) readBufferSize() int {
	n := 4 << 10
	if g.audit != nil {
		n = max(n, g.cfg.Audit.MaxQueryBytes+1<<10)
	}
	return min(n, maxHeaderBytes)
}

// liftReadDeadline removes the deadline handleQuack set: the client is
// known now, and the rest of its message may take as long as it takes.
func liftReadDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
}

// peekHeader reads just enough of the body to decode the header object.
func peekHeader(br *bufio.Reader) (wire.Header, int, error) {
	for n := 256; ; n *= 2 {
		if n > maxHeaderBytes {
			n = maxHeaderBytes
		}
		b, peekErr := br.Peek(n)
		h, hlen, err := wire.ParseHeader(b)
		if err == nil {
			return h, hlen, nil
		}
		if !errors.Is(err, wire.ErrTruncated) {
			return h, 0, err
		}
		if peekErr != nil || n == maxHeaderBytes {
			// The body ended (or the header is absurdly large).
			return h, 0, err
		}
	}
}

func (g *Gateway) connectFailed(w http.ResponseWriter, reason, msg, exceptionType string) string {
	if g.met != nil {
		g.met.ConnectFailures.WithLabelValues(reason).Inc()
	}
	writeError(w, msg, exceptionType, false)
	return reason
}

func (g *Gateway) handleConnect(w http.ResponseWriter, r *http.Request, br *bufio.Reader, h wire.Header) string {
	limit := int64(g.cfg.Server.MaxHandshakeBytes)
	body, err := io.ReadAll(io.LimitReader(br, limit+1))
	if err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			writeError(w, "quackmux: the connection request did not arrive within server.readHeaderTimeout", wire.ExceptionInvalidInput, false)
		}
		return "client_error"
	}
	// The whole handshake is in: lift the deadline before waiting for an
	// engine, which can take minutes.
	liftReadDeadline(w)
	if int64(len(body)) > limit {
		return g.connectFailed(w, "malformed", "quackmux: connection request too large", wire.ExceptionInvalidInput)
	}
	_, req, err := wire.DecodeConnectionRequest(body)
	if err != nil {
		return g.connectFailed(w, "malformed", "Failed to deserialize: "+err.Error(), wire.ExceptionInvalidInput)
	}
	if !req.SupportsVersion(wire.ProtocolVersion) {
		return g.connectFailed(w, "version", fmt.Sprintf("Unsupported Quack version - server only supports version %d of quack", wire.ProtocolVersion), wire.ExceptionInvalidInput)
	}
	sc := g.cfg.Session
	if req.HeartbeatTimeoutSeconds == 0 {
		return g.connectFailed(w, "malformed", fmt.Sprintf("heartbeat_timeout out of range - must be between 1 and %d seconds",
			uint64(sc.MaxHeartbeatTimeout.Seconds())), wire.ExceptionInvalidInput)
	}
	hb := clampHeartbeat(req.HeartbeatTimeoutSeconds, sc.MinHeartbeatTimeout.Duration, sc.MaxHeartbeatTimeout.Duration)

	credential, tokenOpts, err := auth.ParseToken(req.AuthString)
	if err != nil {
		return g.connectFailed(w, "options", "quackmux: "+err.Error(), wire.ExceptionInvalidInput)
	}
	opts, err := policy.Options(tokenOpts, r.Header)
	if err != nil {
		return g.connectFailed(w, "options", "quackmux: "+err.Error(), wire.ExceptionInvalidInput)
	}
	if opts[policy.OptUser] != "" && g.cfg.Auth.Mode != "none" {
		// The identity comes from the credential; silently ignoring a
		// requested user would be confusing.
		return g.connectFailed(w, "options", "quackmux: option \"user\" is only accepted with auth.mode=none", wire.ExceptionInvalidInput)
	}
	authOpts := url.Values{}
	for k, v := range opts {
		authOpts.Set(k, v)
	}
	id, err := g.auth.Authenticate(r.Context(), auth.Request{Credential: credential, Options: authOpts, HTTP: r})
	if err != nil {
		if g.met != nil {
			g.met.AuthFailures.WithLabelValues(g.cfg.Auth.Mode).Inc()
		}
		g.log.Info("authentication failed", "remote", r.RemoteAddr, "err", err)
		// Same text as the stock server, so clients behave identically.
		return g.connectFailed(w, "auth", "Authentication failed", wire.ExceptionInvalidInput)
	}

	sessionID := session.NewSessionID()
	dec, err := policy.Resolve(g.cfg, id, opts, sessionID)
	if err != nil {
		g.log.Info("session denied", "user", id.User, "err", err)
		return g.connectFailed(w, "denied", "quackmux: "+err.Error(), wire.ExceptionPermission)
	}
	resv, err := g.sessions.Reserve(id.User, dec.MaxSessions)
	if err != nil {
		return g.connectFailed(w, "limit", "quackmux: "+err.Error(), wire.ExceptionPermission)
	}
	defer resv.Cancel()

	lease, err := g.engines.Acquire(r.Context(), dec.Key, id.User)
	if err != nil {
		if r.Context().Err() != nil {
			return "client_gone"
		}
		g.log.Warn("no engine for session", "user", id.User, "key", dec.Key.String(), "err", err)
		reason := "engine"
		if errors.Is(err, engine.ErrLimit) {
			reason = "limit"
		}
		return g.connectFailed(w, reason, "quackmux: could not provide an engine: "+err.Error(), wire.ExceptionConnection)
	}
	committed := false
	defer func() {
		if !committed {
			lease.Release()
		}
	}()

	// The engine lease is the gateway's business: client heartbeats are
	// answered by the gateway, which renews the (longer) engine lease itself.
	backendReq := req
	backendReq.AuthString = lease.Token
	backendReq.HeartbeatTimeoutSeconds = uint64(sc.BackendHeartbeatTimeout.Duration / time.Second)
	msg := backendReq.Encode(wire.Header{Type: wire.TypeConnectionRequest, ClientQueryID: h.ClientQueryID})

	// The handshake has a deadline of its own and does not end with the
	// client: a client that gives up after the engine created the connection
	// would otherwise leave it behind (the stock server never expires it).
	hsCtx, hsCancel := context.WithTimeout(context.WithoutCancel(r.Context()), g.cfg.Server.Upstream.HandshakeTimeout.Duration)
	g.engines.BeginRequest(lease.ID)
	respBody, err := g.up.RoundTrip(hsCtx, lease.Endpoint, msg)
	g.engines.EndRequest(lease.ID)
	hsCancel()
	if err != nil {
		if r.Context().Err() != nil {
			return "client_gone"
		}
		g.log.Warn("engine handshake failed", "engine", lease.ID, "err", err)
		return g.connectFailed(w, "engine", fmt.Sprintf("quackmux: engine %s is unreachable: %v", lease.ID, err), wire.ExceptionConnection)
	}
	rh, _, err := wire.ParseHeader(respBody)
	if err != nil {
		return g.connectFailed(w, "engine", "quackmux: undecodable engine response: "+err.Error(), wire.ExceptionConnection)
	}
	if rh.Type == wire.TypeConnectionResponse && rh.ConnectionID != "" && r.Context().Err() != nil {
		g.disconnectBackend(lease.Endpoint, rh.ConnectionID)
		return "client_gone"
	}
	if rh.Type == wire.TypeErrorResponse {
		if e, derr := wire.DecodeErrorResponse(respBody); derr == nil {
			g.log.Warn("engine rejected handshake", "engine", lease.ID, "err", e.Message)
			if e.MustInvalidate {
				g.engines.MarkFailed(lease.ID, "engine database invalidated: "+e.Message)
			}
		}
		if g.met != nil {
			g.met.ConnectFailures.WithLabelValues("engine_error").Inc()
		}
		writeBody(w, respBody)
		return "engine_error"
	}
	if rh.Type != wire.TypeConnectionResponse || rh.ConnectionID == "" {
		return g.connectFailed(w, "engine", fmt.Sprintf("quackmux: unexpected engine response %s", rh.Type), wire.ExceptionConnection)
	}
	_, cr, err := wire.DecodeConnectionResponse(respBody)
	if err != nil {
		g.disconnectBackend(lease.Endpoint, rh.ConnectionID)
		return g.connectFailed(w, "engine", "quackmux: undecodable engine response: "+err.Error(), wire.ExceptionConnection)
	}
	if cr.QuackVersion != wire.ProtocolVersion || cr.HeartbeatTimeoutSeconds == 0 {
		g.disconnectBackend(lease.Endpoint, rh.ConnectionID)
		return g.connectFailed(w, "engine", fmt.Sprintf("quackmux: engine speaks Quack version %d (heartbeat %ds), gateway needs %d",
			cr.QuackVersion, cr.HeartbeatTimeoutSeconds, wire.ProtocolVersion), wire.ExceptionConnection)
	}

	// Attach the session to the engine before it becomes visible: a close
	// right after Open (admin, engine gone) detaches it, which must not come
	// first.
	lease.Commit()
	committed = true
	_, gatewayID, err := resv.Open(session.OpenParams{
		SessionID: sessionID, User: id.User, Groups: id.Groups, EngineID: lease.ID, EngineKey: lease.Key,
		BackendConnID: rh.ConnectionID, HeartbeatTimeout: hb,
		ClientID: req.ClientID, ClientVersion: req.ClientDuckDBVersion, ClientPlatform: req.ClientPlatform,
		RemoteAddr: r.RemoteAddr,
	})
	if err != nil {
		g.engines.DetachSession(lease.ID)
		g.disconnectBackend(lease.Endpoint, rh.ConnectionID)
		return g.connectFailed(w, "internal", "quackmux: "+err.Error(), wire.ExceptionConnection)
	}
	// The client gets the gateway id and the lease the gateway enforces.
	cr.HeartbeatTimeoutSeconds = uint64(hb / time.Second)
	writeBody(w, cr.Encode(wire.Header{Type: wire.TypeConnectionResponse, ConnectionID: gatewayID, ClientQueryID: rh.ClientQueryID}))
	return "ok"
}

func (g *Gateway) disconnectBackend(endpoint, backendConnID string) {
	go func() {
		if err := g.up.Disconnect(context.Background(), endpoint, backendConnID); err != nil {
			g.log.Debug("cleanup disconnect failed", "err", err)
		}
	}()
}

func clampHeartbeat(seconds uint64, min, max time.Duration) time.Duration {
	if seconds > uint64(max/time.Second) {
		return max.Truncate(time.Second)
	}
	d := time.Duration(seconds) * time.Second
	if d < min {
		return min.Truncate(time.Second)
	}
	return d
}

func closedMessage(c *session.Closed) string {
	if c.Transient {
		return "quackmux: " + c.Reason
	}
	return fmt.Sprintf("quackmux: session closed: %s. DETACH and ATTACH again to reconnect.", c.Reason)
}

func (g *Gateway) handleRouted(w http.ResponseWriter, r *http.Request, br *bufio.Reader, h wire.Header, hlen int) string {
	sess, closed, err := g.sessions.Resolve(h.ConnectionID)
	if err != nil {
		// Same text as the stock server for unknown connections.
		writeError(w, "Invalid connection id", wire.ExceptionInvalidInput, false)
		return "invalid_connection"
	}
	if closed != nil {
		writeError(w, closedMessage(closed), wire.ExceptionConnection, closed.MustInvalidate)
		return "session_closed"
	}
	target, ok := g.engines.Lookup(sess.EngineID)
	if !ok {
		g.sessions.Close(sess.ID, "engine no longer available", session.CloseMode{Force: true, MustInvalidate: true})
		writeError(w, closedMessage(&session.Closed{Reason: "engine no longer available"}), wire.ExceptionConnection, true)
		return "engine_gone"
	}
	if c, ok := sess.Begin(g.now(), h.Type); !ok {
		writeError(w, closedMessage(c), wire.ExceptionConnection, c.MustInvalidate)
		return "session_closed"
	}
	defer func() { sess.End(g.now(), h.Type) }()
	liftReadDeadline(w) // a valid session: its uploads may take long

	switch h.Type {
	case wire.TypeHeartbeatRequest:
		// Answered by the gateway, which owns the client-facing lease (Begin
		// renewed it) and keeps the engine lease alive on its own. A hung
		// engine can therefore never stall the client's heartbeat thread,
		// which would block its DETACH.
		writeBody(w, wire.EncodeEmpty(wire.Header{Type: wire.TypeSuccessResponse}))
		return "ok_local"
	case wire.TypeDisconnect:
		// Acknowledge at once (the client waits only 2s) and close the engine
		// connection in the background.
		g.sessions.Close(sess.ID, "client disconnect", session.CloseMode{Disconnect: true, Force: true})
		writeBody(w, wire.EncodeEmpty(wire.Header{Type: wire.TypeSuccessResponse}))
		return "ok_local"
	}

	g.engines.BeginRequest(target.ID)
	defer g.engines.EndRequest(target.ID)

	if h.Type == wire.TypePrepareRequest && g.audit != nil {
		g.auditQuery(br, hlen, h, sess, target)
	}

	newHeader := wire.Header{Type: h.Type, ConnectionID: sess.BackendConnID, ClientQueryID: h.ClientQueryID}.Encode()
	uc := g.cfg.Server.Upstream
	if uc.MaxRequestBytes > 0 && r.ContentLength > uc.MaxRequestBytes {
		writeError(w, fmt.Sprintf("quackmux: message of %d bytes exceeds the limit of %d bytes", r.ContentLength, uc.MaxRequestBytes),
			wire.ExceptionInvalidInput, false)
		return "too_large"
	}
	if _, err := br.Discard(hlen); err != nil {
		writeError(w, "Failed to deserialize: "+err.Error(), wire.ExceptionInvalidInput, false)
		return "malformed"
	}
	contentLength := int64(-1)
	if r.ContentLength >= 0 {
		contentLength = r.ContentLength - int64(hlen) + int64(len(newHeader))
	}
	// The transport may still read the body after Do has returned (it probes
	// for bytes beyond the content length in its own goroutine), so the body
	// must never reach past the client's message and the byte count is
	// atomic.
	var in io.Reader
	var sent func() int64
	if contentLength >= 0 && uc.MaxBufferedRequestBytes > 0 && contentLength <= uc.MaxBufferedRequestBytes && g.buffers.TryAcquire(contentLength) {
		// Read the whole message before talking to the engine, so that a slow
		// client cannot stall the engine's socket (it gives up after 5s).
		buf := make([]byte, contentLength)
		copy(buf, newHeader)
		_, err := io.ReadFull(br, buf[len(newHeader):])
		if err != nil {
			g.buffers.Release(contentLength)
			return "client_error"
		}
		// The budget is returned as soon as the transport has sent the body
		// (it closes it then), not when the engine answers: an upload can
		// wait long for the engine under backpressure.
		release := sync.OnceFunc(func() { g.buffers.Release(contentLength) })
		defer release()
		in = &releasingReader{Reader: bytes.NewReader(buf), release: release}
		sent = func() int64 { return contentLength }
	} else {
		var rest io.Reader = br
		if r.ContentLength >= 0 {
			rest = io.LimitReader(br, r.ContentLength-int64(hlen))
		}
		cr := &countingReader{r: io.MultiReader(bytes.NewReader(newHeader), rest)}
		in, sent = cr, cr.n.Load
	}
	upCtx := r.Context()
	stopAbort := func() bool { return true }
	if h.Type == wire.TypePrepareRequest {
		// A query waiting for its engine when the gateway shuts down is
		// answered with an error (see AbortQueries), never cut off.
		var cancelUp context.CancelFunc
		upCtx, cancelUp = context.WithCancel(upCtx)
		defer cancelUp()
		stopAbort = context.AfterFunc(g.abortCtx, cancelUp)
	}
	upReq, err := upstream.NewRequest(upCtx, target.Endpoint, in, contentLength)
	if err != nil {
		writeError(w, "quackmux: "+err.Error(), wire.ExceptionConnection, false)
		return "internal"
	}
	sentAt := g.now()
	resp, err := g.up.HTTP.Do(upReq)
	aborted := !stopAbort() // the answer (or the failure) is in: a shutdown no longer cuts it off
	if g.met != nil {
		g.met.Bytes.WithLabelValues("in").Add(float64(sent()))
	}
	if err != nil {
		if r.Context().Err() != nil {
			return "client_gone"
		}
		if aborted {
			cctx, cancel := context.WithTimeout(context.Background(), abortCancelTimeout)
			if cerr := g.up.CancelAll(cctx, target.Endpoint, sess.BackendConnID); cerr != nil {
				g.log.Warn("cancelling a query at shutdown failed", "engine", target.ID, "session", sess.ID, "err", cerr)
			}
			cancel()
			writeError(w, "quackmux: the gateway is shutting down; the query was cancelled and may not have run. Run it again.",
				wire.ExceptionConnection, false)
			return "shutdown"
		}
		g.log.Warn("engine request failed", "engine", target.ID, "session", sess.ID, "type", h.Type, "err", err)
		msg := fmt.Sprintf("quackmux: engine %s is unreachable: %v", target.ID, err)
		if _, still := g.engines.Lookup(target.ID); !still {
			writeError(w, msg, wire.ExceptionConnection, true)
		} else {
			writeError(w, msg, wire.ExceptionConnection, false)
		}
		return "engine_unreachable"
	}
	defer resp.Body.Close()
	// The engine answered, so it received the message - and renewed its
	// lease - at or after sentAt (not when the answer came back).
	sess.TouchBackend(sentAt)
	if resp.StatusCode != http.StatusOK {
		writeError(w, fmt.Sprintf("quackmux: engine %s answered HTTP %d", target.ID, resp.StatusCode), wire.ExceptionConnection, false)
		return "engine_status"
	}

	rb := bufio.NewReaderSize(resp.Body, 32<<10)
	respType := wire.TypeInvalid
	if p, _ := rb.Peek(3); len(p) == 3 && p[0] == 0x01 && p[1] == 0x00 && p[2] < 0x80 {
		respType = wire.MessageType(p[2])
	}
	if n := resp.ContentLength; respType == wire.TypeErrorResponse && n >= 0 && n <= maxInspectedErrorBytes && g.buffers.TryAcquire(n) {
		// Small errors are read and looked at; larger ones (a huge message
		// from error()) pass through like any other response, whole.
		body := make([]byte, n)
		_, err := io.ReadFull(rb, body)
		resp.Body.Close()
		if err != nil {
			g.buffers.Release(n)
			// Nothing was sent yet: answer instead of dropping the connection
			// (a dropped connection can make libcurl resend the request).
			writeError(w, fmt.Sprintf("quackmux: engine %s broke off its response: %v", target.ID, err), wire.ExceptionConnection, false)
			return "engine_broken"
		}
		if e, derr := wire.DecodeErrorResponse(body); derr == nil {
			switch {
			case e.MustInvalidate:
				g.engines.MarkFailed(target.ID, "engine database invalidated: "+e.Message)
			case upstream.IsSessionGone(e.Message):
				g.sessions.ClosedByBackend(sess, e.Message)
			}
		}
		writeBody(w, body)
		g.buffers.Release(n)
		return "engine_error"
	}

	if n := resp.ContentLength; n >= 0 && uc.MaxBufferedResponseBytes > 0 && n <= uc.MaxBufferedResponseBytes && g.buffers.TryAcquire(n) {
		// Drain the engine at full speed, then serve the client from memory.
		buf := make([]byte, n)
		_, err := io.ReadFull(rb, buf)
		resp.Body.Close()
		if err != nil {
			g.buffers.Release(n)
			g.log.Warn("engine response broken", "engine", target.ID, "session", sess.ID, "type", h.Type, "err", err)
			writeError(w, fmt.Sprintf("quackmux: engine %s broke off its response: %v", target.ID, err), wire.ExceptionConnection, false)
			return "engine_broken"
		}
		writeBody(w, buf)
		g.buffers.Release(n)
		if g.met != nil {
			g.met.Bytes.WithLabelValues("out").Add(float64(n))
		}
		return "ok"
	}

	w.Header().Set("Content-Type", wire.ContentType)
	if resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	w.WriteHeader(http.StatusOK)
	n, err := rb.WriteTo(w)
	if g.met != nil {
		g.met.Bytes.WithLabelValues("out").Add(float64(n))
	}
	if err != nil {
		if r.Context().Err() == nil {
			g.log.Warn("response stream broken", "engine", target.ID, "session", sess.ID, "type", h.Type, "bytes", n, "err", err)
		}
		// Never let the client see a cut-off body as a complete message.
		panic(http.ErrAbortHandler)
	}
	return "ok"
}

func (g *Gateway) auditQuery(br *bufio.Reader, hlen int, h wire.Header, sess *session.Session, target engine.Target) {
	max := g.cfg.Audit.MaxQueryBytes
	want := hlen + max + 16
	if want > maxHeaderBytes {
		want = maxHeaderBytes
	}
	b, _ := br.Peek(want)
	if len(b) < hlen {
		return
	}
	sql, truncated, err := wire.PeekPrepareSQL(b[hlen:], max)
	if err != nil {
		return
	}
	attrs := []any{"user", sess.User, "session", sess.ID, "engine", target.ID, "key", target.Key.String()}
	if mayHoldCredentials(sql) {
		attrs = append(attrs, "sql", redactedSQL, "redacted", true)
	} else {
		attrs = append(attrs, "sql", strings.ToValidUTF8(sql, "?"), "truncated", truncated)
	}
	if h.ClientQueryID.Valid {
		attrs = append(attrs, "clientQueryId", h.ClientQueryID.Value)
	}
	g.audit.Info("query", attrs...)
}

// redactedSQL replaces, in the audit log, the text of a statement that may
// hold credentials.
const redactedSQL = "[redacted: the statement may contain credentials]"

// credentialPattern finds SQL that may hold credentials: CREATE SECRET and
// every other statement that names a secret, password or token (also inside
// identifiers such as s3_secret_access_key or motherduck_token), Azure
// account keys and SAS signatures, and URLs with a user and password (also
// when the logged text ends inside one). It errs on the side of redacting.
var credentialPattern = regexp.MustCompile(`(?i)secret|passw|\bpwd\b|token|accountkey|[?&;]sig=|://[^/\s'"@]*:[^/\s'"@]*(@|$)`)

func mayHoldCredentials(sql string) bool {
	return credentialPattern.MatchString(sql)
}

// releasingReader is a request body that runs release once the transport
// closes it (after sending it, or on failure).
type releasingReader struct {
	*bytes.Reader
	release func()
}

func (r *releasingReader) Close() error {
	r.release()
	return nil
}

// countingReader counts the bytes read through it. The count is atomic: the
// HTTP transport reads in its own goroutine.
type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}
