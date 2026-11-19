// Package quacktest provides an in-process fake Quack engine that implements
// the server side of the protocol closely enough to test the gateway: the
// connection handshake with token authentication, per-connection heartbeat
// leases, connection-scoped requests, DISCONNECT, and simple query results.
//
// Query behavior is scripted through the SQL text of PREPARE_REQUEST:
//
//	SLEEP <ms>      blocks the request for <ms> milliseconds
//	FAIL <message>  answers with an ERROR_RESPONSE
//	BLOB <bytes>    answers with a PREPARE_RESPONSE followed by <bytes> filler
//	                bytes (to exercise streaming of large bodies)
//	INVALIDATE      answers with an ERROR_RESPONSE that has must_invalidate set
//
// Everything else answers with an empty result.
package quacktest

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"quackmux/internal/wire"
)

// Banner is what the real server answers on GET /.
const Banner = "This is a DuckDB Quack RPC endpoint. Use ATTACH 'quack:...' to connect here.\n"

// Received records one request the fake engine handled.
type Received struct {
	Type         wire.MessageType
	ConnectionID string
	AuthString   string
	SQL          string
	Headers      http.Header
	BodyLen      int
}

type conn struct {
	id               string
	clientID         string
	heartbeatTimeout time.Duration
	lastRenewed      time.Time
	expired          bool
}

// HeartbeatTimeouts returns the lease each live connection negotiated.
func (s *Server) HeartbeatTimeouts() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]time.Duration, 0, len(s.conns))
	for _, c := range s.conns {
		out = append(out, c.heartbeatTimeout)
	}
	return out
}

// Server is a fake Quack engine.
type Server struct {
	Token string
	// Now can be replaced to control lease expiry in tests.
	Now func() time.Time

	mu       sync.Mutex
	conns    map[string]*conn
	received []Received
	closed   bool

	http *httptest.Server
}

// NewServer starts a fake engine that accepts the given token.
func NewServer(token string) *Server {
	s := &Server{Token: token, Now: time.Now, conns: map[string]*conn{}}
	s.http = httptest.NewServer(s)
	return s
}

// URL is the engine's base URL (without /quack).
func (s *Server) URL() string { return s.http.URL }

// Close stops the fake engine.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.http.CloseClientConnections()
	s.http.Close()
}

// Connections returns the ids of the live connections.
func (s *Server) Connections() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.conns))
	for id := range s.conns {
		ids = append(ids, id)
	}
	return ids
}

// Received returns a copy of all handled requests.
func (s *Server) Received() []Received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Received(nil), s.received...)
}

// CountType counts handled requests of a message type.
func (s *Server) CountType(t wire.MessageType) int {
	n := 0
	for _, r := range s.Received() {
		if r.Type == t {
			n++
		}
	}
	return n
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/":
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, Banner)
	case r.Method == http.MethodPost && r.URL.Path == "/quack":
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, trailer := s.handle(body, r.Header)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", wire.ContentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(resp)+trailer))
		_, _ = w.Write(resp)
		if trailer > 0 {
			chunk := bytes.Repeat([]byte{0xab}, 64*1024)
			for left := trailer; left > 0; {
				n := min(left, len(chunk))
				if _, err := w.Write(chunk[:n]); err != nil {
					return
				}
				left -= n
			}
		}
	default:
		http.NotFound(w, r)
	}
}

func errorResponse(format string, args ...any) []byte {
	return wire.ErrorResponse{Message: fmt.Sprintf(format, args...), ExceptionType: wire.ExceptionInvalidInput}.Encode()
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// handle answers one message. trailer is the number of filler bytes to append
// after the response (BLOB queries).
func (s *Server) handle(body []byte, headers http.Header) (resp []byte, trailer int) {
	h, n, err := wire.ParseHeader(body)
	if err != nil {
		return errorResponse("Failed to deserialize: %v", err), 0
	}
	rec := Received{Type: h.Type, ConnectionID: h.ConnectionID, Headers: headers.Clone(), BodyLen: len(body)}
	defer func() {
		s.mu.Lock()
		s.received = append(s.received, rec)
		s.mu.Unlock()
	}()
	if !h.Type.IsClientRequest() {
		return errorResponse("Unsupported message type for server"), 0
	}
	if h.Type == wire.TypeConnectionRequest {
		_, req, err := wire.DecodeConnectionRequest(body)
		if err != nil {
			return errorResponse("Failed to deserialize: %v", err), 0
		}
		rec.AuthString = req.AuthString
		if !req.SupportsVersion(wire.ProtocolVersion) {
			return errorResponse("Unsupported Quack version - server only supports version %d of quack", wire.ProtocolVersion), 0
		}
		if req.HeartbeatTimeoutSeconds == 0 {
			return errorResponse("heartbeat_timeout out of range"), 0
		}
		if req.AuthString != s.Token {
			return errorResponse("Authentication failed"), 0
		}
		id := randomID()
		s.mu.Lock()
		s.conns[id] = &conn{id: id, clientID: req.ClientID, heartbeatTimeout: time.Duration(req.HeartbeatTimeoutSeconds) * time.Second, lastRenewed: s.Now()}
		s.mu.Unlock()
		return wire.ConnectionResponse{
			ServerDuckDBVersion:     "v2.0.0",
			ServerPlatform:          "quacktest",
			QuackVersion:            wire.ProtocolVersion,
			HeartbeatTimeoutSeconds: req.HeartbeatTimeoutSeconds,
		}.Encode(wire.Header{ConnectionID: id}), 0
	}

	s.mu.Lock()
	c, ok := s.conns[h.ConnectionID]
	if !ok {
		s.mu.Unlock()
		return errorResponse("Invalid connection id"), 0
	}
	now := s.Now()
	if c.expired || now.Sub(c.lastRenewed) >= c.heartbeatTimeout {
		delete(s.conns, c.id)
		s.mu.Unlock()
		return errorResponse("Connection heartbeat lease expired"), 0
	}
	c.lastRenewed = now
	s.mu.Unlock()

	switch h.Type {
	case wire.TypeDisconnect:
		s.mu.Lock()
		delete(s.conns, h.ConnectionID)
		s.mu.Unlock()
		return wire.EncodeEmpty(wire.Header{Type: wire.TypeSuccessResponse}), 0
	case wire.TypeHeartbeatRequest, wire.TypeCancelRequest, wire.TypeAcknowledgement:
		return wire.EncodeEmpty(wire.Header{Type: wire.TypeSuccessResponse}), 0
	case wire.TypeSendDataRequest:
		// accept_budget is written with WriteProperty: an unset optional_idx.
		return append(wire.Header{Type: wire.TypeSendDataResponse}.Encode(),
			0x01, 0x00, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01, 0xff, 0xff), 0
	case wire.TypeFetchRequest:
		// Terminal fetch response: no chunks.
		return wire.EncodeEmpty(wire.Header{Type: wire.TypeFetchResponse}), 0
	case wire.TypePrepareRequest:
		p, err := wire.DecodePrepareRequestBody(body[n:])
		if err != nil {
			return errorResponse("Failed to deserialize: %v", err), 0
		}
		rec.SQL = p.SQL
		return s.runQuery(p)
	}
	return errorResponse("Unimplemented message type %s", h.Type), 0
}

func (s *Server) runQuery(p wire.PrepareRequest) ([]byte, int) {
	fields := strings.Fields(p.SQL)
	verb := ""
	if len(fields) > 0 {
		verb = strings.ToUpper(fields[0])
	}
	arg := func() int {
		if len(fields) < 2 {
			return 0
		}
		v, _ := strconv.Atoi(fields[1])
		return v
	}
	switch verb {
	case "SLEEP":
		time.Sleep(time.Duration(arg()) * time.Millisecond)
	case "FAIL":
		return errorResponse("%s", strings.TrimSpace(strings.TrimPrefix(p.SQL, fields[0]))), 0
	case "INVALIDATE":
		return wire.ErrorResponse{Message: "database invalidated", ExceptionType: "FATAL", MustInvalidate: true}.Encode(), 0
	case "BLOB":
		return prepareResponse(p.QueryUUID), arg()
	}
	return prepareResponse(p.QueryUUID), 0
}

// prepareResponse encodes an empty PREPARE_RESPONSE (no columns, no rows):
// fields 1-4 hold their defaults and are omitted, field 5 is mandatory.
func prepareResponse(uuid wire.Hugeint) []byte {
	msg := wire.Header{Type: wire.TypePrepareResponse}.Encode()
	msg = append(msg, 0x05, 0x00)
	msg = appendHugeint(msg, uuid)
	return append(msg, 0xff, 0xff)
}

func appendHugeint(b []byte, h wire.Hugeint) []byte {
	v := h.Upper
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if (v == 0 && c&0x40 == 0) || (v == -1 && c&0x40 != 0) {
			b = append(b, c)
			break
		}
		b = append(b, c|0x80)
	}
	u := h.Lower
	for {
		c := byte(u & 0x7f)
		u >>= 7
		if u != 0 {
			c |= 0x80
		}
		b = append(b, c)
		if u == 0 {
			return b
		}
	}
}

// ExpireAll marks every connection's lease as expired.
func (s *Server) ExpireAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		c.expired = true
	}
}
