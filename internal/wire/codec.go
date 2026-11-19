// Package wire implements the subset of DuckDB's Quack wire protocol (protocol
// version 3, DuckDB 2.0) that a routing gateway needs: message headers, the
// connection handshake, error responses and the small control messages.
//
// Every Quack HTTP body is a sequence of DuckDB BinarySerializer objects:
//
//	header object | body object | optional raw chunk blob
//
// An object is a list of properties terminated by the field id 0xFFFF. A
// property is a little-endian uint16 field id followed by its value. Unsigned
// integers are ULEB128, signed integers SLEB128, strings are ULEB128(length)
// followed by the bytes, booleans are one raw byte, hugeint_t is
// SLEB128(upper) followed by ULEB128(lower). Properties written with
// WritePropertyWithDefault are omitted entirely when they hold their default
// value, so a decoder must peek at the next field id before reading.
//
// The authoritative definitions live in duckdb-quack
// (src/include/quack_message.json, src/serialize_quack_message.cpp) and in
// DuckDB's src/common/serializer/binary_serializer.cpp.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

const (
	// terminatorField ends every serialized object.
	terminatorField uint16 = 0xFFFF
	// invalidIndex is DConstants::INVALID_INDEX; it encodes an unset optional_idx.
	invalidIndex uint64 = math.MaxUint64
	// maxStringLen mirrors DuckDB, which writes string lengths as uint32.
	maxStringLen = math.MaxUint32
)

// ErrTruncated reports that the input ended before a complete value could be
// decoded. Callers that read a request incrementally can treat it as "need
// more bytes".
var ErrTruncated = errors.New("quack wire: truncated message")

// ErrMalformed reports bytes that cannot be a valid Quack message.
var ErrMalformed = errors.New("quack wire: malformed message")

func malformed(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformed, fmt.Sprintf(format, args...))
}

// decoder is a cursor over a byte slice that mirrors BinaryDeserializer,
// including its one-field look-ahead buffer.
type decoder struct {
	buf []byte
	off int
}

func newDecoder(b []byte) *decoder { return &decoder{buf: b} }

func (d *decoder) remaining() int { return len(d.buf) - d.off }

func (d *decoder) peekField() (uint16, error) {
	if d.remaining() < 2 {
		return 0, ErrTruncated
	}
	return binary.LittleEndian.Uint16(d.buf[d.off:]), nil
}

func (d *decoder) nextField() (uint16, error) {
	id, err := d.peekField()
	if err != nil {
		return 0, err
	}
	d.off += 2
	return id, nil
}

// expectField consumes a mandatory property (WriteProperty / ReadProperty).
func (d *decoder) expectField(id uint16) error {
	got, err := d.nextField()
	if err != nil {
		return err
	}
	if got != id {
		return malformed("field id mismatch, expected %d, got %d", id, got)
	}
	return nil
}

// optionalField consumes the field id if the next property is `id`
// (WritePropertyWithDefault / ReadPropertyWithDefault).
func (d *decoder) optionalField(id uint16) (bool, error) {
	got, err := d.peekField()
	if err != nil {
		return false, err
	}
	if got != id {
		return false, nil
	}
	d.off += 2
	return true, nil
}

// endObject consumes the 0xFFFF terminator.
func (d *decoder) endObject() error {
	got, err := d.nextField()
	if err != nil {
		return err
	}
	if got != terminatorField {
		return malformed("expected end of object, but found field id %d", got)
	}
	return nil
}

// uvarint decodes ULEB128 into a uint64, rejecting encodings DuckDB rejects
// (more than 10 bytes, or bits beyond 64).
func (d *decoder) uvarint() (uint64, error) {
	var result uint64
	var shift uint
	for {
		if d.remaining() < 1 {
			return 0, ErrTruncated
		}
		b := d.buf[d.off]
		d.off++
		if shift >= 64 {
			return 0, malformed("ULEB128 integer too large")
		}
		if shift == 63 && b&0x7E != 0 {
			return 0, malformed("ULEB128 integer overflows uint64")
		}
		result |= uint64(b&0x7F) << shift
		shift += 7
		if b&0x80 == 0 {
			return result, nil
		}
	}
}

// svarint decodes SLEB128 into an int64.
func (d *decoder) svarint() (int64, error) {
	var result uint64
	var shift uint
	var b byte
	for {
		if d.remaining() < 1 {
			return 0, ErrTruncated
		}
		b = d.buf[d.off]
		d.off++
		if shift >= 64 {
			return 0, malformed("SLEB128 integer too large")
		}
		result |= uint64(b&0x7F) << shift
		shift += 7
		if b&0x80 == 0 {
			break
		}
	}
	if shift < 64 && b&0x40 != 0 {
		result |= ^uint64(0) << shift
	}
	return int64(result), nil
}

func (d *decoder) uint8Value() (uint8, error) {
	v, err := d.uvarint()
	if err != nil {
		return 0, err
	}
	if v > math.MaxUint8 {
		return 0, malformed("value %d does not fit uint8", v)
	}
	return uint8(v), nil
}

func (d *decoder) str() (string, error) {
	n, err := d.uvarint()
	if err != nil {
		return "", err
	}
	if n > maxStringLen {
		return "", malformed("string length %d exceeds uint32", n)
	}
	if uint64(d.remaining()) < n {
		return "", ErrTruncated
	}
	s := string(d.buf[d.off : d.off+int(n)])
	d.off += int(n)
	return s, nil
}

func (d *decoder) boolean() (bool, error) {
	if d.remaining() < 1 {
		return false, ErrTruncated
	}
	b := d.buf[d.off]
	d.off++
	return b != 0, nil
}

func (d *decoder) hugeint() (Hugeint, error) {
	upper, err := d.svarint()
	if err != nil {
		return Hugeint{}, err
	}
	lower, err := d.uvarint()
	if err != nil {
		return Hugeint{}, err
	}
	return Hugeint{Upper: upper, Lower: lower}, nil
}

func (d *decoder) optionalIdx() (OptionalIdx, error) {
	v, err := d.uvarint()
	if err != nil {
		return OptionalIdx{}, err
	}
	if v == invalidIndex {
		return OptionalIdx{}, nil
	}
	return SomeIdx(v), nil
}

// Optional ("with default") property helpers. They return the zero value when
// the property is absent, exactly like ReadPropertyWithDefault.

func (d *decoder) optString(id uint16) (string, error) {
	ok, err := d.optionalField(id)
	if err != nil || !ok {
		return "", err
	}
	return d.str()
}

func (d *decoder) optUvarint(id uint16) (uint64, error) {
	ok, err := d.optionalField(id)
	if err != nil || !ok {
		return 0, err
	}
	return d.uvarint()
}

func (d *decoder) optBool(id uint16) (bool, error) {
	ok, err := d.optionalField(id)
	if err != nil || !ok {
		return false, err
	}
	return d.boolean()
}

func (d *decoder) optOptionalIdx(id uint16) (OptionalIdx, error) {
	ok, err := d.optionalField(id)
	if err != nil || !ok {
		return OptionalIdx{}, err
	}
	return d.optionalIdx()
}

// stringMap decodes an unordered_map<string,string>: a ULEB128 count followed
// by objects {0: key, 1: value}.
func (d *decoder) stringMap() (map[string]string, error) {
	n, err := d.uvarint()
	if err != nil {
		return nil, err
	}
	// Every entry needs at least 2+1+2+1+2 bytes; reject absurd counts early.
	if n > uint64(d.remaining())/8+1 {
		return nil, malformed("map entry count %d exceeds message size", n)
	}
	m := make(map[string]string, int(n))
	for i := uint64(0); i < n; i++ {
		if err := d.expectField(0); err != nil {
			return nil, err
		}
		k, err := d.str()
		if err != nil {
			return nil, err
		}
		if err := d.expectField(1); err != nil {
			return nil, err
		}
		v, err := d.str()
		if err != nil {
			return nil, err
		}
		if err := d.endObject(); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, nil
}

// encoder mirrors BinarySerializer.
type encoder struct {
	buf []byte
}

func (e *encoder) bytes() []byte { return e.buf }

func (e *encoder) field(id uint16) {
	e.buf = binary.LittleEndian.AppendUint16(e.buf, id)
}

func (e *encoder) endObject() { e.field(terminatorField) }

func (e *encoder) uvarint(v uint64) {
	for {
		b := byte(v & 0x7F)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		e.buf = append(e.buf, b)
		if v == 0 {
			return
		}
	}
}

func (e *encoder) svarint(v int64) {
	for {
		b := byte(v & 0x7F)
		v >>= 7 // arithmetic shift, like the C++ implementation
		if (v == 0 && b&0x40 == 0) || (v == -1 && b&0x40 != 0) {
			e.buf = append(e.buf, b)
			return
		}
		e.buf = append(e.buf, b|0x80)
	}
}

func (e *encoder) str(s string) {
	e.uvarint(uint64(len(s)))
	e.buf = append(e.buf, s...)
}

func (e *encoder) boolean(v bool) {
	if v {
		e.buf = append(e.buf, 1)
	} else {
		e.buf = append(e.buf, 0)
	}
}

func (e *encoder) hugeint(h Hugeint) {
	e.svarint(h.Upper)
	e.uvarint(h.Lower)
}

func (e *encoder) optionalIdx(o OptionalIdx) {
	if !o.Valid {
		e.uvarint(invalidIndex)
		return
	}
	e.uvarint(o.Value)
}

// "With default" writers: omit the property when it holds the default value.

func (e *encoder) optString(id uint16, s string) {
	if s == "" {
		return
	}
	e.field(id)
	e.str(s)
}

func (e *encoder) optUvarint(id uint16, v uint64) {
	if v == 0 {
		return
	}
	e.field(id)
	e.uvarint(v)
}

func (e *encoder) optBool(id uint16, v bool) {
	if !v {
		return
	}
	e.field(id)
	e.boolean(v)
}

func (e *encoder) optOptionalIdx(id uint16, o OptionalIdx) {
	if !o.Valid {
		return
	}
	e.field(id)
	e.optionalIdx(o)
}

func (e *encoder) stringMap(id uint16, m map[string]string) {
	if len(m) == 0 {
		return
	}
	e.field(id)
	e.uvarint(uint64(len(m)))
	// Deterministic order keeps encodings reproducible in tests and logs.
	for _, k := range sortedKeys(m) {
		e.field(0)
		e.str(k)
		e.field(1)
		e.str(m[k])
		e.endObject()
	}
}
