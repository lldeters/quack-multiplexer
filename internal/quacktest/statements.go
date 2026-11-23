package quacktest

import (
	"time"

	"quackmux/internal/wire"
)

// Like the Quack server, the fake engine keeps one statement per
// connection: a PREPARE_REQUEST replaces the statement before it (and
// interrupts it with Server.Supersede), CANCEL_REQUEST interrupts it, and
// FETCH_REQUEST reads the batches of a STREAM statement. Requests that wait
// for an interrupted statement get its error, like those of the real server
// (quack_server.cpp, AbortStatement).

// Texts of the server's errors around a statement.
const (
	MsgCancelled     = "Interrupted: query was cancelled"
	MsgSuperseded    = "superseded by a new query"
	MsgResultClosed  = "Result has been closed"
	MsgNoBatchIndex  = "FETCH_REQUEST is missing its batch index"
	msgDisconnected  = "connection closed during fetch"
	msgCancelOtherFn = "Attempted to cancel a different query with id '%s' instead of '%s'"
)

// statement is the statement of a connection. Its fields are guarded by
// Server.mu.
type statement struct {
	uuid    wire.Hugeint
	started time.Time
	// batches and every describe a STREAM statement: batch i (from 1) is
	// ready at started + i*every.
	stream  bool
	batches uint64
	every   time.Duration
	// done is closed when the statement is interrupted, with reason.
	done    chan struct{}
	aborted bool
	reason  string
}

// replaceStatement makes a new statement the connection's, interrupting the
// one before it.
func (s *Server) replaceStatement(c *conn, uuid wire.Hugeint) *statement {
	st := &statement{uuid: uuid, started: time.Now(), done: make(chan struct{})}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.stmt != nil && s.Supersede {
		abortLocked(c.stmt, MsgSuperseded)
	}
	c.stmt = st
	return st
}

func abortLocked(st *statement, reason string) {
	if !st.aborted {
		st.aborted, st.reason = true, reason
		close(st.done)
	}
}

func interrupted(reason string) []byte {
	return wire.ErrorResponse{Message: reason, ExceptionType: wire.ExceptionInterrupt}.Encode()
}

// sleep blocks for d or until st is interrupted; it returns the error to
// answer with in that case.
func (s *Server) sleep(st *statement, d time.Duration) []byte {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-st.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return interrupted(st.reason)
	}
}

// cancel answers a CANCEL_REQUEST: the zero uuid interrupts whatever runs,
// another uuid only the statement it names.
func (s *Server) cancel(c *conn, body []byte, rec *Received) []byte {
	u, err := wire.LeadingQueryUUID(body)
	if err != nil {
		return errorResponse("Failed to deserialize: %v", err)
	}
	rec.QueryUUID = u
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.CancelError != "" {
		return errorResponse("%s", s.CancelError)
	}
	var current wire.Hugeint
	if c.stmt != nil {
		current = c.stmt.uuid
	}
	if !u.IsZero() && u != current {
		return errorResponse(msgCancelOtherFn, u, current)
	}
	if c.stmt != nil {
		abortLocked(c.stmt, MsgCancelled)
	}
	return wire.EncodeEmpty(wire.Header{Type: wire.TypeSuccessResponse})
}

// fetch answers a FETCH_REQUEST. It waits until the batch of a STREAM
// statement is ready; beyond the last batch it waits for the whole result
// and answers with the terminal FETCH_RESPONSE.
func (s *Server) fetch(c *conn, body []byte, rec *Received) []byte {
	f, err := wire.DecodeFetchRequestBody(body)
	if err != nil {
		return errorResponse("Failed to deserialize: %v", err)
	}
	rec.QueryUUID = f.QueryUUID
	s.mu.Lock()
	st := c.stmt
	if st == nil || st.uuid != f.QueryUUID {
		s.mu.Unlock()
		return errorResponse(MsgResultClosed)
	}
	if st.aborted {
		s.mu.Unlock()
		return interrupted(st.reason)
	}
	s.mu.Unlock()
	if f.BatchIndex == 0 {
		return errorResponse(MsgNoBatchIndex)
	}
	if !st.stream {
		// The whole result came with the PREPARE_RESPONSE.
		return wire.FetchResponseHead{TotalBatches: wire.SomeIdx(0)}.Encode(wire.Header{})
	}
	ready := min(f.BatchIndex, st.batches)
	if err := s.sleep(st, time.Until(st.started.Add(time.Duration(ready)*st.every))); err != nil {
		return err
	}
	if f.BatchIndex > st.batches {
		return wire.FetchResponseHead{TotalBatches: wire.SomeIdx(st.batches)}.Encode(wire.Header{})
	}
	// One chunk of filler: the gateway passes the blob on untouched.
	head := wire.FetchResponseHead{ChunkCount: 1, BatchIndex: wire.SomeIdx(f.BatchIndex)}.Encode(wire.Header{})
	return append(head, 0x64, 0x00, 0x01, 0xff, 0xff)
}

// endStatement interrupts the statement of a connection that goes away.
func (s *Server) endStatement(c *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.stmt != nil {
		abortLocked(c.stmt, msgDisconnected)
	}
}
