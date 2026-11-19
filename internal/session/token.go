package session

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"quackmux/internal/engine"
)

// The gateway hands every client a connection id of its own instead of the
// engine's. The id is a sealed (AES-256-GCM) description of the route, so
// that any gateway process holding the same key can route a request even if
// it has never seen the session before (gateway restart or leader fail-over).
//
//	"qm" + base64url(nonce[12] | ciphertext | tag[16])
//
// The nonce doubles as the session id (12 random bytes, never reused).
//
// The id must stay short: the DuckDB client writes the SEND_DATA header into
// a fixed 256-byte buffer and throws an InternalException (which invalidates
// the client's whole database) when 64 + len(connection_id) + 36 > 256, so a
// connection id must never exceed 156 bytes. The binary payload keeps ids at
// about 90 characters and at most MaxIDLength.
//
// The backend connection id is a bearer credential for the engine session, so
// it must not leak to the client in clear text; GCM also authenticates the id,
// so a client cannot forge a route to somebody else's engine.

const (
	idPrefix       = "qm"
	payloadVersion = 1
	// MaxUserBytes is the longest user name stored verbatim in an id; longer
	// names are stored as a digest (the session then shows "~<digest>" after
	// a gateway restart).
	MaxUserBytes = 40
	// MaxIDLength is the upper bound of the ids this package produces.
	MaxIDLength = 140
	// HardIDLimit is the client's limit (see above).
	HardIDLimit = 156
)

// ErrInvalidID reports a connection id that was not minted by a gateway with
// the current key (forged, corrupted, or minted under another key).
var ErrInvalidID = errors.New("invalid or foreign quackmux connection id")

// Route is the routing information sealed into a gateway connection id.
type Route struct {
	// SessionID is the gateway's session identifier: base64url of 12 random
	// bytes (see NewSessionID). It is also the GCM nonce.
	SessionID string
	// EngineUID identifies the engine (EngineUID of its id).
	EngineUID engine.UID
	// BackendConnID is the connection id the engine assigned.
	BackendConnID string
	// User is the authenticated user (possibly a digest, see MaxUserBytes).
	User string
	// CreatedUnix is the session creation time (seconds since epoch).
	CreatedUnix int64
	// HeartbeatTimeout is the client's heartbeat lease in seconds.
	HeartbeatTimeout uint64
}

// Sealer seals and opens gateway connection ids.
type Sealer struct {
	aead cipher.AEAD
}

// NewSealer derives an AES-256-GCM key from secret. The secret should hold at
// least 32 bytes of entropy and must be identical on all gateway replicas.
func NewSealer(secret []byte) (*Sealer, error) {
	if len(secret) < 16 {
		return nil, fmt.Errorf("session key must be at least 16 bytes, got %d", len(secret))
	}
	key := sha256.Sum256(append([]byte("quackmux/session-id/v1\x00"), secret...))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// Backend connection id encodings.
const (
	connRaw      = 0 // length-prefixed bytes
	connHexUpper = 1 // 32 upper-case hex digits stored as 16 bytes
	connHexLower = 2 // 32 lower-case hex digits stored as 16 bytes
)

func encodeConnID(b []byte, id string) ([]byte, error) {
	if len(id) == 32 {
		if raw, err := hex.DecodeString(id); err == nil {
			switch id {
			case strings.ToUpper(id):
				return append(append(b, connHexUpper), raw...), nil
			case strings.ToLower(id):
				return append(append(b, connHexLower), raw...), nil
			}
		}
	}
	if len(id) == 0 || len(id) > 64 {
		return nil, fmt.Errorf("backend connection id of %d bytes cannot be sealed", len(id))
	}
	b = append(b, connRaw, byte(len(id)))
	return append(b, id...), nil
}

// StoredUser returns the form of a user name that fits into an id.
func StoredUser(user string) string {
	if len(user) <= MaxUserBytes {
		return user
	}
	sum := sha256.Sum256([]byte(user))
	return "~" + hex.EncodeToString(sum[:12])
}

// Seal turns a route into a client-facing connection id.
func (s *Sealer) Seal(r Route) (string, error) {
	nonce, err := base64.RawURLEncoding.DecodeString(r.SessionID)
	if err != nil || len(nonce) != s.aead.NonceSize() {
		return "", fmt.Errorf("session id %q is not a sealable session id", r.SessionID)
	}
	user := StoredUser(r.User)
	p := make([]byte, 0, 64)
	p = append(p, payloadVersion)
	p = append(p, r.EngineUID[:]...)
	if p, err = encodeConnID(p, r.BackendConnID); err != nil {
		return "", err
	}
	p = binary.BigEndian.AppendUint32(p, uint32(r.CreatedUnix))
	hb := r.HeartbeatTimeout
	if hb > 0xFFFF {
		hb = 0xFFFF
	}
	p = binary.BigEndian.AppendUint16(p, uint16(hb))
	p = append(p, byte(len(user)))
	p = append(p, user...)

	sealed := s.aead.Seal(nonce[:len(nonce):len(nonce)], nonce, p, []byte(idPrefix))
	id := idPrefix + base64.RawURLEncoding.EncodeToString(sealed)
	if len(id) > HardIDLimit {
		return "", fmt.Errorf("connection id of %d bytes exceeds the client limit of %d", len(id), HardIDLimit)
	}
	return id, nil
}

// Open recovers the route from a connection id produced by Seal.
func (s *Sealer) Open(id string) (Route, error) {
	var r Route
	if !strings.HasPrefix(id, idPrefix) || len(id) > HardIDLimit {
		return r, ErrInvalidID
	}
	raw, err := base64.RawURLEncoding.DecodeString(id[len(idPrefix):])
	if err != nil {
		return r, ErrInvalidID
	}
	ns := s.aead.NonceSize()
	if len(raw) < ns+s.aead.Overhead() {
		return r, ErrInvalidID
	}
	p, err := s.aead.Open(nil, raw[:ns], raw[ns:], []byte(idPrefix))
	if err != nil {
		return r, ErrInvalidID
	}
	r.SessionID = base64.RawURLEncoding.EncodeToString(raw[:ns])
	// version(1) uid(8) kind(1) ...
	if len(p) < 1+8+1 || p[0] != payloadVersion {
		return r, ErrInvalidID
	}
	copy(r.EngineUID[:], p[1:9])
	rest := p[9:]
	switch rest[0] {
	case connHexUpper, connHexLower:
		if len(rest) < 17 {
			return r, ErrInvalidID
		}
		h := hex.EncodeToString(rest[1:17])
		if rest[0] == connHexUpper {
			h = strings.ToUpper(h)
		}
		r.BackendConnID = h
		rest = rest[17:]
	case connRaw:
		if len(rest) < 2 || len(rest) < 2+int(rest[1]) {
			return r, ErrInvalidID
		}
		r.BackendConnID = string(rest[2 : 2+int(rest[1])])
		rest = rest[2+int(rest[1]):]
	default:
		return r, ErrInvalidID
	}
	if len(rest) < 4+2+1 {
		return r, ErrInvalidID
	}
	r.CreatedUnix = int64(binary.BigEndian.Uint32(rest))
	r.HeartbeatTimeout = uint64(binary.BigEndian.Uint16(rest[4:]))
	n := int(rest[6])
	if len(rest) != 7+n {
		return r, ErrInvalidID
	}
	r.User = string(rest[7:])
	if r.BackendConnID == "" {
		return r, ErrInvalidID
	}
	return r, nil
}

// NewSessionID returns a random session identifier (12 random bytes,
// base64url encoded: 16 characters).
func NewSessionID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
