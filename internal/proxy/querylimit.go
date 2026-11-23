package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"quackmux/internal/policy"
	"quackmux/internal/session"
	"quackmux/internal/upstream"
	"quackmux/internal/wire"
)

// The query time limit (limits.queryTimeout, profiles.<name>.queryTimeout,
// overrides[].queryTimeout) cancels a query on its engine once it has run
// longer than the limit; the client gets an ERROR_RESPONSE, and the session
// stays usable. It is a policy the operator opts into, not a transport
// timeout: without a limit the gateway does not follow queries at all.
//
// A query runs from the moment the gateway forwards its PREPARE_REQUEST
// until it is over. Time the client spends between FETCHes or uploads
// counts: the query holds its engine connection, transaction and result
// buffer until then. The gateway follows the queries of a session by their
// uuid and takes only the engine's word for their end, so that a client
// cannot stop the clock of a query that still runs. A query is over when
//
//   - its PREPARE_REQUEST is answered with an error;
//   - a later PREPARE_REQUEST of the session is answered with a result: the
//     engine replaced the queries whose PREPAREs no longer waited;
//   - a FETCH of the query gets the terminal FETCH_RESPONSE (the result is
//     complete) or, for a FETCH the engine certainly decoded, an error;
//   - a CANCEL of the client for it (or for whatever runs) succeeds;
//   - the session closes.
//
// A PREPARE_RESPONSE that carries the whole result looks no different to the
// gateway than one with more to fetch: needs_more_fetch follows the column
// types, which the gateway does not decode. An answered query therefore
// rests: the limit does not cancel it while the client does nothing for it,
// and cancels it as soon as the client fetches or uploads for it again.
//
// At the limit the gateway sends CANCEL with the query's uuid. The engine
// answers the requests that wait for the query with an INTERRUPT error, which
// the gateway replaces by its own message; if the engine does not confirm the
// cancel, the gateway answers them itself. Later FETCHes and uploads of the
// query get the same error from the gateway. A PREPARE of the session waits
// until the CANCEL is answered: the engine could otherwise apply it to the
// next query.

// msgCancelOther starts the engine's answer to a CANCEL for a query that is
// not the statement of the connection (quack_server.cpp).
const msgCancelOther = "Attempted to cancel a different query"

// maxQueryHeadBytes covers the uuid and the batch indices that a
// FETCH_REQUEST or CANCEL_REQUEST body starts with.
const maxQueryHeadBytes = 64

// The engine refuses a CANCEL for a query whose PREPARE it has not made the
// connection's statement yet; the gateway tries again this often.
const (
	cancelRetries    = 25
	cancelRetryDelay = 200 * time.Millisecond
)

// errShuttingDown reports a PREPARE that waited for a CANCEL when the gateway
// began to shut down.
var errShuttingDown = errors.New("the gateway is shutting down")

// queryLimits follows the queries of the sessions that have a time limit.
type queryLimits struct {
	g *Gateway

	mu       sync.Mutex
	sessions map[string]*sessionQueries
}

// sessionQueries are the followed queries of one session. All fields of it
// and of its queries and requests are guarded by queryLimits.mu.
type sessionQueries struct {
	sess    *session.Session
	queries []*trackedQuery // in the order the gateway forwarded them
	// cancels counts the CANCELs the gateway has in flight for the session;
	// cancelsDone is closed when they are all answered.
	cancels     int
	cancelsDone chan struct{}
}

// trackedQuery is one query of a session.
type trackedQuery struct {
	sq            *sessionQueries
	uuid          wire.Hugeint
	uuidKnown     bool
	clientQueryID wire.OptionalIdx
	limit         time.Duration
	source        string // the setting the limit comes from
	started       time.Time
	timer         *time.Timer

	preparing bool // its PREPARE_REQUEST waits for the engine
	reading   bool // the client fetched or uploaded for it: it is not complete
	expired   bool // the limit passed while it rested
	timedOut  bool // the gateway cancels or cancelled it at its limit
	ended     bool
	waiting   map[*queryRequest]struct{}
}

// queryRequest is a forwarded request the query time limit follows.
type queryRequest struct {
	l    *queryLimits
	sess *session.Session
	kind wire.MessageType
	q    *trackedQuery // nil for CANCEL_REQUEST
	// uuid is the query a CANCEL_REQUEST names (zero: whatever runs).
	uuid wire.Hugeint
	// decoded marks a FETCH_REQUEST with a batch index that the engine
	// certainly decodes: an error in answer to it ends the query.
	decoded bool
	abort   context.CancelFunc // cancels the request to the engine
	aborted bool               // the limit cancelled the request to the engine
}

// requestEvent is how the engine answered a followed request.
type requestEvent uint8

const (
	evFailed   requestEvent = iota // no answer from the engine
	evAnswered                     // an answer that does not end the query
	evResult                       // PREPARE_RESPONSE
	evComplete                     // the terminal FETCH_RESPONSE
	evError                        // ERROR_RESPONSE
)

func newQueryLimits(g *Gateway) *queryLimits {
	return &queryLimits{g: g, sessions: map[string]*sessionQueries{}}
}

// limitFor resolves the time limit of the queries of s (0: none) and the
// setting it comes from.
func (l *queryLimits) limitFor(s *session.Session) (time.Duration, string) {
	return policy.QueryTimeout(l.g.cfg, s.User, s.Groups, s.EngineKey.Profile)
}

// sessionLocked returns the state of s; with create it makes one for an open
// session. A session that closed has none: its close hook dropped it.
func (l *queryLimits) sessionLocked(s *session.Session, create bool) *sessionQueries {
	sq := l.sessions[s.ID]
	if sq != nil && sq.sess != s {
		l.dropLocked(sq) // left over from an earlier object of the session
		sq = nil
	}
	if sq == nil && create {
		if _, open := l.g.sessions.Get(s.ID); !open {
			return nil
		}
		sq = &sessionQueries{sess: s}
		l.sessions[s.ID] = sq
	}
	return sq
}

// newQueryLocked starts following a query of sq.
func (l *queryLimits) newQueryLocked(sq *sessionQueries, uuid wire.Hugeint, known bool, clientQueryID wire.OptionalIdx,
	limit time.Duration, source string) *trackedQuery {
	q := &trackedQuery{sq: sq, uuid: uuid, uuidKnown: known, clientQueryID: clientQueryID, limit: limit,
		source: source, started: l.g.now(), waiting: map[*queryRequest]struct{}{}}
	q.timer = time.AfterFunc(limit, func() { l.expire(q) })
	sq.queries = append(sq.queries, q)
	return q
}

// endLocked stops following q.
func (l *queryLimits) endLocked(q *trackedQuery) {
	if q.ended {
		return
	}
	q.ended = true
	q.timer.Stop()
	sq := q.sq
	for i, o := range sq.queries {
		if o == q {
			sq.queries = append(sq.queries[:i], sq.queries[i+1:]...)
			break
		}
	}
	l.forgetIdleLocked(sq)
}

// forgetIdleLocked drops the state of a session that has nothing left to
// follow.
func (l *queryLimits) forgetIdleLocked(sq *sessionQueries) {
	if len(sq.queries) == 0 && sq.cancels == 0 && l.sessions[sq.sess.ID] == sq {
		delete(l.sessions, sq.sess.ID)
	}
}

// dropLocked forgets a session and its queries.
func (l *queryLimits) dropLocked(sq *sessionQueries) {
	for _, q := range sq.queries {
		q.ended = true
		q.timer.Stop()
	}
	sq.queries = nil
	if l.sessions[sq.sess.ID] == sq {
		delete(l.sessions, sq.sess.ID)
	}
}

// endOthersLocked ends the queries of sq other than keep for which match
// holds.
func (l *queryLimits) endOthersLocked(sq *sessionQueries, keep *trackedQuery, match func(*trackedQuery) bool) {
	for _, o := range append([]*trackedQuery(nil), sq.queries...) {
		if o != keep && match(o) {
			l.endLocked(o)
		}
	}
}

// sessionClosed is the session manager's close hook.
func (l *queryLimits) sessionClosed(s *session.Session) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if sq := l.sessions[s.ID]; sq != nil && sq.sess == s {
		l.dropLocked(sq)
	}
}

// track starts following a request of sess right before the gateway
// forwards it. body is the body after the header if the gateway buffered
// it, head the start of a FETCH or CANCEL body, scan watches a PREPARE body
// that streams; abort cancels the request to the engine. The result is nil
// if the limit does not follow the request. local is set if the gateway must
// answer with that query's timeout error instead of forwarding the request.
// err reports that ctx ended, or that the gateway shut down, while a PREPARE
// waited for the CANCEL of an earlier query.
func (l *queryLimits) track(ctx context.Context, h wire.Header, sess *session.Session, body, head []byte,
	scan *prepareUUIDSink, abort context.CancelFunc) (r *queryRequest, local *trackedQuery, err error) {
	if body != nil {
		head = body
	}
	switch h.Type {
	case wire.TypePrepareRequest:
		var uuid wire.Hugeint
		known := false
		if body != nil {
			var err error
			if uuid, err = wire.PrepareQueryUUID(body); err != nil {
				return nil, nil, nil // the engine cannot decode it either: no query starts
			}
			known = true
		}
		r, err := l.beginPrepare(ctx, sess, h.ClientQueryID, uuid, known, abort)
		if r != nil && scan != nil {
			scan.found = r.setUUID
		}
		return r, nil, err
	case wire.TypeFetchRequest:
		uuid, err := wire.LeadingQueryUUID(head)
		if err != nil {
			return nil, nil, nil // the engine answers that it cannot decode it
		}
		f, ferr := wire.DecodeFetchRequestBody(head)
		r, local := l.beginReading(sess, &uuid, ferr == nil && f.BatchIndex > 0, abort)
		return r, local, nil
	case wire.TypeSendDataRequest:
		r, local := l.beginReading(sess, nil, false, abort)
		return r, local, nil
	case wire.TypeCancelRequest:
		uuid, err := wire.LeadingQueryUUID(head)
		if err != nil {
			return nil, nil, nil
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.sessionLocked(sess, false) == nil {
			return nil, nil, nil
		}
		return &queryRequest{l: l, sess: sess, kind: h.Type, uuid: uuid}, nil, nil
	}
	return nil, nil, nil
}

// beginPrepare follows a new query. It first waits for a CANCEL the gateway
// sent for an earlier query of the session.
func (l *queryLimits) beginPrepare(ctx context.Context, sess *session.Session, clientQueryID wire.OptionalIdx,
	uuid wire.Hugeint, known bool, abort context.CancelFunc) (*queryRequest, error) {
	limit, source := l.limitFor(sess)
	if limit <= 0 {
		return nil, nil
	}
	for {
		l.mu.Lock()
		sq := l.sessionLocked(sess, true)
		if sq == nil {
			l.mu.Unlock()
			return nil, nil
		}
		if done := sq.cancelsDone; done != nil {
			l.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-l.g.abortCtx.Done():
				return nil, errShuttingDown
			}
		}
		// Add the query first: ending the others must not leave the
		// session without queries (forgetIdleLocked).
		q := l.newQueryLocked(sq, uuid, known, clientQueryID, limit, source)
		q.preparing = true
		if known {
			l.endSameUUIDLocked(sq, q, uuid)
		}
		r := &queryRequest{l: l, sess: sess, kind: wire.TypePrepareRequest, q: q, abort: abort}
		q.waiting[r] = struct{}{}
		l.mu.Unlock()
		return r, nil
	}
}

// endSameUUIDLocked ends the queries of sq other than keep that have uuid: a
// new PREPARE with it replaces them, and a CANCEL could not tell them apart.
func (l *queryLimits) endSameUUIDLocked(sq *sessionQueries, keep *trackedQuery, uuid wire.Hugeint) {
	l.endOthersLocked(sq, keep, func(o *trackedQuery) bool { return o.uuidKnown && o.uuid == uuid })
}

// beginReading follows a FETCH (of the query with *uuid) or an upload (of the
// newest query: uuid is nil). A FETCH for a query the gateway does not know,
// such as one that began before a gateway restart, starts following it, with
// the limit counted from now. decoded marks a FETCH the engine certainly
// decodes.
func (l *queryLimits) beginReading(sess *session.Session, uuid *wire.Hugeint, decoded bool,
	abort context.CancelFunc) (*queryRequest, *trackedQuery) {
	l.mu.Lock()
	sq := l.sessionLocked(sess, false)
	var q *trackedQuery
	if sq != nil {
		for i := len(sq.queries) - 1; i >= 0; i-- {
			if o := sq.queries[i]; uuid == nil || (o.uuidKnown && o.uuid == *uuid) {
				q = o
				break
			}
		}
	}
	if q == nil {
		if uuid == nil {
			l.mu.Unlock()
			return nil, nil
		}
		limit, source := l.limitFor(sess)
		if limit <= 0 {
			l.mu.Unlock()
			return nil, nil
		}
		if sq = l.sessionLocked(sess, true); sq == nil {
			l.mu.Unlock()
			return nil, nil
		}
		q = l.newQueryLocked(sq, *uuid, true, wire.OptionalIdx{}, limit, source)
	}
	if q.timedOut {
		l.mu.Unlock()
		return nil, q
	}
	q.reading = true
	if q.expired {
		// The limit passed while the query rested: the client is back for
		// it, so it is not complete.
		l.mu.Unlock()
		l.enforce(q)
		l.mu.Lock()
		if q.timedOut {
			l.mu.Unlock()
			return nil, q
		}
		if q.ended {
			// The engine runs another statement: the query was over.
			l.mu.Unlock()
			return nil, nil
		}
	}
	r := &queryRequest{l: l, sess: sess, kind: wire.TypeFetchRequest, q: q, decoded: decoded, abort: abort}
	if uuid == nil {
		r.kind = wire.TypeSendDataRequest
	}
	q.waiting[r] = struct{}{}
	l.mu.Unlock()
	return r, nil
}

// setUUID records the uuid of a PREPARE whose body streamed past the
// scanner.
func (r *queryRequest) setUUID(uuid wire.Hugeint) {
	l := r.l
	l.mu.Lock()
	defer l.mu.Unlock()
	q := r.q
	if q.ended || q.uuidKnown {
		return
	}
	q.uuid, q.uuidKnown = uuid, true
	l.endSameUUIDLocked(q.sq, q, uuid)
}

// timeoutError returns the error the gateway answers with instead of the
// engine's answer to r: the query of r ran out of time.
func (r *queryRequest) timeoutError() (string, bool) {
	if r == nil || r.q == nil {
		return "", false
	}
	r.l.mu.Lock()
	defer r.l.mu.Unlock()
	if !r.q.timedOut {
		return "", false
	}
	return r.q.message(), true
}

// finish ends a followed request with what the engine answered.
func (r *queryRequest) finish(ev requestEvent) {
	if r == nil {
		return
	}
	l := r.l
	if r.q == nil {
		// A CANCEL of the client: once it succeeded, the queries it named
		// are over (the engine ended the statement it found).
		if ev != evAnswered {
			return
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		if sq := l.sessions[r.sess.ID]; sq != nil && sq.sess == r.sess {
			l.endOthersLocked(sq, nil, func(o *trackedQuery) bool {
				if r.uuid.IsZero() {
					return !o.preparing // a PREPARE still on its way may come after the CANCEL
				}
				return o.uuidKnown && o.uuid == r.uuid
			})
		}
		return
	}
	q := r.q
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(q.waiting, r)
	if !q.ended {
		switch r.kind {
		case wire.TypePrepareRequest:
			q.preparing = false
			switch ev {
			case evResult:
				// The engine replaced the statement before this one: every
				// query whose PREPARE no longer waits is over. (One that
				// still waits may reach the engine after this one.)
				l.endOthersLocked(q.sq, q, func(o *trackedQuery) bool { return !o.preparing })
			case evError:
				// Also a query that timed out: its client got the error,
				// and there is no result to fetch.
				l.endLocked(q)
			case evFailed:
				// No answer: the engine may still run the query (the client
				// went away), so the limit keeps following it.
				q.reading = true
			}
		case wire.TypeFetchRequest:
			switch {
			case ev == evComplete:
				l.endLocked(q)
			case ev == evError && r.decoded && !q.timedOut:
				l.endLocked(q)
			}
		}
	}
}

// expire runs when the limit of q has passed.
func (l *queryLimits) expire(q *trackedQuery) {
	l.mu.Lock()
	if q.ended || q.timedOut {
		l.mu.Unlock()
		return
	}
	if !q.preparing && !q.reading {
		// Answered and resting: it may well be complete. The limit is
		// enforced when the client comes back for it (beginReading).
		q.expired = true
		l.mu.Unlock()
		return
	}
	l.mu.Unlock()
	l.enforce(q)
}

// enforce cancels q on its engine and makes the requests of q fail with the
// timeout error.
func (l *queryLimits) enforce(q *trackedQuery) {
	l.mu.Lock()
	if q.ended || q.timedOut {
		l.mu.Unlock()
		return
	}
	q.timedOut, q.expired = true, false
	sess := q.sq.sess
	elapsed := l.g.now().Sub(q.started)
	l.mu.Unlock()

	result, cancelErr := l.cancelOnEngine(q)
	if result == "" {
		// The engine runs another statement on the connection, so the query
		// was over (superseded) and nothing waits for it.
		l.g.log.Debug("query time limit reached by a query that was already over", "session", sess.ID,
			"queryUuid", q.uuid.String())
		return
	}
	if result == "failed" {
		// The client gets its error now, whatever the engine does.
		l.abortWaiting(q)
	}
	attrs := []any{"user", sess.User, "session", sess.ID, "engine", sess.EngineID, "key", sess.EngineKey.String(),
		"queryUuid", q.uuid.String(), "limit", q.limit.String(), "source", q.source,
		"elapsed", elapsed.Round(time.Millisecond).String(), "cancel", result}
	if q.clientQueryID.Valid {
		attrs = append(attrs, "clientQueryId", q.clientQueryID.Value)
	}
	if cancelErr != nil {
		attrs = append(attrs, "err", cancelErr)
	}
	l.g.log.Warn("query cancelled at its time limit", attrs...)
	if l.g.audit != nil {
		l.g.audit.Info("query timeout", attrs...)
	}
	if l.g.met != nil {
		l.g.met.QueryTimeouts.WithLabelValues(sess.EngineKey.Profile, result).Inc()
	}
}

// cancelOnEngine sends CANCEL for a query that timed out. It returns
// "cancelled" if the engine confirmed it or the query had not reached the
// engine, "failed" if the gateway could not cancel it, and "" if the engine
// runs another statement on the connection: then q was over, and it ends.
func (l *queryLimits) cancelOnEngine(q *trackedQuery) (string, error) {
	l.mu.Lock()
	sq := q.sq
	if !q.uuidKnown {
		// The PREPARE body has not passed the uuid yet, so the engine has
		// not got the query: stopping the request is enough.
		l.mu.Unlock()
		l.abortWaiting(q)
		return "cancelled", nil
	}
	if q.uuid.IsZero() && sq.queries[len(sq.queries)-1] != q {
		// CANCEL with the zero uuid ends whatever runs, and the session has
		// sent a newer query since.
		l.mu.Unlock()
		return "failed", errors.New("the query has the zero uuid, and its session sent a newer query")
	}
	l.mu.Unlock()
	for attempt := 1; ; attempt++ {
		err := l.sendCancel(q)
		var ee *upstream.EngineError
		if err == nil {
			return "cancelled", nil
		}
		if !errors.As(err, &ee) || !strings.HasPrefix(ee.Response.Message, msgCancelOther) {
			return "failed", err
		}
		l.mu.Lock()
		preparing := q.preparing && !q.ended
		if !preparing {
			q.timedOut = false
			l.endLocked(q)
		}
		l.mu.Unlock()
		if !preparing {
			return "", nil
		}
		// The engine has not made the PREPARE its statement yet (it still
		// reads or authorizes it).
		if attempt == cancelRetries {
			return "failed", err
		}
		time.Sleep(cancelRetryDelay)
	}
}

// sendCancel sends CANCEL for q to its engine. PREPAREs of the session wait
// until it is answered.
func (l *queryLimits) sendCancel(q *trackedQuery) error {
	l.mu.Lock()
	sq := q.sq
	if sq.cancels == 0 {
		sq.cancelsDone = make(chan struct{})
	}
	sq.cancels++
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		if sq.cancels--; sq.cancels == 0 {
			close(sq.cancelsDone)
			sq.cancelsDone = nil
			l.forgetIdleLocked(sq)
		}
		l.mu.Unlock()
	}()
	target, ok := l.g.engines.Lookup(sq.sess.EngineID)
	if !ok {
		return fmt.Errorf("engine %s is not available", sq.sess.EngineID)
	}
	return l.g.up.Control(context.Background(), target.Endpoint, wire.EncodeCancel(sq.sess.BackendConnID, q.uuid))
}

// abortWaiting cancels the requests of q that wait for the engine; their
// handlers answer with the timeout error.
func (l *queryLimits) abortWaiting(q *trackedQuery) {
	l.mu.Lock()
	var aborts []context.CancelFunc
	for r := range q.waiting {
		r.aborted = true
		aborts = append(aborts, r.abort)
	}
	l.mu.Unlock()
	for _, abort := range aborts {
		abort()
	}
}

// message is the error a client gets for a query that timed out.
func (q *trackedQuery) message() string {
	return fmt.Sprintf("quackmux: the query ran longer than its time limit of %s (%s) and was cancelled", q.limit, q.source)
}

// prepareUUIDSink watches the body of a PREPARE_REQUEST that streams to the
// engine and reports its uuid once it has passed.
type prepareUUIDSink struct {
	scanner  wire.PrepareUUIDScanner
	reported bool
	found    func(wire.Hugeint)
}

func (s *prepareUUIDSink) Write(p []byte) (int, error) {
	n, _ := s.scanner.Write(p)
	if !s.reported {
		if u, ok := s.scanner.UUID(); ok {
			s.reported = true
			if s.found != nil {
				s.found(u)
			}
		}
	}
	return n, nil
}

// peekQueryHead returns the start of a FETCH or CANCEL body without consuming
// it; length is the body length (negative: unknown).
func peekQueryHead(br *bufio.Reader, length int64) []byte {
	n := maxQueryHeadBytes
	if length >= 0 && length < int64(n) {
		n = int(length)
	}
	b, _ := br.Peek(n)
	return b
}
