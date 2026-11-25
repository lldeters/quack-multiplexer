package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"quackmux/internal/auth"
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
// counts. The gateway follows the queries of a session by their uuid and
// takes only the engine's word for their end, so that a client cannot stop
// the clock of a query that still runs. A query is over when
//
//   - its PREPARE_REQUEST is answered with an error;
//   - a PREPARE_REQUEST of the session that the gateway forwarded after the
//     query's PREPARE got its result is answered with a result: the engine
//     replaced the query;
//   - a FETCH of the query that the gateway forwarded after its PREPARE was
//     answered gets the terminal FETCH_RESPONSE (the result is complete) or,
//     for a FETCH the engine certainly decoded, an error;
//   - a CANCEL of the client for it (or for whatever runs) succeeds that the
//     gateway forwarded after its PREPARE was answered;
//   - the session closes.
//
// Answers to requests that the gateway forwarded while the PREPARE still
// waited prove nothing: the engine may not have made the query its
// statement yet, and they may be about the statement before.
//
// The gateway cannot see whether a client still wants a query that it does
// not ask anything for: an answered PREPARE may carry the whole result
// (needs_more_fetch follows the column types, which the gateway does not
// decode), an upload is complete once its row count is fetched, and a scan
// with LIMIT stops reading early. The limit is therefore enforced when it
// passes while the engine works for the query (a request of it waits for
// the engine, or its PREPARE went unanswered after the engine got all of
// it), and otherwise at the next request of the query. Queries that are
// complete or that the client left alone never get a CANCEL; the next
// PREPARE of the session replaces them on the engine.
//
// At the limit the gateway sends CANCEL with the query's uuid. The engine
// answers the requests that wait for the query with an INTERRUPT error,
// which the gateway replaces by its own message; if the engine does not
// confirm the cancel, the gateway answers them itself and says so. Later
// FETCHes and uploads of the query get the same error from the gateway. A
// PREPARE whose body the engine has not got completely is cut off instead:
// the engine cannot run it then. The next PREPAREs of the session wait until
// the gateway is done with the query: a CANCEL could otherwise meet them.
// COMMIT and ROLLBACK have no limit: DuckDB does not interrupt a commit, and
// the engine would report it as interrupted although it committed.

// msgCancelOther starts the engine's answer to a CANCEL for a query that is
// not the statement of the connection (quack_server.cpp).
const msgCancelOther = "Attempted to cancel a different query"

// maxQueryHeadBytes covers the uuid and the batch indices that a
// FETCH_REQUEST or CANCEL_REQUEST body starts with, and the SQL text of a
// PREPARE_REQUEST that ends a transaction.
const maxQueryHeadBytes = 64

// The engine refuses a CANCEL for a query whose PREPARE it has not made the
// connection's statement yet, and a CANCEL with the zero uuid (or the uuid
// of the statement before) meets the statement before it then: the gateway
// cancels again, at most this often, until the PREPARE is answered.
const (
	cancelRetries    = 25
	cancelRetryDelay = 200 * time.Millisecond
)

// maxSessionQueries bounds the queries the gateway follows per session. A
// DuckDB client has one query at a time on a connection; more are open only
// for clients that abandon or race their requests. Beyond the bound the
// gateway forgets the oldest query whose PREPARE was answered: the engine
// has run so many PREPAREs of the session since that it replaced it.
const maxSessionQueries = 64

// fenceRetry is how soon the gateway looks again at a limit that passed
// while it did not hold a fresh lease (see Options.Fence).
const fenceRetry = time.Second

// errShuttingDown reports a PREPARE that waited for a CANCEL when the gateway
// began to shut down.
var errShuttingDown = errors.New("the gateway is shutting down")

// errNotFresh reports a CANCEL the gateway did not send because it does not
// hold a fresh lease: another replica may serve the session by now.
var errNotFresh = errors.New("this gateway does not hold a fresh lease")

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
	queries []*trackedQuery // in the order the gateway began following them
	// prepares counts the PREPAREs of the session the gateway forwarded.
	prepares uint64
	// cancels counts the enforcements in progress for the session;
	// cancelsDone is closed when they are all done. PREPAREs wait for it.
	cancels     int
	cancelsDone chan struct{}
	// adopting is set while the engine may still run a query of the session
	// that began before this process (the session was adopted): a FETCH for
	// a uuid the gateway does not know then starts following that query. It
	// ends once the engine has shown what it runs.
	adopting bool
}

// trackedQuery is one query of a session.
type trackedQuery struct {
	sq            *sessionQueries
	uuid          wire.Hugeint
	uuidKnown     bool
	clientQueryID wire.OptionalIdx
	limit         time.Duration // 0: none (the statement ends a transaction)
	source        string        // the setting the limit comes from
	started       time.Time
	timer         *time.Timer // nil without a limit

	prepare  *queryRequest // its PREPARE_REQUEST (nil: a FETCH began following it)
	prepared chan struct{} // closed when its PREPARE_REQUEST is over (nil: no PREPARE)
	seq      uint64        // the number of its PREPARE among those of the session
	// answered is the number of PREPAREs of the session the gateway had
	// forwarded when the PREPARE of the query got its result (0: not yet).
	answered   uint64
	preparing  bool          // its PREPARE_REQUEST waits for the engine
	unanswered bool          // its PREPARE got no answer although the engine got all of it
	adopted    bool          // a FETCH began following it, and the engine has not shown it has it
	expired    bool          // the limit passed while the engine did not work for it
	timedOut   bool          // the gateway cancels or cancelled it at its limit
	enforcing  bool          // ... and is not done with it yet
	enforced   chan struct{} // closed when the gateway is done with it (nil: not timed out)
	failed     bool          // the gateway could not cancel it on the engine
	reported   bool          // a client got its timeout error
	ended      bool
	dropped    bool // its session closed
	waiting    map[*queryRequest]struct{}
}

// queryRequest is a forwarded request the query time limit follows.
type queryRequest struct {
	l    *queryLimits
	sess *session.Session
	kind wire.MessageType
	q    *trackedQuery // nil for CANCEL_REQUEST
	// ends are the queries a CANCEL_REQUEST of the client ends once the
	// engine confirms it: those it names whose PREPARE had been answered when
	// the gateway forwarded it.
	ends []*trackedQuery
	// decoded marks a FETCH_REQUEST with a batch index that the engine
	// certainly decodes: an error in answer to it ends the query.
	decoded bool
	// early marks a FETCH_REQUEST the gateway forwarded while the PREPARE of
	// its query still waited.
	early bool
	abort context.CancelFunc // cancels the request to the engine
	body  *countingReader    // the body of a PREPARE_REQUEST
}

// requestEvent is how the engine answered a followed request.
type requestEvent uint8

const (
	evFailed   requestEvent = iota // no answer from the engine
	evAnswered                     // an answer that does not end the query
	evResult                       // PREPARE_RESPONSE
	evBatch                        // FETCH_RESPONSE with a batch
	evComplete                     // the terminal FETCH_RESPONSE
	evError                        // ERROR_RESPONSE
)

// enforcement is what the gateway does about a query at its limit.
type enforcement struct {
	elapsed time.Duration
	cut     bool  // its PREPARE was cut off: the engine never got all of it
	fail    error // the gateway cannot cancel it on the engine
}

func newQueryLimits(g *Gateway) *queryLimits {
	return &queryLimits{g: g, sessions: map[string]*sessionQueries{}}
}

// limitFor resolves the time limit of the queries of s (0: none) and the
// setting it comes from. A session adopted after a restart carries no
// groups; the authenticator may know them.
func (l *queryLimits) limitFor(s *session.Session) (time.Duration, string) {
	groups := s.Groups
	if s.Adopted && groups == nil {
		if gl, ok := l.g.auth.(auth.GroupLookup); ok {
			groups = gl.GroupsOf(s.User)
		}
	}
	return policy.QueryTimeout(l.g.cfg, s.User, groups, s.EngineKey.Profile)
}

// fresh reports whether the gateway may act on engines (see Options.Fence).
func (l *queryLimits) fresh() bool {
	return l.g.fence == nil || l.g.fence()
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
		sq = &sessionQueries{sess: s, adopting: s.Adopted}
		l.sessions[s.ID] = sq
	}
	return sq
}

// newQueryLocked starts following a query of sq.
func (l *queryLimits) newQueryLocked(sq *sessionQueries, uuid wire.Hugeint, known bool, clientQueryID wire.OptionalIdx,
	limit time.Duration, source string) *trackedQuery {
	q := &trackedQuery{sq: sq, uuid: uuid, uuidKnown: known, clientQueryID: clientQueryID, limit: limit,
		source: source, started: time.Now(), waiting: map[*queryRequest]struct{}{}}
	if limit > 0 {
		q.timer = time.AfterFunc(limit, func() { l.expire(q) })
	}
	sq.queries = append(sq.queries, q)
	return q
}

// endLocked stops following q.
func (l *queryLimits) endLocked(q *trackedQuery) {
	if q.ended {
		return
	}
	q.ended = true
	if q.timer != nil {
		q.timer.Stop()
	}
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
// follow. An adopted session keeps it once it no longer adopts queries.
func (l *queryLimits) forgetIdleLocked(sq *sessionQueries) {
	if len(sq.queries) == 0 && sq.cancels == 0 && (!sq.sess.Adopted || sq.adopting) && l.sessions[sq.sess.ID] == sq {
		delete(l.sessions, sq.sess.ID)
	}
}

// dropLocked forgets a session and its queries.
func (l *queryLimits) dropLocked(sq *sessionQueries) {
	for _, q := range sq.queries {
		q.ended, q.dropped = true, true
		if q.timer != nil {
			q.timer.Stop()
		}
	}
	sq.queries = nil
	if l.sessions[sq.sess.ID] == sq {
		delete(l.sessions, sq.sess.ID)
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
// it, head the start of the body otherwise, scan watches a PREPARE body that
// streams; abort cancels the request to the engine, and in is the body as
// the transport reads it. The result is nil if the limit does not follow
// the request. local is set if the gateway must answer with that query's
// timeout error instead of forwarding the request. err reports that ctx
// ended, or that the gateway shut down, while a PREPARE waited for the
// gateway to finish with an earlier query.
func (l *queryLimits) track(ctx context.Context, h wire.Header, sess *session.Session, body, head []byte,
	scan *prepareUUIDSink, abort context.CancelFunc, in *countingReader) (r *queryRequest, local *trackedQuery, err error) {
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
		r, err := l.beginPrepare(ctx, sess, h.ClientQueryID, uuid, known, endsTransaction(head), abort, in)
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
		r, local := l.beginReading(ctx, sess, &uuid, ferr == nil && f.BatchIndex > 0, abort)
		return r, local, nil
	case wire.TypeSendDataRequest:
		r, local := l.beginReading(ctx, sess, nil, false, abort)
		return r, local, nil
	case wire.TypeCancelRequest:
		uuid, err := wire.LeadingQueryUUID(head)
		if err != nil {
			return nil, nil, nil
		}
		return l.beginCancel(sess, uuid), nil, nil
	}
	return nil, nil, nil
}

// beginPrepare follows a new query; without a clock if it ends a
// transaction. It first waits until the gateway is done with the queries of
// the session that reached their limit.
func (l *queryLimits) beginPrepare(ctx context.Context, sess *session.Session, clientQueryID wire.OptionalIdx,
	uuid wire.Hugeint, known, endsTxn bool, abort context.CancelFunc, in *countingReader) (*queryRequest, error) {
	limit, source := l.limitFor(sess)
	if limit <= 0 {
		return nil, nil
	}
	if endsTxn {
		limit = 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for {
		sq := l.sessionLocked(sess, true)
		if sq == nil {
			return nil, nil
		}
		if done := sq.cancelsDone; done != nil {
			l.mu.Unlock()
			select {
			case <-done:
				l.mu.Lock()
				continue
			case <-ctx.Done():
				l.mu.Lock()
				return nil, ctx.Err()
			case <-l.g.abortCtx.Done():
				l.mu.Lock()
				return nil, errShuttingDown
			}
		}
		if len(sq.queries) >= maxSessionQueries {
			for _, o := range sq.queries {
				if !o.preparing {
					l.endLocked(o)
					break
				}
			}
		}
		sq.prepares++
		q := l.newQueryLocked(sq, uuid, known, clientQueryID, limit, source)
		q.seq = sq.prepares
		r := &queryRequest{l: l, sess: sess, kind: wire.TypePrepareRequest, q: q, abort: abort, body: in}
		q.prepare, q.prepared, q.preparing = r, make(chan struct{}), true
		q.waiting[r] = struct{}{}
		return r, nil
	}
}

// findLocked returns the query a FETCH (of the query with *uuid) or an
// upload (uuid nil) is for: the newest one whose PREPARE was answered (a
// result exists only then), else the newest one.
func (sq *sessionQueries) findLocked(uuid *wire.Hugeint) *trackedQuery {
	var newest *trackedQuery
	for i := len(sq.queries) - 1; i >= 0; i-- {
		o := sq.queries[i]
		if uuid != nil && (!o.uuidKnown || o.uuid != *uuid) {
			continue
		}
		if !o.preparing {
			return o
		}
		if newest == nil {
			newest = o
		}
	}
	return newest
}

// beginReading follows a FETCH (of the query with *uuid) or an upload (uuid
// nil). decoded marks a FETCH the engine certainly decodes.
func (l *queryLimits) beginReading(ctx context.Context, sess *session.Session, uuid *wire.Hugeint, decoded bool,
	abort context.CancelFunc) (*queryRequest, *trackedQuery) {
	l.mu.Lock()
	for {
		sq := l.sessionLocked(sess, false)
		var q *trackedQuery
		if sq != nil {
			q = sq.findLocked(uuid)
		}
		if q == nil {
			if q = l.adoptLocked(sess, sq, uuid); q == nil {
				l.mu.Unlock()
				return nil, nil
			}
		}
		if q.timedOut {
			if !q.enforcing {
				l.mu.Unlock()
				return nil, q
			}
			// Answer once the gateway knows whether the engine cancelled it.
			done := q.enforced
			l.mu.Unlock()
			select {
			case <-done:
				l.mu.Lock()
				continue // the query may have turned out to be over
			case <-ctx.Done():
			case <-l.g.abortCtx.Done():
			}
			return nil, q
		}
		if q.expired {
			// The limit passed while the engine did not work for the query:
			// the client is back for it, so it is not over.
			e := l.startLocked(q)
			l.mu.Unlock()
			l.run(q, e)
			l.mu.Lock()
			continue
		}
		r := &queryRequest{l: l, sess: sess, kind: wire.TypeFetchRequest, q: q, decoded: decoded, early: q.preparing, abort: abort}
		if uuid == nil {
			r.kind = wire.TypeSendDataRequest
		}
		q.waiting[r] = struct{}{}
		l.mu.Unlock()
		return r, nil
	}
}

// adoptLocked starts following the query that a FETCH for a uuid the gateway
// does not know is for (uuid nil: an upload, which it does not follow).
// Every query of a session that this process opened passed it as a PREPARE,
// so it follows only queries of an adopted session that began before this
// process, and only until the engine has shown what it runs; a FETCH that
// comes after the end of a query, or for a uuid the engine never had,
// starts nothing. The limit of an adopted query counts from this FETCH.
func (l *queryLimits) adoptLocked(sess *session.Session, sq *sessionQueries, uuid *wire.Hugeint) *trackedQuery {
	if uuid == nil || !sess.Adopted {
		return nil
	}
	limit, source := l.limitFor(sess)
	if limit <= 0 {
		return nil
	}
	if sq == nil {
		if sq = l.sessionLocked(sess, true); sq == nil {
			return nil
		}
	}
	if !sq.adopting || len(sq.queries) >= maxSessionQueries {
		l.forgetIdleLocked(sq)
		return nil
	}
	q := l.newQueryLocked(sq, *uuid, true, wire.OptionalIdx{}, limit, source)
	q.adopted = true
	return q
}

// beginCancel follows a CANCEL of the client for the query with uuid (zero:
// whatever runs).
func (l *queryLimits) beginCancel(sess *session.Session, uuid wire.Hugeint) *queryRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	sq := l.sessionLocked(sess, false)
	if sq == nil {
		return nil
	}
	r := &queryRequest{l: l, sess: sess, kind: wire.TypeCancelRequest}
	for _, o := range sq.queries {
		// A PREPARE that still waits, or that went unanswered, may reach the
		// engine after the CANCEL.
		if !o.preparing && !o.unanswered && (uuid.IsZero() || (o.uuidKnown && o.uuid == uuid)) {
			r.ends = append(r.ends, o)
		}
	}
	return r
}

// setUUID records the uuid of a PREPARE whose body streamed past the
// scanner.
func (r *queryRequest) setUUID(uuid wire.Hugeint) {
	l := r.l
	l.mu.Lock()
	defer l.mu.Unlock()
	if q := r.q; !q.ended && !q.uuidKnown {
		q.uuid, q.uuidKnown = uuid, true
	}
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
	r.q.reported = true
	return r.q.messageLocked(), true
}

// finish ends a followed request with what the engine answered.
func (r *queryRequest) finish(ev requestEvent) {
	if r == nil {
		return
	}
	l := r.l
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.q == nil {
		// A CANCEL of the client: once the engine confirmed it, it ended the
		// statement of the connection, so the queries it named are over, and
		// nothing from before an adoption runs any more.
		if ev != evAnswered {
			return
		}
		if sq := l.sessions[r.sess.ID]; sq != nil && sq.sess == r.sess {
			sq.adopting = false
		}
		for _, o := range r.ends {
			l.endLocked(o)
		}
		return
	}
	q := r.q
	delete(q.waiting, r)
	if r == q.prepare {
		q.preparing = false
		close(q.prepared)
	}
	if q.ended {
		return
	}
	sq := q.sq
	switch r.kind {
	case wire.TypePrepareRequest:
		switch ev {
		case evResult, evAnswered:
			// The engine made the query its statement: it replaced the
			// queries that the gateway knows to be older (also one from
			// before an adoption).
			q.answered = sq.prepares
			sq.adopting = false
			for _, o := range append([]*trackedQuery(nil), sq.queries...) {
				if o != q && o.replacedBy(q) {
					l.endLocked(o)
				}
			}
		case evError:
			// Also a query that timed out: its client got the error, and
			// there is no result to fetch.
			l.endLocked(q)
		case evFailed:
			if q.uuidKnown && (r.body == nil || r.body.sentAll()) {
				// No answer, but the engine got the whole PREPARE: it may
				// run the query (the client went away).
				q.unanswered = true
			} else {
				l.endLocked(q) // the engine cannot have it
			}
		}
	case wire.TypeFetchRequest:
		switch {
		case r.early:
			// The engine may not have made the query its statement yet: the
			// answer may be about the statement before.
		case ev == evComplete:
			sq.adopting = false
			l.endLocked(q)
		case ev == evBatch:
			q.adopted, sq.adopting = false, false // the engine runs the query
		case q.timedOut:
			// The engine's answer to the CANCEL: later requests of the query
			// get the timeout error from the gateway.
		case ev == evError && (r.decoded || q.adopted):
			// An adopted query that the engine has not shown it has: its limit
			// began at this FETCH, so ending it loses nothing.
			l.endLocked(q)
		}
	}
}

// replacedBy reports whether the result of the PREPARE of q proves that the
// engine replaced o: o began before this process (a FETCH began following
// it), or its PREPARE got its result before the gateway forwarded the one of
// q, or the gateway has cancelled it at its limit. A PREPARE that waited or
// went unanswered when the one of q was forwarded may have reached the
// engine after it.
func (o *trackedQuery) replacedBy(q *trackedQuery) bool {
	switch {
	case o.preparing || o.enforcing:
		return false
	case o.prepare == nil:
		return true
	case o.answered > 0 && o.answered < q.seq:
		return true
	}
	return o.timedOut && !o.failed
}

// expire runs when the limit of q has passed.
func (l *queryLimits) expire(q *trackedQuery) {
	l.mu.Lock()
	if q.ended || q.timedOut || q.expired {
		l.mu.Unlock()
		return
	}
	if !l.fresh() {
		// Another replica may serve the session by now: leave its engine
		// alone, and look again later.
		q.timer.Reset(fenceRetry)
		l.mu.Unlock()
		return
	}
	if len(q.waiting) == 0 && !q.unanswered {
		// The engine does not work for the query, which may well be
		// complete. The limit is enforced when the client comes back for it
		// (beginReading).
		q.expired = true
		l.mu.Unlock()
		return
	}
	e := l.startLocked(q)
	l.mu.Unlock()
	l.run(q, e)
}

// startLocked marks q timed out and decides how to end it, in one critical
// section with the checks before: nothing can end q or forward a PREPARE of
// its session in between. Until run is done, the session's PREPAREs wait.
func (l *queryLimits) startLocked(q *trackedQuery) *enforcement {
	q.timedOut, q.expired, q.enforcing = true, false, true
	q.enforced = make(chan struct{})
	sq := q.sq
	e := &enforcement{elapsed: time.Since(q.started)}
	switch {
	case q.preparing && q.prepare.body != nil && q.prepare.body.stop():
		// The engine has not got the whole PREPARE, and now never will.
		e.cut = true
	case !q.uuidKnown:
		e.fail = errors.New("the gateway could not read the query's uuid")
	case q.uuid.IsZero() && sq.prepares > q.seq:
		// CANCEL with the zero uuid ends whatever runs: it could meet a
		// later query of the session.
		e.fail = errors.New("the query has the zero uuid, and its session sent a newer query")
	}
	if e.fail != nil {
		q.failed = true
	}
	if sq.cancels == 0 {
		sq.cancelsDone = make(chan struct{})
	}
	sq.cancels++
	return e
}

// run carries out an enforcement that startLocked began: it ends q on its
// engine and makes the requests of q fail with the timeout error.
func (l *queryLimits) run(q *trackedQuery, e *enforcement) {
	sq := q.sq
	var result string
	var err error
	switch {
	case e.cut:
		l.abortWaiting(q)
		result = "cancelled" // the query never reached the engine
	case e.fail != nil:
		l.abortWaiting(q)
		result, err = "failed", e.fail
	default:
		result, err = l.cancelOnEngine(q)
		if result == "failed" {
			l.mu.Lock()
			q.failed = true
			l.mu.Unlock()
			// The client gets its error now, whatever the engine does.
			l.abortWaiting(q)
		}
	}

	l.mu.Lock()
	uuid := q.uuid
	q.enforcing = false
	close(q.enforced)
	if result == "cancelled" && q.unanswered {
		l.endLocked(q) // no client waits for it, and the engine has ended it
	}
	if sq.cancels--; sq.cancels == 0 {
		close(sq.cancelsDone)
		sq.cancelsDone = nil
		l.forgetIdleLocked(sq)
	}
	l.mu.Unlock()

	if result == "" {
		// The engine runs another statement on the connection (the query was
		// over), or the session closed.
		l.g.log.Debug("query time limit reached by a query that was already over", "session", sq.sess.ID,
			"queryUuid", uuid.String())
		return
	}
	sess := sq.sess
	attrs := []any{"user", sess.User, "session", sess.ID, "engine", sess.EngineID, "key", sess.EngineKey.String(),
		"queryUuid", uuid.String(), "limit", q.limit.String(), "source", q.source,
		"elapsed", e.elapsed.Round(time.Millisecond).String(), "cancel", result}
	if q.clientQueryID.Valid {
		attrs = append(attrs, "clientQueryId", q.clientQueryID.Value)
	}
	if err != nil {
		attrs = append(attrs, "err", err)
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
// "cancelled" if the engine confirmed it, "failed" if the gateway could not
// cancel it, and "" if the engine runs another statement on the connection
// (then q was over, and it ends) or the session closed.
func (l *queryLimits) cancelOnEngine(q *trackedQuery) (string, error) {
	for attempt := 1; ; attempt++ {
		l.mu.Lock()
		dropped, ended, preparing, reported := q.dropped, q.ended, q.preparing, q.reported
		l.mu.Unlock()
		switch {
		case dropped:
			return "", nil // the session closed, and its engine connection with it
		case ended && attempt == 1 && !reported:
			return "", nil // it ended on its own as its limit passed, and no client saw the timeout
		case ended:
			return "cancelled", nil // the engine has ended it
		case !l.fresh():
			return "failed", errNotFresh
		}
		err := l.sendCancel(q)
		if err == nil {
			if !preparing {
				// The engine ended its statement, which had the query's uuid
				// (for the zero uuid: nothing newer runs, see startLocked).
				return "cancelled", nil
			}
			// The PREPARE still waited when the CANCEL left: the engine may
			// have ended the statement before it (the zero uuid, or the uuid
			// of that statement once more). Once the PREPARE is answered with
			// an error, the query is over; with a result, cancel again.
			if l.waitPrepared(q, cancelRetryDelay) {
				l.mu.Lock()
				result := q.answered > 0 && !q.ended
				l.mu.Unlock()
				if !result {
					return "cancelled", nil
				}
			}
			if attempt == cancelRetries {
				return "failed", errors.New("the engine did not end the query's PREPARE after the CANCEL")
			}
			continue
		}
		var ee *upstream.EngineError
		if !errors.As(err, &ee) || !strings.HasPrefix(ee.Response.Message, msgCancelOther) {
			return "failed", err
		}
		if !preparing {
			// The engine runs another statement: the query was over, and
			// nothing waits for it.
			l.mu.Lock()
			q.timedOut = false
			l.endLocked(q)
			l.mu.Unlock()
			return "", nil
		}
		// The engine has not made the PREPARE its statement yet (it still
		// reads or authorizes it).
		if attempt == cancelRetries {
			return "failed", err
		}
		l.waitPrepared(q, cancelRetryDelay)
	}
}

// waitPrepared waits at most d for the PREPARE of q to be over and reports
// whether it is.
func (l *queryLimits) waitPrepared(q *trackedQuery, d time.Duration) bool {
	if q.prepared == nil {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-q.prepared:
		return true
	case <-t.C:
		return false
	}
}

// sendCancel sends CANCEL for q to its engine.
func (l *queryLimits) sendCancel(q *trackedQuery) error {
	sess := q.sq.sess
	target, ok := l.g.engines.Lookup(sess.EngineID)
	if !ok {
		return fmt.Errorf("engine %s is not available", sess.EngineID)
	}
	return l.g.up.Control(context.Background(), target.Endpoint, wire.EncodeCancel(sess.BackendConnID, q.uuid))
}

// abortWaiting cancels the requests of q that wait for the engine; their
// handlers answer with the timeout error.
func (l *queryLimits) abortWaiting(q *trackedQuery) {
	l.mu.Lock()
	var aborts []context.CancelFunc
	for r := range q.waiting {
		aborts = append(aborts, r.abort)
	}
	l.mu.Unlock()
	for _, abort := range aborts {
		abort()
	}
}

// message returns the error a client gets for a query that timed out.
func (l *queryLimits) message(q *trackedQuery) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	q.reported = true
	return q.messageLocked()
}

func (q *trackedQuery) messageLocked() string {
	if q.failed {
		return fmt.Sprintf("quackmux: the query ran longer than its time limit of %s (%s); the gateway could not "+
			"cancel it on the engine, so it may still be running", q.limit, q.source)
	}
	return fmt.Sprintf("quackmux: the query ran longer than its time limit of %s (%s) and was cancelled", q.limit, q.source)
}

// endsTransaction reports whether the start of a PREPARE_REQUEST body holds
// a whole statement that ends a transaction (COMMIT, END, ROLLBACK, ABORT,
// optionally with WORK or TRANSACTION). The DuckDB client sends one after
// every statement on a remote database.
func endsTransaction(body []byte) bool {
	sql, truncated, err := wire.PeekPrepareSQL(body, maxQueryHeadBytes)
	if err != nil || truncated {
		return false
	}
	for i := 0; i < len(sql); i++ {
		if sql[i] >= 0x80 {
			return false // DuckDB's keywords and white space are ASCII
		}
	}
	words := strings.Fields(strings.ToUpper(strings.TrimSuffix(strings.TrimSpace(sql), ";")))
	if len(words) == 0 || len(words) > 2 {
		return false
	}
	switch words[0] {
	case "COMMIT", "END", "ROLLBACK", "ABORT":
	default:
		return false
	}
	return len(words) == 1 || words[1] == "WORK" || words[1] == "TRANSACTION"
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

// peekQueryHead returns the start of a FETCH, CANCEL or PREPARE body without
// consuming it; length is the body length (negative: unknown).
func peekQueryHead(br *bufio.Reader, length int64) []byte {
	n := maxQueryHeadBytes
	if length >= 0 && length < int64(n) {
		n = int(length)
	}
	b, _ := br.Peek(n)
	return b
}
