package upstream

import (
	"context"
	"errors"
	"fmt"

	"quackmux/internal/wire"
)

// The listing of an engine's connections runs on a control connection of the
// gateway's own: the engine has no other way to tell what it holds.
const (
	// controlHeartbeatSeconds is the lease of a control connection. It is
	// closed right after the listing; should that fail, it expires (lazily,
	// like every lease of the Quack server).
	controlHeartbeatSeconds = 60
	// maxListedConnections is the inline row budget of the listing. DuckDB
	// v2.0.0 cannot list more than 2048 connections anyway (one vector).
	maxListedConnections = 1 << 16
)

// ErrIncompleteListing reports a listing the engine did not answer in full.
var ErrIncompleteListing = errors.New("the engine did not list its connections in full")

// Connections returns the ids of the connections an engine holds, its Quack
// connections with their lease expired but not yet collected included, and
// without the control connection it opens for the query (authenticated with
// the engine token) and closes again. An authorization function on the
// engine must let wire.ActiveConnectionsSQL through.
func (c *Client) Connections(ctx context.Context, endpoint, token string) ([]string, error) {
	hello := wire.ConnectionRequest{
		AuthString: token, ClientPlatform: "quackmux",
		MinSupportedVersion: wire.ProtocolVersion, MaxSupportedVersion: wire.ProtocolVersion,
		HeartbeatTimeoutSeconds: controlHeartbeatSeconds,
	}.Encode(wire.Header{})
	body, err := c.exchange(ctx, endpoint, hello)
	if err != nil {
		return nil, fmt.Errorf("control connection: %w", err)
	}
	h, cr, err := wire.DecodeConnectionResponse(body)
	self := h.ConnectionID
	if h.Type == wire.TypeConnectionResponse && self != "" {
		// The engine created the connection, even if the rest of its answer
		// cannot be decoded. Close it also after a cancelled ctx: it would
		// stay otherwise.
		defer func() { _ = c.Disconnect(context.WithoutCancel(ctx), endpoint, self) }()
	}
	if err != nil {
		return nil, fmt.Errorf("control connection: undecodable engine response: %w", err)
	}
	if self == "" {
		return nil, errors.New("control connection: the engine sent no connection id")
	}
	if cr.QuackVersion != wire.ProtocolVersion {
		return nil, fmt.Errorf("control connection: the engine speaks Quack version %d", cr.QuackVersion)
	}

	query := wire.PrepareRequest{SQL: wire.ActiveConnectionsSQL, InlineRows: wire.SomeIdx(maxListedConnections)}
	body, err = c.exchange(ctx, endpoint, query.Encode(wire.Header{ConnectionID: self}))
	if err != nil {
		return nil, err
	}
	res, err := wire.DecodeVarcharResult(body)
	if err != nil {
		return nil, fmt.Errorf("listing: %w", err)
	}
	if res.NeedsMoreFetch {
		return nil, ErrIncompleteListing
	}
	if len(res.Columns) != 1 || res.Names[0] != "connection_id" {
		return nil, fmt.Errorf("listing: unexpected columns %v", res.Names)
	}
	col := res.Columns[0]
	ids := make([]string, 0, len(col.Values))
	for i, id := range col.Values {
		if col.IsNull(i) || id == "" {
			return nil, errors.New("listing: a connection without an id")
		}
		if id != self {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// exchange sends a control message with the control timeout and returns the
// answer; an ERROR_RESPONSE becomes an *EngineError.
func (c *Client) exchange(ctx context.Context, endpoint string, msg []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.controlTimeout)
	defer cancel()
	body, err := c.RoundTrip(ctx, endpoint, msg)
	if err != nil {
		return nil, err
	}
	h, _, err := wire.ParseHeader(body)
	if err != nil {
		return nil, fmt.Errorf("undecodable engine response: %w", err)
	}
	if h.Type == wire.TypeErrorResponse {
		e, err := wire.DecodeErrorResponse(body)
		if err != nil {
			return nil, fmt.Errorf("undecodable engine error: %w", err)
		}
		return nil, &EngineError{Response: e}
	}
	return body, nil
}
