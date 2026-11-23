package wire

import "encoding/binary"

// What the gateway reads to follow a query (the query time limit): the query
// uuid of PREPARE_REQUEST, FETCH_REQUEST and CANCEL_REQUEST, and the head of
// FETCH_RESPONSE.
//
// The uuid decoders follow DuckDB's EncodingUtil exactly, not the stricter
// codec of this package: a value may use more bytes than necessary, and bits
// beyond its width are dropped. A client must not be able to send a uuid the
// engine accepts but the gateway cannot read.

// PrepareUUIDScanner finds the query uuid of a PREPARE_REQUEST while its body
// (the bytes after the header) passes through in pieces of any size. The uuid
// follows the SQL text, which can be large, so a proxy that streams the body
// cannot look it up in a buffer. Write the body to the scanner; UUID reports
// the uuid once its bytes have passed. The scanner keeps only a few bytes.
//
// The body starts {1: sql_query (omitted when empty), 2: query_uuid, ...}
// (PrepareRequestMessage::Serialize).
type PrepareUUIDScanner struct {
	state   uuidScanState
	pending []byte // bytes of the field id or varint being read
	skip    uint64 // bytes of the SQL text still to pass
	upper   int64
	uuid    Hugeint
	err     error
}

type uuidScanState uint8

const (
	scanFirstField uuidScanState = iota // field 1 (sql_query) or 2 (query_uuid)
	scanSQLLength
	scanSQL
	scanUUIDField
	scanUpper
	scanLower
	scanDone
	scanFailed
)

// Write consumes the next bytes of the body. It never fails: an undecodable
// body ends the scan (see Err), and bytes after the uuid are ignored.
func (s *PrepareUUIDScanner) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && s.state != scanDone && s.state != scanFailed {
		if s.state == scanSQL {
			k := min(uint64(len(p)), s.skip)
			p = p[k:]
			if s.skip -= k; s.skip == 0 {
				s.state = scanUUIDField
			}
			continue
		}
		b := p[0]
		p = p[1:]
		s.pending = append(s.pending, b)
		switch s.state {
		case scanFirstField, scanUUIDField:
			if len(s.pending) < 2 {
				continue
			}
			id := binary.LittleEndian.Uint16(s.pending)
			s.pending = s.pending[:0]
			switch {
			case id == 2:
				s.state = scanUpper
			case id == 1 && s.state == scanFirstField:
				s.state = scanSQLLength
			default:
				s.fail(malformed("PREPARE_REQUEST: unexpected field id %d", id))
			}
		case scanSQLLength, scanUpper, scanLower:
			bits := uint(64)
			if s.state == scanSQLLength {
				bits = 32 // ReadString reads a uint32 length
			}
			if b&0x80 != 0 {
				if len(s.pending) >= lebMaxBytes(bits) {
					s.fail(malformed("PREPARE_REQUEST: LEB128 value too long"))
				}
				continue
			}
			v := lebValue(s.pending, bits, s.state == scanUpper)
			s.pending = s.pending[:0]
			switch s.state {
			case scanSQLLength:
				if v == 0 {
					s.state = scanUUIDField
				} else {
					s.skip, s.state = v, scanSQL
				}
			case scanUpper:
				s.upper, s.state = int64(v), scanLower
			default:
				s.uuid, s.state, s.pending = Hugeint{Upper: s.upper, Lower: v}, scanDone, nil
			}
		}
	}
	return n, nil
}

func (s *PrepareUUIDScanner) fail(err error) {
	s.state, s.err, s.pending = scanFailed, err, nil
}

// UUID returns the query uuid once its bytes have passed.
func (s *PrepareUUIDScanner) UUID() (Hugeint, bool) {
	return s.uuid, s.state == scanDone
}

// Err reports a body that cannot be the start of a PREPARE_REQUEST body; the
// scan has ended then.
func (s *PrepareUUIDScanner) Err() error { return s.err }

// PrepareQueryUUID returns the query uuid of a PREPARE_REQUEST body (the bytes
// after the header) without copying the SQL text. ErrTruncated means that the
// body ends before the uuid.
func PrepareQueryUUID(body []byte) (Hugeint, error) {
	var s PrepareUUIDScanner
	_, _ = s.Write(body)
	if u, ok := s.UUID(); ok {
		return u, nil
	}
	if err := s.Err(); err != nil {
		return Hugeint{}, err
	}
	return Hugeint{}, ErrTruncated
}

// LeadingQueryUUID returns the query uuid that a FETCH_REQUEST or
// CANCEL_REQUEST body (the bytes after the header) starts with: both write it
// as their mandatory first field. ErrTruncated means that the body ends before
// the uuid.
func LeadingQueryUUID(body []byte) (Hugeint, error) {
	if len(body) < 2 {
		return Hugeint{}, ErrTruncated
	}
	if id := binary.LittleEndian.Uint16(body); id != 1 {
		return Hugeint{}, malformed("query uuid: field id mismatch, expected 1, got %d", id)
	}
	upper, n, err := lebNext(body[2:], 64, true)
	if err != nil {
		return Hugeint{}, err
	}
	lower, _, err := lebNext(body[2+n:], 64, false)
	if err != nil {
		return Hugeint{}, err
	}
	return Hugeint{Upper: int64(upper), Lower: lower}, nil
}

// lebMaxBytes is the most bytes DuckDB reads for a LEB128 value of a type
// with bits bits: one more makes it fail ("data may be corrupt").
func lebMaxBytes(bits uint) int { return int(bits+6) / 7 }

// lebNext decodes the LEB128 value at the start of p (see lebValue) and
// returns it with the number of bytes it took.
func lebNext(p []byte, bits uint, signed bool) (uint64, int, error) {
	for i, b := range p {
		if i >= lebMaxBytes(bits) {
			break
		}
		if b&0x80 == 0 {
			return lebValue(p[:i+1], bits, signed), i + 1, nil
		}
	}
	if len(p) < lebMaxBytes(bits) {
		return 0, 0, ErrTruncated
	}
	return 0, 0, malformed("LEB128 value too long")
}

// lebValue decodes the complete LEB128 value enc like DuckDB's
// EncodingUtil::DecodeUnsignedLEB128 and DecodeSignedLEB128 for a type of
// bits bits (32 or 64): bits beyond the width are dropped, and a signed value
// is sign-extended from its last byte. Signed results are returned as their
// two's complement bit pattern.
func lebValue(enc []byte, bits uint, signed bool) uint64 {
	var v uint64
	var shift uint
	for _, b := range enc {
		v |= uint64(b&0x7f) << shift
		shift += 7
	}
	if signed && shift < bits && enc[len(enc)-1]&0x40 != 0 {
		v |= ^uint64(0) << shift
	}
	if bits < 64 {
		v &= 1<<bits - 1
	}
	return v
}

// FetchResponseHead is the body object of a FETCH_RESPONSE; the raw blob of
// ChunkCount DataChunks follows it (FetchResponseMessage::Serialize).
type FetchResponseHead struct {
	ChunkCount   uint64
	TotalBatches OptionalIdx
	BatchIndex   OptionalIdx
}

// Terminal reports the answer to a FETCH beyond the last batch of a result
// that is complete: it carries no chunks, only the number of batches, and
// the client stops fetching when it gets one.
func (f FetchResponseHead) Terminal() bool { return f.ChunkCount == 0 }

// DecodeFetchResponseHead decodes the header and the body object of a
// FETCH_RESPONSE message. msg may end anywhere after the body object: the
// blob is not read.
func DecodeFetchResponseHead(msg []byte) (FetchResponseHead, error) {
	h, n, err := ParseHeader(msg)
	if err != nil {
		return FetchResponseHead{}, err
	}
	if h.Type != TypeFetchResponse {
		return FetchResponseHead{}, malformed("expected FETCH_RESPONSE, got %s", h.Type)
	}
	d := newDecoder(msg[n:])
	var f FetchResponseHead
	if f.ChunkCount, err = d.optUvarint(1); err != nil {
		return f, err
	}
	if f.TotalBatches, err = d.optOptionalIdx(2); err != nil {
		return f, err
	}
	if f.BatchIndex, err = d.optOptionalIdx(3); err != nil {
		return f, err
	}
	return f, d.endObject()
}

// Encode serializes the header and body object of a FETCH_RESPONSE; the blob
// of ChunkCount DataChunks must follow.
func (f FetchResponseHead) Encode(h Header) []byte {
	h.Type = TypeFetchResponse
	var e encoder
	h.encodeTo(&e)
	e.optUvarint(1, f.ChunkCount)
	e.optOptionalIdx(2, f.TotalBatches)
	e.optOptionalIdx(3, f.BatchIndex)
	e.endObject()
	return e.bytes()
}
