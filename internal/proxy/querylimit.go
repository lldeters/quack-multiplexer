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
// statement yet, and they may be about the statement before. The gateway
// waits for the answer to a PREPARE also when its client goes away: the
// engine runs the query anyway.
//
// The engine gets a request only once all of it arrived, and then serves
// the statement the connection has. A FETCH or an upload that no query of
// the session claims when the gateway forwards it (its body may come
// slowly) therefore counts as a request of the first query the gateway
// begins to follow afterwards that it may serve: a FETCH one with its uuid,
// an upload any query. So do the requests of a query that ends while they
// are in flight, for the next query they may serve.
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
//
// The Quack server lets the requests of a connection that are in flight
// finish after a DISCONNECT, and their statement runs on with them, while a
// CANCEL no longer reaches it. When a session closes while the limit may
// still have to cancel a query of it, the gateway therefore keeps following
// the session's queries and holds the DISCONNECT to the engine until it is
// done with them (the client's DISCONNECT is answered at once all the same).
// Requests of the session that it has not forwarded yet are not forwarded
// any more.

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
// for clients that race their requests. The gateway never forgets a query
// that the engine may still run, or the client could stop its clock: at the
// bound it forgets only queries whose cancel the engine confirmed, and
// otherwise does not forward a further PREPARE (or a FETCH that would adopt
// a query) but answers it with an error. Queries whose cancel failed (the
// gateway gave up on them) do not count: it can do nothing more about them
// and keeps them only to answer their later requests itself, at most this
// many, the newest.
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

// errTooManyQueries reports a request the gateway does not forward because
// it follows maxSessionQueries queries of the session already.
var errTooManyQueries = errors.New("too many queries of the session are not over")

// errSessionClosed reports a request the gateway does not forward because
// its session closed after the request arrived: forwarded now, it could keep
// a query of the session running on the engine past the DISCONNECT.
var errSessionClosed = errors.New("the session is closed")

// tooManyQueriesMessage is the client's error for errTooManyQueries.
var tooManyQueriesMessage = fmt.Sprintf("quackmux: the session has %d queries that are not over, the most the "+
	"gateway follows; it did not forward this request. Cancel those queries or reconnect.", maxSessionQueries)

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
	// closed is set once the session closed; hold, while set, holds its
	// DISCONNECT to the engine (see sessionClosed).
	closed bool
	hold   chan struct{}
	// strays are the FETCHes and uploads in flight that no query claims
	// (see strayLocked).
	strays map[*queryRequest]struct{}
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
	q    *trackedQuery // nil for CANCEL_REQUEST and a stray; guarded by queryLimits.mu
	// sq follows a stray (a FETCH or an upload that no query claims), uuid
	// is the uuid of a FETCH.
	sq   *sessionQueries
	uuid wire.Hugeint
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
	body  *bodyGate          // the body of a PREPARE_REQUEST
	// client is the context of the client's request of a PREPARE that
	// outlives its client (outlivesClient).
	client context.Context
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
	// quiet marks a precaution: no request of the query waits, and the
	// gateway does not know whether it still runs (its PREPARE got no
	// answer), so it is no timeout a client sees.
	quiet bool
}

func newQueryLimits(g *Gateway) *queryLimits {
	return &queryLimits{g: g, sessions: map[string]*sessionQueries{}}
}

// limitFor resolves the time limit of the queries of s (0: none) and the
// setting it comes from.
func (l *queryLimits) limitFor(s *session.Session) (time.Duration, string) {
	return policy.QueryTimeout(l.g.cfg, s.User, l.groupsOf(s), s.EngineKey.Profile)
}

// limited reports whether the queries of s have a time limit.
func (l *queryLimits) limited(s *session.Session) bool {
	return policy.QueryTimeoutLimit(l.g.cfg, s.User, l.groupsOf(s), s.EngineKey.Profile) > 0
}

// groupsOf returns the groups of s. A session adopted after a restart
// carries none; the authenticator may know them.
func (l *queryLimits) groupsOf(s *session.Session) []string {
	if s.Adopted && s.Groups == nil {
		if gl, ok := l.g.auth.(auth.GroupLookup); ok {
			return gl.GroupsOf(s.User)
		}
	}
	return s.Groups
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
	// Its FETCHes and uploads in flight may serve a later statement.
	for r := range q.waiting {
		if r.kind != wire.TypePrepareRequest {
			delete(q.waiting, r)
			l.rehomeLocked(sq, r)
		}
	}
	l.forgetIdleLocked(sq)
}

// rehomeLocked hands a FETCH or an upload in flight to the newest query of
// sq that it may serve (a FETCH: one with its uuid), or makes it a stray.
// Its answer ends nothing: it may be about another statement.
func (l *queryLimits) rehomeLocked(sq *sessionQueries, r *queryRequest) {
	r.early = true
	for i := len(sq.queries) - 1; i >= 0; i-- {
		o := sq.queries[i]
		if o.limit > 0 && (r.kind != wire.TypeFetchRequest || (o.uuidKnown && o.uuid == r.uuid)) {
			r.q, r.sq = o, nil
			o.waiting[r] = struct{}{}
			return
		}
	}
	r.q = nil
	l.addStrayLocked(sq, r)
}

// addStrayLocked follows r as a stray of sq.
func (l *queryLimits) addStrayLocked(sq *sessionQueries, r *queryRequest) {
	r.sq = sq
	if sq.strays == nil {
		sq.strays = map[*queryRequest]struct{}{}
	}
	sq.strays[r] = struct{}{}
}

// homeLocked hands q the strays of its session that it may serve: uploads,
// and FETCHes with its uuid.
func (l *queryLimits) homeLocked(q *trackedQuery) {
	if q.limit <= 0 {
		return // nothing to enforce for it
	}
	sq := q.sq
	for r := range sq.strays {
		if r.kind == wire.TypeFetchRequest && (!q.uuidKnown || r.uuid != q.uuid) {
			continue
		}
		delete(sq.strays, r)
		r.q, r.sq = q, nil
		q.waiting[r] = struct{}{}
	}
}

// forgetIdleLocked drops the state of a session that has nothing left to
// follow. An adopted session keeps it once it no longer adopts queries. A
// closed session keeps it, and its DISCONNECT waits, only while the limit
// may still have to cancel one of its queries.
func (l *queryLimits) forgetIdleLocked(sq *sessionQueries) {
	if sq.closed {
		if !sq.holdsLocked() {
			l.dropLocked(sq)
		}
		return
	}
	if len(sq.queries) == 0 && len(sq.strays) == 0 && sq.cancels == 0 && (!sq.sess.Adopted || sq.adopting) &&
		l.sessions[sq.sess.ID] == sq {
		delete(l.sessions, sq.sess.ID)
	}
}

// holdsLocked reports whether the limit may still have to cancel a query of
// sq on its engine: an enforcement runs, or a query with a clock that has
// not run out of time waits for the engine, or got no answer to its PREPARE
// although the engine got all of it (the engine may run it).
func (sq *sessionQueries) holdsLocked() bool {
	if sq.cancels > 0 {
		return true
	}
	for _, q := range sq.queries {
		if q.limit > 0 && !q.timedOut && (len(q.waiting) > 0 || q.unanswered) {
			return true
		}
	}
	return false
}

// roomLocked makes room for one more query of sq and reports whether there
// is room. It forgets only queries that the engine is done with: those
// whose cancel at their limit it confirmed, or whose PREPARE the gateway
// cut off. Later requests of them get the engine's error then instead of
// the gateway's. Queries the gateway gave up on do not count.
func (l *queryLimits) roomLocked(sq *sessionQueries) bool {
	if len(sq.queries) < maxSessionQueries {
		return true
	}
	kept := make([]*trackedQuery, 0, len(sq.queries))
	counted := 0
	for _, o := range sq.queries {
		if o.timedOut && !o.enforcing && !o.failed {
			forgetLocked(o)
			continue
		}
		if !o.givenUp() {
			counted++
		}
		kept = append(kept, o)
	}
	sq.queries = kept
	return counted < maxSessionQueries
}

// givenUp reports whether the gateway could not cancel q at its limit and
// is done trying: q may still run, but the gateway can do nothing more
// about it than answer its later requests itself.
func (q *trackedQuery) givenUp() bool { return q.timedOut && q.failed && !q.enforcing }

// pruneLocked forgets the oldest queries of sq that the gateway gave up on
// beyond maxSessionQueries.
func (l *queryLimits) pruneLocked(sq *sessionQueries) {
	n := 0
	for _, o := range sq.queries {
		if o.givenUp() {
			n++
		}
	}
	if n <= maxSessionQueries {
		return
	}
	kept := make([]*trackedQuery, 0, len(sq.queries))
	for _, o := range sq.queries {
		if n > maxSessionQueries && o.givenUp() {
			forgetLocked(o)
			n--
			continue
		}
		kept = append(kept, o)
	}
	sq.queries = kept
}

// forgetLocked marks a query that the gateway stops following without
// evidence of its end: requests of it in flight keep it (their answers
// still get its timeout error), nothing else does.
func forgetLocked(q *trackedQuery) {
	q.ended = true
	if q.timer != nil {
		q.timer.Stop()
	}
}

// dropLocked forgets a session and its queries, and lets its DISCONNECT go
// if it holds it. A PREPARE of the session that waits for the engine
// although its client went away ends with its client again: nothing
// follows its query any more.
func (l *queryLimits) dropLocked(sq *sessionQueries) {
	for _, q := range sq.queries {
		q.ended, q.dropped = true, true
		if q.timer != nil {
			q.timer.Stop()
		}
		for r := range q.waiting {
			if r.client != nil {
				context.AfterFunc(r.client, r.abort)
			}
		}
	}
	sq.queries, sq.strays = nil, nil
	if l.sessions[sq.sess.ID] == sq {
		delete(l.sessions, sq.sess.ID)
	}
	if sq.hold != nil {
		close(sq.hold)
		sq.hold = nil
	}
}

// sessionClosed is the session manager's close hook. keep reports that the
// engine still has the session's connection and is about to get its
// DISCONNECT. While the limit may still have to cancel a query of the
// session on the engine (holdsLocked), and only then, the gateway keeps
// following the session's queries and returns a channel that holds the
// DISCONNECT; the channel is closed once nothing holds it any more.
func (l *queryLimits) sessionClosed(s *session.Session, keep bool) <-chan struct{} {
	l.mu.Lock()
	sq := l.sessions[s.ID]
	if sq == nil || sq.sess != s {
		l.mu.Unlock()
		return nil
	}
	sq.closed = true
	if !keep || !sq.holdsLocked() {
		l.dropLocked(sq)
		l.mu.Unlock()
		return nil
	}
	hold := make(chan struct{})
	sq.hold = hold
	l.mu.Unlock()
	l.g.log.Debug("the DISCONNECT of a closed session waits: the query time limit may still have to cancel a query of it",
		"session", s.ID, "engine", s.EngineID)
	return hold
}

// track starts following a request of sess right before the gateway
// forwards it. body is the body after the header if the gateway buffered
// it, head the start of the body otherwise, scan watches a PREPARE body that
// streams; abort cancels the request to the engine, and in is the body of a
// PREPARE as the transport reads it. The result is nil if the limit does not
// follow the request. local is set if the gateway must answer with that query's
// timeout error instead of forwarding the request. err reports that ctx
// ended, or that the gateway shut down, while a PREPARE waited for the
// gateway to finish with an earlier query, or errTooManyQueries, or
// errSessionClosed.
func (l *queryLimits) track(ctx context.Context, h wire.Header, sess *session.Session, body, head []byte,
	scan *prepareUUIDSink, abort context.CancelFunc, in *bodyGate) (r *queryRequest, local *trackedQuery, err error) {
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
		return l.beginReading(ctx, sess, &uuid, ferr == nil && f.BatchIndex > 0, abort)
	case wire.TypeSendDataRequest:
		return l.beginReading(ctx, sess, nil, false, abort)
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
	uuid wire.Hugeint, known, endsTxn bool, abort context.CancelFunc, in *bodyGate) (*queryRequest, error) {
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
		if sq == nil || sq.closed {
			return nil, errSessionClosed
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
		if !l.roomLocked(sq) {
			return nil, errTooManyQueries
		}
		sq.prepares++
		q := l.newQueryLocked(sq, uuid, known, clientQueryID, limit, source)
		q.seq = sq.prepares
		r := &queryRequest{l: l, sess: sess, kind: wire.TypePrepareRequest, q: q, abort: abort, body: in}
		if limit > 0 {
			r.client = ctx // see outlivesClient
		}
		q.prepare, q.prepared, q.preparing = r, make(chan struct{}), true
		q.waiting[r] = struct{}{}
		l.homeLocked(q)
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
	abort context.CancelFunc) (*queryRequest, *trackedQuery, error) {
	l.mu.Lock()
	for {
		sq := l.sessionLocked(sess, false)
		if l.closedLocked(sess, sq) {
			l.mu.Unlock()
			return nil, nil, errSessionClosed
		}
		var q *trackedQuery
		if sq != nil {
			q = sq.findLocked(uuid)
		}
		if q == nil {
			var err error
			if q, err = l.adoptLocked(sess, sq, uuid); err != nil {
				l.mu.Unlock()
				return nil, nil, err
			}
			if q == nil {
				r, err := l.strayLocked(sess, sq, uuid, abort)
				l.mu.Unlock()
				return r, nil, err
			}
		}
		if q.timedOut {
			if !q.enforcing {
				l.mu.Unlock()
				return nil, q, nil
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
			return nil, q, nil
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
		return r, nil, nil
	}
}

// closedLocked reports whether the session sess, whose state is sq (nil:
// none), closed while the limit applies to it: its requests are not
// forwarded any more then. The close hook marks the state of a session
// after the session itself, under l.mu: a session that the hook has not
// seen yet was open when its requests began to be followed.
func (l *queryLimits) closedLocked(sess *session.Session, sq *sessionQueries) bool {
	if sq != nil {
		return sq.closed
	}
	if _, closed := sess.Closed(); !closed {
		return false
	}
	return l.limited(sess)
}

// strayLocked follows a FETCH (of the query with *uuid) or an upload (uuid
// nil) that no query of the session claims, as a stray of the session (nil:
// the limit does not apply to it). The gateway forwards it; a query that
// it may serve and that the gateway begins to follow while it is in flight
// takes it (homeLocked).
func (l *queryLimits) strayLocked(sess *session.Session, sq *sessionQueries, uuid *wire.Hugeint,
	abort context.CancelFunc) (*queryRequest, error) {
	if sq == nil {
		if !l.limited(sess) {
			return nil, nil
		}
		if sq = l.sessionLocked(sess, true); sq == nil {
			return nil, errSessionClosed
		}
	}
	r := &queryRequest{l: l, sess: sess, kind: wire.TypeSendDataRequest, early: true, abort: abort}
	if uuid != nil {
		r.kind, r.uuid = wire.TypeFetchRequest, *uuid
	}
	l.addStrayLocked(sq, r)
	return r, nil
}

// adoptLocked starts following the query that a FETCH for a uuid the gateway
// does not know is for (uuid nil: an upload, which it does not follow).
// Every query of a session that this process opened passed it as a PREPARE,
// so it follows only queries of an adopted session that began before this
// process, and only until the engine has shown what it runs; a FETCH that
// comes after the end of a query, or for a uuid the engine never had,
// starts nothing. The limit of an adopted query counts from this FETCH. It
// returns errTooManyQueries if it would adopt a query but has no room: the
// FETCH may be for a query from before the restart that still runs.
func (l *queryLimits) adoptLocked(sess *session.Session, sq *sessionQueries, uuid *wire.Hugeint) (*trackedQuery, error) {
	if uuid == nil || !sess.Adopted {
		return nil, nil
	}
	limit, source := l.limitFor(sess)
	if limit <= 0 {
		return nil, nil
	}
	if sq == nil {
		if sq = l.sessionLocked(sess, true); sq == nil {
			return nil, nil
		}
	}
	if !sq.adopting {
		l.forgetIdleLocked(sq)
		return nil, nil
	}
	if !l.roomLocked(sq) {
		return nil, errTooManyQueries
	}
	q := l.newQueryLocked(sq, *uuid, true, wire.OptionalIdx{}, limit, source)
	q.adopted = true
	return q, nil
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
		l.homeLocked(q)
	}
}

// outlivesClient reports whether r is a PREPARE that keeps waiting for the
// engine when its client goes away. The engine runs the query anyway (it
// does not notice that the client left), and only its answer tells the
// gateway whether the query is over: without it the gateway would follow
// the query until its limit, and cancel it then whatever the engine had
// done. The limit bounds the wait.
func (r *queryRequest) outlivesClient() bool {
	// The query of a PREPARE never changes (others can, see rehomeLocked).
	return r != nil && r.kind == wire.TypePrepareRequest && r.q != nil && r.q.limit > 0
}

// timeoutError returns the error the gateway answers with instead of the
// engine's answer to r: the query of r ran out of time.
func (r *queryRequest) timeoutError() (string, bool) {
	if r == nil {
		return "", false
	}
	r.l.mu.Lock()
	defer r.l.mu.Unlock()
	q := r.q
	if q == nil || !q.timedOut {
		return "", false
	}
	q.reported = true
	return q.messageLocked(), true
}

// finish ends a followed request with what the engine answered.
func (r *queryRequest) finish(ev requestEvent) {
	if r == nil {
		return
	}
	l := r.l
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.q == nil && r.kind != wire.TypeCancelRequest {
		// A stray: no query took it.
		if sq := r.sq; sq != nil {
			delete(sq.strays, r)
			l.forgetIdleLocked(sq)
		}
		return
	}
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
	if sq.closed {
		// The DISCONNECT of the session may have waited for this request.
		defer l.forgetIdleLocked(sq)
	}
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
			if q.limit > 0 && q.uuidKnown && (r.body == nil || r.body.sentAll()) {
				// No answer, but the engine got the whole PREPARE: it may
				// run the query. (The gateway waits for the answer also when
				// the client goes away; this takes a failure between gateway
				// and engine, or the gateway's own abort.)
				q.unanswered = true
			} else {
				// The engine cannot have it, or there is no limit to enforce.
				l.endLocked(q)
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
		// alone, and look again later. But do not hold a PREPARE of the
		// query for the engine's answer once its client went away: the
		// query is followed as one whose PREPARE got no answer then, and
		// cancelled as a precaution once the lease is fresh.
		var aborts []context.CancelFunc
		for r := range q.waiting {
			if r.client != nil && r.client.Err() != nil {
				aborts = append(aborts, r.abort)
			}
		}
		q.timer.Reset(fenceRetry)
		l.mu.Unlock()
		for _, abort := range aborts {
			abort()
		}
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
	e := &enforcement{elapsed: time.Since(q.started), quiet: q.unanswered && len(q.waiting) == 0}
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
	if q.givenUp() && !q.ended {
		l.pruneLocked(sq)
	}
	if sq.cancels--; sq.cancels == 0 {
		close(sq.cancelsDone)
		sq.cancelsDone = nil
		l.forgetIdleLocked(sq)
	}
	l.mu.Unlock()

	if result == "" {
		// The engine runs another statement on the connection (the query was
		// over), or the session or its engine connection is gone.
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
	if e.quiet {
		// The CANCEL was a precaution: no client waits for the query, and the
		// gateway does not know whether it still ran after its PREPARE got
		// no answer (the client got an error for that).
		l.g.log.Debug("query time limit reached by a query whose PREPARE got no answer", attrs...)
		return
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
// (then q was over, and it ends), or the session or its engine connection
// is gone.
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
		isEngineError := errors.As(err, &ee)
		l.mu.Lock()
		dropped = q.dropped
		l.mu.Unlock()
		if dropped || (isEngineError && upstream.IsSessionGone(ee.Response.Message)) {
			// The session closed while the CANCEL was on its way, or the
			// engine no longer has its connection: the query ended with it.
			return "", nil
		}
		if !isEngineError || !strings.HasPrefix(ee.Response.Message, msgCancelOther) {
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
