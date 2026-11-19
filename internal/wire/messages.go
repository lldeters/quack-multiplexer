package wire

import (
	"fmt"
	"sort"
	"strconv"
)

// ProtocolVersion is the Quack wire-protocol version this package speaks
// (QUACK_VERSION in duckdb-quack). Client and server must agree exactly.
const ProtocolVersion = 3

// ContentType is the media type the DuckDB Quack server answers with.
const ContentType = "application/vnd.duckdb"

// MessageType enumerates Quack messages (enum class MessageType : uint8_t).
type MessageType uint8

const (
	TypeInvalid            MessageType = 0
	TypeConnectionRequest  MessageType = 1
	TypeConnectionResponse MessageType = 2
	TypePrepareRequest     MessageType = 3
	TypePrepareResponse    MessageType = 4
	TypeFetchRequest       MessageType = 7
	TypeFetchResponse      MessageType = 8
	TypeSendDataRequest    MessageType = 9
	TypeSuccessResponse    MessageType = 10
	TypeDisconnect         MessageType = 11
	TypeCancelRequest      MessageType = 12
	TypeSendDataResponse   MessageType = 14
	TypeAcknowledgement    MessageType = 15
	TypeHeartbeatRequest   MessageType = 16
	TypeErrorResponse      MessageType = 100
)

var typeNames = map[MessageType]string{
	TypeInvalid:            "INVALID",
	TypeConnectionRequest:  "CONNECTION_REQUEST",
	TypeConnectionResponse: "CONNECTION_RESPONSE",
	TypePrepareRequest:     "PREPARE_REQUEST",
	TypePrepareResponse:    "PREPARE_RESPONSE",
	TypeFetchRequest:       "FETCH_REQUEST",
	TypeFetchResponse:      "FETCH_RESPONSE",
	TypeSendDataRequest:    "SEND_DATA_REQUEST",
	TypeSuccessResponse:    "SUCCESS_RESPONSE",
	TypeDisconnect:         "DISCONNECT_MESSAGE",
	TypeCancelRequest:      "CANCEL_REQUEST",
	TypeSendDataResponse:   "SEND_DATA_RESPONSE",
	TypeAcknowledgement:    "ACKNOWLEDGEMENT",
	TypeHeartbeatRequest:   "HEARTBEAT_REQUEST",
	TypeErrorResponse:      "ERROR_RESPONSE",
}

func (t MessageType) String() string {
	if n, ok := typeNames[t]; ok {
		return n
	}
	return "UNKNOWN_" + strconv.Itoa(int(t))
}

// IsClientRequest reports whether a Quack server accepts this message type
// from a client (ServerSupportsMessage in quack_server.cpp).
func (t MessageType) IsClientRequest() bool {
	switch t {
	case TypeConnectionRequest, TypePrepareRequest, TypeFetchRequest, TypeSendDataRequest,
		TypeDisconnect, TypeCancelRequest, TypeAcknowledgement, TypeHeartbeatRequest:
		return true
	}
	return false
}

// Hugeint is DuckDB's hugeint_t (used for query uuids).
type Hugeint struct {
	Upper int64
	Lower uint64
}

// IsZero reports the {0,0} value. Quack uses it for internal catalog traffic
// and as the "cancel whatever runs" wildcard.
func (h Hugeint) IsZero() bool { return h.Upper == 0 && h.Lower == 0 }

func (h Hugeint) String() string {
	return fmt.Sprintf("%016x%016x", uint64(h.Upper), h.Lower)
}

// OptionalIdx is DuckDB's optional_idx.
type OptionalIdx struct {
	Valid bool
	Value uint64
}

// SomeIdx returns a valid OptionalIdx.
func SomeIdx(v uint64) OptionalIdx { return OptionalIdx{Valid: true, Value: v} }

// Header is the first object of every Quack message.
type Header struct {
	Type          MessageType
	ConnectionID  string
	ClientQueryID OptionalIdx
}

// Encode serializes the header object exactly like MessageHeader::Serialize.
func (h Header) Encode() []byte {
	var e encoder
	h.encodeTo(&e)
	return e.bytes()
}

func (h Header) encodeTo(e *encoder) {
	e.field(1)
	e.uvarint(uint64(h.Type))
	e.optString(2, h.ConnectionID)
	e.field(3)
	e.optionalIdx(h.ClientQueryID)
	e.endObject()
}

// ParseHeader decodes the header object at the start of msg. It returns the
// header and the number of bytes it occupies, so the caller can splice a new
// header in front of the untouched remainder. ErrTruncated means msg does not
// yet contain the whole header.
func ParseHeader(msg []byte) (Header, int, error) {
	d := newDecoder(msg)
	var h Header
	if err := d.expectField(1); err != nil {
		return h, 0, err
	}
	t, err := d.uint8Value()
	if err != nil {
		return h, 0, err
	}
	h.Type = MessageType(t)
	if h.ConnectionID, err = d.optString(2); err != nil {
		return h, 0, err
	}
	if err := d.expectField(3); err != nil {
		return h, 0, err
	}
	if h.ClientQueryID, err = d.optionalIdx(); err != nil {
		return h, 0, err
	}
	if err := d.endObject(); err != nil {
		return h, 0, err
	}
	return h, d.off, nil
}

// ConnectionRequest is the handshake a client sends first
// (ConnectionRequestMessage).
type ConnectionRequest struct {
	AuthString              string
	ClientDuckDBVersion     string
	ClientPlatform          string
	MinSupportedVersion     uint64
	MaxSupportedVersion     uint64
	ClientID                string
	HeartbeatTimeoutSeconds uint64
}

// SupportsVersion reports whether the client's advertised range includes v.
func (c ConnectionRequest) SupportsVersion(v uint64) bool {
	return c.MinSupportedVersion <= v && v <= c.MaxSupportedVersion
}

// DecodeConnectionRequest decodes a complete CONNECTION_REQUEST message.
func DecodeConnectionRequest(msg []byte) (Header, ConnectionRequest, error) {
	h, n, err := ParseHeader(msg)
	if err != nil {
		return h, ConnectionRequest{}, err
	}
	if h.Type != TypeConnectionRequest {
		return h, ConnectionRequest{}, malformed("expected CONNECTION_REQUEST, got %s", h.Type)
	}
	d := newDecoder(msg[n:])
	var c ConnectionRequest
	if c.AuthString, err = d.optString(1); err != nil {
		return h, c, err
	}
	if c.ClientDuckDBVersion, err = d.optString(2); err != nil {
		return h, c, err
	}
	if c.ClientPlatform, err = d.optString(3); err != nil {
		return h, c, err
	}
	if c.MinSupportedVersion, err = d.optUvarint(4); err != nil {
		return h, c, err
	}
	if c.MaxSupportedVersion, err = d.optUvarint(5); err != nil {
		return h, c, err
	}
	if c.ClientID, err = d.optString(6); err != nil {
		return h, c, err
	}
	if c.HeartbeatTimeoutSeconds, err = d.optUvarint(7); err != nil {
		return h, c, err
	}
	if err := d.endObject(); err != nil {
		return h, c, err
	}
	if d.remaining() != 0 {
		return h, c, malformed("%d unexpected trailing bytes after CONNECTION_REQUEST", d.remaining())
	}
	return h, c, nil
}

// Encode serializes a complete CONNECTION_REQUEST message (header + body).
func (c ConnectionRequest) Encode(h Header) []byte {
	h.Type = TypeConnectionRequest
	var e encoder
	h.encodeTo(&e)
	e.optString(1, c.AuthString)
	e.optString(2, c.ClientDuckDBVersion)
	e.optString(3, c.ClientPlatform)
	e.optUvarint(4, c.MinSupportedVersion)
	e.optUvarint(5, c.MaxSupportedVersion)
	e.optString(6, c.ClientID)
	e.optUvarint(7, c.HeartbeatTimeoutSeconds)
	e.endObject()
	return e.bytes()
}

// ConnectionResponse is the server's handshake answer. The new connection id
// travels in the response header.
type ConnectionResponse struct {
	ServerDuckDBVersion     string
	ServerPlatform          string
	QuackVersion            uint64
	HeartbeatTimeoutSeconds uint64
}

// DecodeConnectionResponse decodes a complete CONNECTION_RESPONSE message.
func DecodeConnectionResponse(msg []byte) (Header, ConnectionResponse, error) {
	h, n, err := ParseHeader(msg)
	if err != nil {
		return h, ConnectionResponse{}, err
	}
	if h.Type != TypeConnectionResponse {
		return h, ConnectionResponse{}, malformed("expected CONNECTION_RESPONSE, got %s", h.Type)
	}
	d := newDecoder(msg[n:])
	var c ConnectionResponse
	if c.ServerDuckDBVersion, err = d.optString(1); err != nil {
		return h, c, err
	}
	if c.ServerPlatform, err = d.optString(2); err != nil {
		return h, c, err
	}
	if c.QuackVersion, err = d.optUvarint(3); err != nil {
		return h, c, err
	}
	if c.HeartbeatTimeoutSeconds, err = d.optUvarint(4); err != nil {
		return h, c, err
	}
	if err := d.endObject(); err != nil {
		return h, c, err
	}
	return h, c, nil
}

// Encode serializes a complete CONNECTION_RESPONSE message.
func (c ConnectionResponse) Encode(h Header) []byte {
	h.Type = TypeConnectionResponse
	var e encoder
	h.encodeTo(&e)
	e.optString(1, c.ServerDuckDBVersion)
	e.optString(2, c.ServerPlatform)
	e.optUvarint(3, c.QuackVersion)
	e.optUvarint(4, c.HeartbeatTimeoutSeconds)
	e.endObject()
	return e.bytes()
}

// Exception type names as produced by Exception::ExceptionTypeToString. The
// client maps unknown names to INVALID_INPUT, so these are best effort.
const (
	ExceptionInvalidInput = "Invalid Input"
	ExceptionPermission   = "Permission"
	ExceptionConnection   = "Connection"
	ExceptionIO           = "IO"
	ExceptionInterrupt    = "INTERRUPT"
)

// ErrorResponse is the error message a server returns (always with HTTP 200).
type ErrorResponse struct {
	Message       string
	ExceptionType string
	ExtraInfo     map[string]string
	// MustInvalidate tells the client that the attached database behind this
	// connection is gone; the client then refuses further use until the
	// database is DETACHed and ATTACHed again.
	MustInvalidate bool
}

// Encode serializes a complete ERROR_RESPONSE message.
func (r ErrorResponse) Encode() []byte {
	var e encoder
	Header{Type: TypeErrorResponse}.encodeTo(&e)
	e.optString(1, r.Message)
	e.optString(2, r.ExceptionType)
	e.stringMap(3, r.ExtraInfo)
	e.optBool(4, r.MustInvalidate)
	e.endObject()
	return e.bytes()
}

// DecodeErrorResponse decodes the body of an ERROR_RESPONSE message.
func DecodeErrorResponse(msg []byte) (ErrorResponse, error) {
	h, n, err := ParseHeader(msg)
	if err != nil {
		return ErrorResponse{}, err
	}
	if h.Type != TypeErrorResponse {
		return ErrorResponse{}, malformed("expected ERROR_RESPONSE, got %s", h.Type)
	}
	d := newDecoder(msg[n:])
	var r ErrorResponse
	if r.Message, err = d.optString(1); err != nil {
		return r, err
	}
	if r.ExceptionType, err = d.optString(2); err != nil {
		return r, err
	}
	if ok, err := d.optionalField(3); err != nil {
		return r, err
	} else if ok {
		if r.ExtraInfo, err = d.stringMap(); err != nil {
			return r, err
		}
	}
	if r.MustInvalidate, err = d.optBool(4); err != nil {
		return r, err
	}
	if err := d.endObject(); err != nil {
		return r, err
	}
	return r, nil
}

// PrepareRequest is the body of PREPARE_REQUEST (a query).
type PrepareRequest struct {
	SQL        string
	QueryUUID  Hugeint
	InlineRows OptionalIdx
}

// DecodePrepareRequestBody decodes a PREPARE_REQUEST body (the bytes after
// the header).
func DecodePrepareRequestBody(body []byte) (PrepareRequest, error) {
	d := newDecoder(body)
	var p PrepareRequest
	var err error
	if p.SQL, err = d.optString(1); err != nil {
		return p, err
	}
	if err := d.expectField(2); err != nil {
		return p, err
	}
	if p.QueryUUID, err = d.hugeint(); err != nil {
		return p, err
	}
	if p.InlineRows, err = d.optOptionalIdx(3); err != nil {
		return p, err
	}
	if err := d.endObject(); err != nil {
		return p, err
	}
	return p, nil
}

// Encode serializes a complete PREPARE_REQUEST message.
func (p PrepareRequest) Encode(h Header) []byte {
	h.Type = TypePrepareRequest
	var e encoder
	h.encodeTo(&e)
	e.optString(1, p.SQL)
	e.field(2)
	e.hugeint(p.QueryUUID)
	e.optOptionalIdx(3, p.InlineRows)
	e.endObject()
	return e.bytes()
}

// PeekPrepareSQL extracts the beginning of the SQL text of a PREPARE_REQUEST
// body without requiring the complete body. It returns at most limit bytes of
// the statement and whether the statement was cut.
func PeekPrepareSQL(body []byte, limit int) (sql string, truncated bool, err error) {
	d := newDecoder(body)
	ok, err := d.optionalField(1)
	if err != nil || !ok {
		return "", false, err
	}
	n, err := d.uvarint()
	if err != nil {
		return "", false, err
	}
	avail := d.remaining()
	take := int(min(n, uint64(avail)))
	if limit >= 0 && take > limit {
		take = limit
	}
	return string(d.buf[d.off : d.off+take]), uint64(take) < n, nil
}

// FetchRequest is the body of FETCH_REQUEST.
type FetchRequest struct {
	QueryUUID  Hugeint
	BatchIndex uint64
	AckIndex   uint64
}

// DecodeFetchRequestBody decodes a FETCH_REQUEST body.
func DecodeFetchRequestBody(body []byte) (FetchRequest, error) {
	d := newDecoder(body)
	var f FetchRequest
	var err error
	if err = d.expectField(1); err != nil {
		return f, err
	}
	if f.QueryUUID, err = d.hugeint(); err != nil {
		return f, err
	}
	if f.BatchIndex, err = d.optUvarint(2); err != nil {
		return f, err
	}
	if f.AckIndex, err = d.optUvarint(3); err != nil {
		return f, err
	}
	return f, d.endObject()
}

// Encode serializes a complete FETCH_REQUEST message.
func (f FetchRequest) Encode(h Header) []byte {
	h.Type = TypeFetchRequest
	var e encoder
	h.encodeTo(&e)
	e.field(1)
	e.hugeint(f.QueryUUID)
	e.optUvarint(2, f.BatchIndex)
	e.optUvarint(3, f.AckIndex)
	e.endObject()
	return e.bytes()
}

// EncodeCancel serializes a CANCEL_REQUEST. A zero uuid cancels whatever query
// currently runs on the connection.
func EncodeCancel(connectionID string, queryUUID Hugeint) []byte {
	var e encoder
	Header{Type: TypeCancelRequest, ConnectionID: connectionID}.encodeTo(&e)
	e.field(1)
	e.hugeint(queryUUID)
	e.endObject()
	return e.bytes()
}

// EncodeEmpty serializes a message whose body has no fields (DISCONNECT,
// HEARTBEAT, SUCCESS).
func EncodeEmpty(h Header) []byte {
	var e encoder
	h.encodeTo(&e)
	e.endObject()
	return e.bytes()
}

// EncodeAcknowledgement serializes an ACKNOWLEDGEMENT message.
func EncodeAcknowledgement(connectionID string, queryUUID Hugeint) []byte {
	var e encoder
	Header{Type: TypeAcknowledgement, ConnectionID: connectionID}.encodeTo(&e)
	if !queryUUID.IsZero() {
		e.field(1)
		e.hugeint(queryUUID)
	}
	e.endObject()
	return e.bytes()
}

// ReplaceHeader returns a copy of msg whose header object is replaced by h.
// Everything after the header (body object and raw blob) is kept verbatim.
func ReplaceHeader(msg []byte, h Header) ([]byte, error) {
	_, n, err := ParseHeader(msg)
	if err != nil {
		return nil, err
	}
	hdr := h.Encode()
	out := make([]byte, 0, len(hdr)+len(msg)-n)
	out = append(out, hdr...)
	return append(out, msg[n:]...), nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
