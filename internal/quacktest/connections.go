package quacktest

import (
	"sort"

	"quackmux/internal/wire"
)

// listConnections answers the gateway's listing of the engine's connections
// (wire.ActiveConnectionsSQL) like quack_active_connections() of the Quack
// server: one VARCHAR column with the id of every connection the engine
// holds, the asking one and those with an expired lease included. It reports
// false for every other message.
func (s *Server) listConnections(h wire.Header, body []byte, rec *Received) ([]byte, bool) {
	if h.Type != wire.TypePrepareRequest {
		return nil, false
	}
	p, err := wire.DecodePrepareRequestBody(body)
	if err != nil || p.SQL != wire.ActiveConnectionsSQL {
		return nil, false
	}
	rec.SQL = p.SQL
	s.mu.Lock()
	reject := s.listingError
	ids := make([]string, 0, len(s.conns))
	for id := range s.conns {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	if reject != "" {
		return errorResponse("%s", reject), true
	}
	sort.Strings(ids)
	return wire.VarcharResult{
		Names:     []string{"connection_id"},
		Columns:   []wire.VarcharColumn{{Values: ids}},
		QueryUUID: p.QueryUUID,
	}.Encode(), true
}

// SetListingError makes the listing of the connections fail with msg, like
// an authorization function that rejects it; "" restores the answer.
func (s *Server) SetListingError(msg string) {
	s.mu.Lock()
	s.listingError = msg
	s.mu.Unlock()
}

// Connect opens a connection like a client handshake would, without a
// client, and returns its id (a connection whose client died).
func (s *Server) Connect() string {
	resp, _ := s.handle(wire.ConnectionRequest{
		AuthString: s.Token, MinSupportedVersion: wire.ProtocolVersion, MaxSupportedVersion: wire.ProtocolVersion,
		HeartbeatTimeoutSeconds: 60,
	}.Encode(wire.Header{}), nil)
	h, _, err := wire.ParseHeader(resp)
	if err != nil || h.Type != wire.TypeConnectionResponse {
		panic("quacktest: the fake engine refused its own handshake")
	}
	return h.ConnectionID
}
