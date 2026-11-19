// Package upstream is the gateway's HTTP client towards engines.
package upstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"quackmux/internal/config"
	"quackmux/internal/wire"
)

// Client sends Quack messages to engines. It keeps a pool of keep-alive
// connections per engine, like the DuckDB client does towards a server.
type Client struct {
	HTTP           *http.Client
	controlTimeout time.Duration
	maxResponse    int64
}

// New builds a client from the upstream configuration.
func New(cfg config.UpstreamConfig) *Client {
	dialer := &net.Dialer{Timeout: cfg.DialTimeout.Duration, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy:                 nil, // engines are reached directly, never through HTTP_PROXY
		DialContext:           dialer.DialContext,
		MaxIdleConns:          0, // unlimited in total, bounded per host below
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerEngine,
		IdleConnTimeout:       cfg.IdleConnTimeout.Duration,
		ResponseHeaderTimeout: cfg.ResponseHeaderTimeout.Duration,
		DisableCompression:    true,
		ForceAttemptHTTP2:     false,
		WriteBufferSize:       64 << 10,
		ReadBufferSize:        64 << 10,
	}
	return &Client{
		HTTP:           &http.Client{Transport: tr},
		controlTimeout: cfg.ControlTimeout.Duration,
		maxResponse:    16 << 20,
	}
}

// QuackURL returns the Quack endpoint of an engine base URL.
func QuackURL(endpoint string) string {
	return strings.TrimSuffix(endpoint, "/") + "/quack"
}

// NewRequest builds a POST /quack request with the Quack content type.
func NewRequest(ctx context.Context, endpoint string, body io.Reader, contentLength int64) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, QuackURL(endpoint), body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = contentLength
	req.Header.Set("Content-Type", wire.ContentType)
	req.Header.Set("User-Agent", "quackmux")
	return req, nil
}

// ErrStatus reports a non-200 answer from an engine.
var ErrStatus = errors.New("engine answered with unexpected HTTP status")

// RoundTrip sends a complete small message and returns the complete answer.
// It is meant for handshakes and control messages, not for data transfer.
func (c *Client) RoundTrip(ctx context.Context, endpoint string, msg []byte) ([]byte, error) {
	req, err := NewRequest(ctx, endpoint, bytes.NewReader(msg), int64(len(msg)))
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%w %d", ErrStatus, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResponse+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > c.maxResponse {
		return nil, fmt.Errorf("engine response exceeds %d bytes", c.maxResponse)
	}
	return body, nil
}

// Control sends a gateway-originated control message (DISCONNECT, CANCEL,
// HEARTBEAT) with the control timeout. It returns the decoded error if the
// engine answered with ERROR_RESPONSE.
func (c *Client) Control(ctx context.Context, endpoint string, msg []byte) error {
	ctx, cancel := context.WithTimeout(ctx, c.controlTimeout)
	defer cancel()
	body, err := c.RoundTrip(ctx, endpoint, msg)
	if err != nil {
		return err
	}
	h, _, err := wire.ParseHeader(body)
	if err != nil {
		return fmt.Errorf("undecodable engine response: %w", err)
	}
	if h.Type == wire.TypeErrorResponse {
		e, err := wire.DecodeErrorResponse(body)
		if err != nil {
			return fmt.Errorf("undecodable engine error: %w", err)
		}
		return &EngineError{Response: e}
	}
	return nil
}

// Disconnect closes a backend connection.
func (c *Client) Disconnect(ctx context.Context, endpoint, backendConnID string) error {
	return c.Control(ctx, endpoint, wire.EncodeEmpty(wire.Header{Type: wire.TypeDisconnect, ConnectionID: backendConnID}))
}

// Heartbeat renews the lease of a backend connection.
func (c *Client) Heartbeat(ctx context.Context, endpoint, backendConnID string) error {
	return c.Control(ctx, endpoint, wire.EncodeEmpty(wire.Header{Type: wire.TypeHeartbeatRequest, ConnectionID: backendConnID}))
}

// CancelAll cancels whatever query runs on a backend connection.
func (c *Client) CancelAll(ctx context.Context, endpoint, backendConnID string) error {
	return c.Control(ctx, endpoint, wire.EncodeCancel(backendConnID, wire.Hugeint{}))
}

// Probe checks that an engine's HTTP endpoint answers GET /.
func (c *Client) Probe(ctx context.Context, endpoint string) error {
	ctx, cancel := context.WithTimeout(ctx, c.controlTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(endpoint, "/")+"/", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w %d", ErrStatus, resp.StatusCode)
	}
	return nil
}

// EngineError is an ERROR_RESPONSE returned by an engine.
type EngineError struct {
	Response wire.ErrorResponse
}

func (e *EngineError) Error() string {
	return "engine error: " + e.Response.Message
}

// Messages the stock Quack server uses when it does not know (or no longer
// knows) a connection. The gateway treats them as "session gone".
const (
	MsgInvalidConnection = "Invalid connection id"
	MsgLeaseExpired      = "Connection heartbeat lease expired"
	MsgAlreadyClosed     = "Connection does not exist / already disconnected"
)

// IsSessionGone reports whether an engine error means the backend connection
// no longer exists.
func IsSessionGone(msg string) bool {
	return strings.Contains(msg, MsgInvalidConnection) || strings.Contains(msg, MsgLeaseExpired) ||
		strings.Contains(msg, MsgAlreadyClosed)
}
