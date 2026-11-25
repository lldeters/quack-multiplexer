package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"quackmux/internal/config"
	"quackmux/internal/metrics"
	"quackmux/internal/session"
	"quackmux/internal/wire"
)

// These tests drive the bookkeeping of the query time limit directly, in
// orders of events that the integration tests cannot force.

// newTestLimits returns the query limits of a gateway without engines and
// a session they follow.
func newTestLimits(t *testing.T, adopted bool) (*queryLimits, *session.Session) {
	t.Helper()
	cfg, err := config.Parse([]byte("auth: {mode: none}\nprovider: {type: kubernetes}\nlimits: {queryTimeout: 1h}\n"))
	if err != nil {
		t.Fatal(err)
	}
	g := &Gateway{cfg: cfg, log: slog.New(slog.DiscardHandler), now: time.Now}
	g.abortCtx, g.abort = context.WithCancel(context.Background())
	t.Cleanup(g.abort)
	l := newQueryLimits(g)
	s := &session.Session{ID: "s1", User: "alice", Adopted: adopted}
	l.sessions[s.ID] = &sessionQueries{sess: s, adopting: adopted}
	t.Cleanup(func() { l.sessionClosed(s, false) }) // stops the timers
	return l, s
}

func (l *queryLimits) testPrepare(t *testing.T, s *session.Session, u wire.Hugeint) *queryRequest {
	t.Helper()
	r, err := l.beginPrepare(context.Background(), s, wire.OptionalIdx{}, u, true, false, func() {}, nil)
	if err != nil || r == nil {
		t.Fatalf("beginPrepare: %v, %v", r, err)
	}
	return r
}

func (l *queryLimits) followed(q *trackedQuery) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !q.ended
}

// The session closes, or the query ends, after the gateway decided to
// enforce the limit of a query with the zero uuid and before it acts on the
// engine: the gateway once indexed the empty list of the session's queries
// then, in the timer goroutine, and crashed.
func TestEnforcementOfAQueryThatEndsMeanwhile(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(l *queryLimits, s *session.Session, q *trackedQuery)
	}{
		{"session closed", func(l *queryLimits, s *session.Session, _ *trackedQuery) { l.sessionClosed(s, false) }},
		{"query ended", func(l *queryLimits, _ *session.Session, q *trackedQuery) {
			l.mu.Lock()
			l.endLocked(q)
			l.mu.Unlock()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, s := newTestLimits(t, false)
			q := l.testPrepare(t, s, wire.Hugeint{}).q
			l.mu.Lock()
			e := l.startLocked(q)
			l.mu.Unlock()
			tc.end(l, s, q)
			l.run(q, e) // without engines: any CANCEL would fail on a nil manager
			select {
			case <-q.enforced:
			default:
				t.Fatal("the enforcement is not marked done")
			}
			l.mu.Lock()
			defer l.mu.Unlock()
			if sq := l.sessions[s.ID]; sq != nil && sq.cancelsDone != nil {
				t.Fatal("PREPAREs of the session still wait for the enforcement")
			}
		})
	}
}

// A PREPARE result proves that the engine replaced only the queries whose
// PREPARE was answered before the gateway forwarded that PREPARE; two
// PREPAREs in flight at once may reach the engine in either order.
func TestPrepareResultEndsOnlyOlderQueries(t *testing.T) {
	l, s := newTestLimits(t, false)
	r1 := l.testPrepare(t, s, wire.Hugeint{Upper: 1})
	r2 := l.testPrepare(t, s, wire.Hugeint{Upper: 2})
	r2.finish(evResult)
	r1.finish(evResult)
	if !l.followed(r1.q) || !l.followed(r2.q) {
		t.Fatalf("followed: first %v, second %v; want both (the engine may have run either last)", l.followed(r1.q), l.followed(r2.q))
	}
	r3 := l.testPrepare(t, s, wire.Hugeint{Upper: 3})
	r3.finish(evResult)
	if l.followed(r1.q) || l.followed(r2.q) || !l.followed(r3.q) {
		t.Fatalf("followed: %v %v %v; want only the third", l.followed(r1.q), l.followed(r2.q), l.followed(r3.q))
	}
	// A PREPARE that went unanswered although the engine got all of it may
	// reach the engine after a later one.
	r4 := l.testPrepare(t, s, wire.Hugeint{Upper: 4})
	r4.finish(evFailed) // the client went away; the body counts as sent (testPrepare has none)
	r5 := l.testPrepare(t, s, wire.Hugeint{Upper: 5})
	r5.finish(evResult)
	if !l.followed(r4.q) || l.followed(r3.q) {
		t.Fatalf("followed: unanswered %v, answered before %v; want the unanswered one only", l.followed(r4.q), l.followed(r3.q))
	}
}

// A CANCEL of the client ends only the queries whose PREPARE had been
// answered when the gateway forwarded it: one still on its way may reach the
// engine after the CANCEL.
func TestClientCancelEndsOnlyAnsweredQueries(t *testing.T) {
	l, s := newTestLimits(t, false)
	r := l.testPrepare(t, s, wire.Hugeint{Upper: 1})
	early := l.beginCancel(s, wire.Hugeint{})
	r.finish(evResult)
	early.finish(evAnswered)
	if !l.followed(r.q) {
		t.Fatal("a CANCEL forwarded while the PREPARE waited ended the query")
	}
	other := l.beginCancel(s, wire.Hugeint{Upper: 9})
	other.finish(evAnswered)
	if !l.followed(r.q) {
		t.Fatal("a CANCEL for another uuid ended the query")
	}
	later := l.beginCancel(s, wire.Hugeint{Upper: 1})
	later.finish(evAnswered)
	if l.followed(r.q) {
		t.Fatal("the confirmed CANCEL did not end the query")
	}
}

// Answers to a FETCH that the gateway forwarded while the PREPARE of its
// query waited end nothing: the engine may not have the statement yet.
func TestEarlyFetchAnswersEndNothing(t *testing.T) {
	for _, ev := range []requestEvent{evComplete, evError} {
		l, s := newTestLimits(t, false)
		u := wire.Hugeint{Upper: 1}
		p := l.testPrepare(t, s, u)
		f, local, err := l.beginReading(context.Background(), s, &u, true, func() {})
		if f == nil || local != nil || err != nil || !f.early {
			t.Fatalf("FETCH while the PREPARE waits: %+v, %v, %v", f, local, err)
		}
		f.finish(ev)
		if !l.followed(p.q) {
			t.Fatalf("an early FETCH answered with event %d ended the query", ev)
		}
		p.finish(evResult)
		f, _, _ = l.beginReading(context.Background(), s, &u, true, func() {})
		f.finish(ev)
		if l.followed(p.q) {
			t.Fatalf("a FETCH after the PREPARE answered with event %d did not end the query", ev)
		}
	}
}

// The gateway follows a bounded number of queries per session. FETCHes of
// an adopted session for unknown uuids start following queries until the
// bound; beyond it the gateway refuses such a FETCH and a PREPARE instead of
// forgetting a query that may still run.
func TestQueriesPerSessionAreBounded(t *testing.T) {
	l, s := newTestLimits(t, true)
	var fetches []*queryRequest
	for i := 0; i < maxSessionQueries; i++ {
		u := wire.Hugeint{Upper: 7, Lower: uint64(i)}
		f, _, err := l.beginReading(context.Background(), s, &u, true, func() {})
		if f == nil || err != nil {
			t.Fatalf("FETCH %d: %v, %v", i, f, err)
		}
		fetches = append(fetches, f)
	}
	count := func() int {
		l.mu.Lock()
		defer l.mu.Unlock()
		return len(l.sessions[s.ID].queries)
	}
	if n := count(); n != maxSessionQueries {
		t.Fatalf("%d adopted queries followed, want %d", n, maxSessionQueries)
	}
	u := wire.Hugeint{Upper: 7, Lower: maxSessionQueries}
	if f, _, err := l.beginReading(context.Background(), s, &u, true, func() {}); f != nil || !errors.Is(err, errTooManyQueries) {
		t.Fatalf("FETCH beyond the bound: %v, %v; want errTooManyQueries", f, err)
	}
	if r, err := l.beginPrepare(context.Background(), s, wire.OptionalIdx{}, wire.Hugeint{Upper: 8}, true, false, func() {}, nil); r != nil || !errors.Is(err, errTooManyQueries) {
		t.Fatalf("PREPARE beyond the bound: %v, %v; want errTooManyQueries", r, err)
	}
	if n := count(); n != maxSessionQueries {
		t.Fatalf("%d queries followed after the refusals, want %d", n, maxSessionQueries)
	}
	fetches[0].finish(evError) // "Result has been closed": the engine never had it
	r := l.testPrepare(t, s, wire.Hugeint{Upper: 8})
	r.finish(evResult) // replaces all of them
	if n := count(); n != 1 {
		t.Fatalf("%d queries followed after the PREPARE's result, want 1", n)
	}
}

// A client cannot make the gateway forget the query that runs by piling up
// PREPAREs that the engine got whole but did not answer (the engine never
// makes a malformed one its statement): at the bound the gateway refuses
// further PREPAREs. It forgets only queries the engine is done with, here
// PREPAREs it cut off at their limit.
func TestRunningQueryIsNeverForgotten(t *testing.T) {
	l, s := newTestLimits(t, false)
	running := l.testPrepare(t, s, wire.Hugeint{Upper: 1})
	running.finish(evResult)
	u := running.q.uuid
	fetch, _, err := l.beginReading(context.Background(), s, &u, true, func() {})
	if fetch == nil || err != nil {
		t.Fatalf("FETCH: %v, %v", fetch, err)
	}
	var piled []*trackedQuery
	for i := 1; i < maxSessionQueries; i++ {
		r := l.testPrepare(t, s, wire.Hugeint{Upper: 2, Lower: uint64(i)})
		r.finish(evFailed) // the client went away; the body counts as sent (testPrepare has none)
		piled = append(piled, r.q)
	}
	r, err := l.beginPrepare(context.Background(), s, wire.OptionalIdx{}, wire.Hugeint{Upper: 3}, true, false, func() {}, nil)
	if r != nil || !errors.Is(err, errTooManyQueries) {
		t.Fatalf("PREPARE beyond the bound: %v, %v; want errTooManyQueries", r, err)
	}
	if !l.followed(running.q) {
		t.Fatal("the gateway forgot the query that runs")
	}
	for _, q := range piled {
		if !l.followed(q) {
			t.Fatal("the gateway forgot a PREPARE the engine may still run")
		}
	}

	// PREPAREs whose bodies the gateway cut off at their limit never reach
	// the engine: they make room.
	l2, s2 := newTestLimits(t, false)
	running = l2.testPrepare(t, s2, wire.Hugeint{Upper: 1})
	running.finish(evResult)
	for i := 1; i < maxSessionQueries; i++ {
		in := &countingReader{size: 100} // nothing of it sent yet
		r, err := l2.beginPrepare(context.Background(), s2, wire.OptionalIdx{}, wire.Hugeint{Upper: 2, Lower: uint64(i)}, true, false, func() {}, in)
		if r == nil || err != nil {
			t.Fatalf("PREPARE %d: %v, %v", i, r, err)
		}
		l2.mu.Lock()
		e := l2.startLocked(r.q)
		l2.mu.Unlock()
		if !e.cut {
			t.Fatalf("PREPARE %d was not cut off", i)
		}
		l2.run(r.q, e)
	}
	r = l2.testPrepare(t, s2, wire.Hugeint{Upper: 3})
	if !l2.followed(running.q) || !l2.followed(r.q) {
		t.Fatalf("followed: the query that ran %v, the new one %v; want both", l2.followed(running.q), l2.followed(r.q))
	}
	l2.mu.Lock()
	n := len(l2.sessions[s2.ID].queries)
	l2.mu.Unlock()
	if n != 2 {
		t.Fatalf("%d queries followed, want the two that may run", n)
	}
}

// A PREPARE that got no answer although the engine got all of it (a failure
// between gateway and engine; the client got an error for it) is cancelled
// at its limit as a precaution. No client sees a timeout then, so the
// gateway neither counts nor logs one, whatever the CANCEL brings; it does
// once a request of the query waits. A statement without a limit (COMMIT)
// that gets no answer is not followed any further.
func TestUnansweredPrepareIsCancelledQuietly(t *testing.T) {
	for _, waiting := range []bool{false, true} {
		l, s := newTestLimits(t, false)
		var logs bytes.Buffer
		l.g.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		l.g.met = metrics.New()
		r := l.testPrepare(t, s, wire.Hugeint{})
		r.finish(evFailed)
		// A later PREPARE: a CANCEL with the zero uuid could meet it, so the
		// gateway cannot send one, and the enforcement fails without an engine.
		l.testPrepare(t, s, wire.Hugeint{Upper: 2})
		if waiting {
			var zero wire.Hugeint
			if f, _, err := l.beginReading(context.Background(), s, &zero, true, func() {}); f == nil || f.q != r.q || err != nil {
				t.Fatalf("FETCH: %v, %v", f, err)
			}
		}
		l.mu.Lock()
		e := l.startLocked(r.q)
		l.mu.Unlock()
		l.run(r.q, e)
		warned := strings.Contains(logs.String(), "level=WARN")
		counted := testCounter(t, l.g.met, "quackmux_query_timeouts_total") > 0
		if warned != waiting || counted != waiting {
			t.Errorf("request waiting %v: logged a warning %v, counted %v; want %v", waiting, warned, counted, waiting)
		}
	}

	l, s := newTestLimits(t, false)
	r, err := l.beginPrepare(context.Background(), s, wire.OptionalIdx{}, wire.Hugeint{}, true, true, func() {}, nil)
	if r == nil || err != nil {
		t.Fatalf("COMMIT: %v, %v", r, err)
	}
	r.finish(evFailed)
	if l.followed(r.q) {
		t.Fatal("the gateway still follows a COMMIT that got no answer")
	}
}

// testCounter sums the counter name in the registry of met.
func testCounter(t *testing.T, met *metrics.Metrics, name string) float64 {
	t.Helper()
	mfs, err := met.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var n float64
	for _, mf := range mfs {
		if mf.GetName() == name {
			for _, m := range mf.GetMetric() {
				n += m.GetCounter().GetValue()
			}
		}
	}
	return n
}

// COMMIT, END, ROLLBACK and ABORT (with WORK or TRANSACTION) have no limit;
// anything longer or different has one.
func TestEndsTransaction(t *testing.T) {
	for sql, want := range map[string]bool{
		"COMMIT": true, "commit;": true, " END TRANSACTION ; ": true, "ROLLBACK WORK": true, "abort": true,
		"Rollback Transaction": true,
		"":                     false, "BEGIN TRANSACTION": false, "COMMIT; SELECT 1": false, "COMMIT;;": false,
		"COMMIT AND CHAIN": false, "SELECT 1": false, "COMMIT/**/": false, "ROLLBACK TO SAVEPOINT a": false,
		"COMMIT ": false, fmt.Sprintf("COMMIT %64s", ""): false,
	} {
		body := wire.PrepareRequest{SQL: sql}.Encode(wire.Header{})
		_, n, err := wire.ParseHeader(body)
		if err != nil {
			t.Fatal(err)
		}
		if got := endsTransaction(body[n:]); got != want {
			t.Errorf("endsTransaction(%q) = %v, want %v", sql, got, want)
		}
	}
}
