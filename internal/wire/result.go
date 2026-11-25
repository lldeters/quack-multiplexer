package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ActiveConnectionsSQL is the one query the gateway sends on its own: after a
// restart it lists the connections of an engine to close the ones that no
// session adopted (package session, orphans.go). It selects nothing but the
// connection ids. They are bearer credentials for whoever reaches the
// engine's port directly, so the authorization macro of the engine image
// (images/engine/authorize.sql) lets exactly this text through only in its
// opt-in variant quackmux_authorize_listing, which fits engines whose port
// only the gateway reaches.
const ActiveConnectionsSQL = "SELECT connection_id FROM quack_active_connections()"

// The rows of a query come back inline in PREPARE_RESPONSE as DataChunks,
// written by DataChunk::Serialize and Vector::Serialize of DuckDB v2.0.0 with
// storage compatibility V2_0_0, like everything on the Quack wire
// (QuackWireSerializationOptions in quack_message.cpp). This file decodes
// results whose columns are all VARCHAR, which is all the gateway asks for;
// anything else fails with ErrUnsupportedResult instead of being guessed at.
//
// PREPARE_RESPONSE body (serialize_quack_message.cpp):
//
//	1 result_types      list of LogicalType, omitted when empty
//	2 result_names      list of strings, omitted when empty
//	3 needs_more_fetch  bool, omitted when false
//	4 results           list of unique_ptr<DataChunkWrapper>, omitted when empty
//	5 query_uuid        hugeint, always
//
// A list is a ULEB128 count and its elements; a unique_ptr element is a
// "present" byte and an object. DataChunkWrapper holds the chunk as the
// object of property 300 (quack_message.cpp). LogicalType is {100: id,
// 101: type_info}, without 101 for types without parameters (types.cpp).
// A DataChunk (data_chunk.cpp) is {100: rows, 101: list of LogicalType,
// 102: list of vector objects}. A flat VARCHAR vector (vector.cpp) is:
//
//	100 has_validity_mask  bool
//	101 validity           blob of ceil(rows/64) little-endian uint64 words,
//	                       bit i%64 of word i/64 set if row i is valid; only
//	                       with has_validity_mask
//	107 byte_data_length   optional_idx
//	108 length_data        blob of rows little-endian uint32 lengths (0 for NULL)
//	109 byte_data          blob of the valid strings, concatenated
//
// Blobs are a ULEB128 size and the bytes. A compressed vector starts with
// 90 vector_type: CONSTANT_VECTOR is followed by the flat fields of a single
// row. The server sends chunks of up to 2048 rows and merges chunks of fewer
// than 1024 rows into flat ones (QuackChunkStager); a larger chunk keeps its
// form. The other compressed forms (dictionary, sequence) are rejected.

// ErrUnsupportedResult reports a query result the decoder does not handle:
// a column that is not VARCHAR or a vector encoding it does not know.
var ErrUnsupportedResult = errors.New("quack wire: unsupported query result")

func unsupported(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsupportedResult, fmt.Sprintf(format, args...))
}

// typeIDVarchar is LogicalTypeId::VARCHAR (types.hpp).
const typeIDVarchar = 25

// VectorType values (vector_type.hpp).
const (
	vectorFlat     = 0
	vectorConstant = 2
)

// Bounds against absurd counts from a broken peer.
const (
	maxChunkRows  = 1 << 16
	maxResultRows = 1 << 20
	// standardVectorSize is the chunk size the encoder uses, like DuckDB.
	standardVectorSize = 2048
)

// VarcharColumn holds the values of one result column.
type VarcharColumn struct {
	Values []string
	// Null marks the NULL values (their entry in Values is ""); nil if
	// there are none.
	Null []bool
}

// IsNull reports whether row i is NULL.
func (c VarcharColumn) IsNull(i int) bool { return c.Null != nil && c.Null[i] }

// VarcharResult is a query result whose columns are all VARCHAR.
type VarcharResult struct {
	Names   []string
	Columns []VarcharColumn
	// NeedsMoreFetch is set when the engine holds back rows for FETCH: the
	// result is incomplete.
	NeedsMoreFetch bool
	QueryUUID      Hugeint
}

// Rows returns the number of rows.
func (r *VarcharResult) Rows() int {
	if len(r.Columns) == 0 {
		return 0
	}
	return len(r.Columns[0].Values)
}

// DecodeVarcharResult decodes a complete PREPARE_RESPONSE whose columns are
// all VARCHAR. It fails with ErrUnsupportedResult for other results, with
// ErrTruncated for an incomplete message and with ErrMalformed otherwise.
func DecodeVarcharResult(msg []byte) (*VarcharResult, error) {
	h, n, err := ParseHeader(msg)
	if err != nil {
		return nil, err
	}
	if h.Type != TypePrepareResponse {
		return nil, malformed("expected PREPARE_RESPONSE, got %s", h.Type)
	}
	d := newDecoder(msg[n:])
	r := &VarcharResult{}
	ncols := 0
	if ok, err := d.optionalField(1); err != nil {
		return nil, err
	} else if ok {
		if ncols, err = d.listCount(); err != nil {
			return nil, err
		}
		for i := 0; i < ncols; i++ {
			if err := d.varcharType(); err != nil {
				return nil, err
			}
		}
	}
	if ok, err := d.optionalField(2); err != nil {
		return nil, err
	} else if ok {
		cnt, err := d.listCount()
		if err != nil {
			return nil, err
		}
		for i := 0; i < cnt; i++ {
			s, err := d.str()
			if err != nil {
				return nil, err
			}
			r.Names = append(r.Names, s)
		}
	}
	if len(r.Names) != ncols {
		return nil, malformed("result with %d types but %d names", ncols, len(r.Names))
	}
	if r.NeedsMoreFetch, err = d.optBool(3); err != nil {
		return nil, err
	}
	r.Columns = make([]VarcharColumn, ncols)
	if ok, err := d.optionalField(4); err != nil {
		return nil, err
	} else if ok {
		cnt, err := d.listCount()
		if err != nil {
			return nil, err
		}
		for i := 0; i < cnt; i++ {
			present, err := d.boolean()
			if err != nil {
				return nil, err
			}
			if !present {
				return nil, malformed("result chunk %d is null", i)
			}
			if err := d.expectField(300); err != nil {
				return nil, err
			}
			if err := d.chunk(r.Columns); err != nil {
				return nil, err
			}
			if err := d.endObject(); err != nil { // DataChunkWrapper
				return nil, err
			}
			if r.Rows() > maxResultRows {
				return nil, malformed("result exceeds %d rows", maxResultRows)
			}
		}
	}
	if err := d.expectField(5); err != nil {
		return nil, err
	}
	if r.QueryUUID, err = d.hugeint(); err != nil {
		return nil, err
	}
	if err := d.endObject(); err != nil {
		return nil, err
	}
	if d.remaining() != 0 {
		return nil, malformed("%d unexpected trailing bytes after PREPARE_RESPONSE", d.remaining())
	}
	return r, nil
}

// listCount reads a list length. Every element takes at least one byte.
func (d *decoder) listCount() (int, error) {
	n, err := d.uvarint()
	if err != nil {
		return 0, err
	}
	if n > uint64(d.remaining()) {
		return 0, malformed("list of %d elements exceeds the message", n)
	}
	return int(n), nil
}

// blob reads a data blob that must hold exactly size bytes, like
// BinaryDeserializer::ReadDataPtr.
func (d *decoder) blob(size uint64) ([]byte, error) {
	n, err := d.uvarint()
	if err != nil {
		return nil, err
	}
	if n != size {
		return nil, malformed("blob of %d bytes, expected %d", n, size)
	}
	if uint64(d.remaining()) < n {
		return nil, ErrTruncated
	}
	b := d.buf[d.off : d.off+int(n)]
	d.off += int(n)
	return b, nil
}

// varcharType reads a LogicalType object and requires plain VARCHAR.
func (d *decoder) varcharType() error {
	if err := d.expectField(100); err != nil {
		return err
	}
	id, err := d.uint8Value()
	if err != nil {
		return err
	}
	if ok, err := d.optionalField(101); err != nil {
		return err
	} else if ok {
		return unsupported("type %d with type info", id)
	}
	if id != typeIDVarchar {
		return unsupported("column of type %d, only VARCHAR (%d) is supported", id, typeIDVarchar)
	}
	return d.endObject()
}

// chunk reads a DataChunk object (with its terminator) and appends its rows
// to cols.
func (d *decoder) chunk(cols []VarcharColumn) error {
	if err := d.expectField(100); err != nil {
		return err
	}
	rows64, err := d.uvarint()
	if err != nil {
		return err
	}
	if rows64 > maxChunkRows {
		return malformed("chunk of %d rows", rows64)
	}
	rows := int(rows64)
	if err := d.expectField(101); err != nil {
		return err
	}
	if n, err := d.listCount(); err != nil {
		return err
	} else if n != len(cols) {
		return malformed("chunk with %d types in a result with %d columns", n, len(cols))
	}
	for range cols {
		if err := d.varcharType(); err != nil {
			return err
		}
	}
	if err := d.expectField(102); err != nil {
		return err
	}
	if n, err := d.listCount(); err != nil {
		return err
	} else if n != len(cols) {
		return malformed("chunk with %d vectors in a result with %d columns", n, len(cols))
	}
	for i := range cols {
		vals, nulls, err := d.varcharVector(rows)
		if err != nil {
			return err
		}
		c := &cols[i]
		if nulls != nil && c.Null == nil {
			c.Null = make([]bool, len(c.Values), len(c.Values)+rows)
		}
		c.Values = append(c.Values, vals...)
		if c.Null != nil {
			if nulls == nil {
				nulls = make([]bool, rows)
			}
			c.Null = append(c.Null, nulls...)
		}
		if err := d.endObject(); err != nil { // vector object
			return err
		}
	}
	return d.endObject()
}

// varcharVector reads the fields of a VARCHAR vector of rows rows.
func (d *decoder) varcharVector(rows int) ([]string, []bool, error) {
	vtype := uint64(vectorFlat)
	if ok, err := d.optionalField(90); err != nil {
		return nil, nil, err
	} else if ok {
		if vtype, err = d.uvarint(); err != nil {
			return nil, nil, err
		}
	}
	switch vtype {
	case vectorFlat:
		return d.flatVarchar(rows)
	case vectorConstant:
		v, null, err := d.flatVarchar(1)
		if err != nil {
			return nil, nil, err
		}
		vals := make([]string, rows)
		for i := range vals {
			vals[i] = v[0]
		}
		var nulls []bool
		if null != nil && null[0] {
			nulls = make([]bool, rows)
			for i := range nulls {
				nulls[i] = true
			}
		}
		return vals, nulls, nil
	}
	return nil, nil, unsupported("vector type %d", vtype)
}

// flatVarchar reads the fields of a flat VARCHAR vector.
func (d *decoder) flatVarchar(rows int) ([]string, []bool, error) {
	if err := d.expectField(100); err != nil {
		return nil, nil, err
	}
	hasValidity, err := d.boolean()
	if err != nil {
		return nil, nil, err
	}
	var nulls []bool
	if hasValidity {
		if err := d.expectField(101); err != nil {
			return nil, nil, err
		}
		mask, err := d.blob(8 * ((uint64(rows) + 63) / 64))
		if err != nil {
			return nil, nil, err
		}
		nulls = make([]bool, rows)
		for i := range nulls {
			nulls[i] = binary.LittleEndian.Uint64(mask[8*(i/64):])&(1<<(uint(i)%64)) == 0
		}
	}
	if err := d.expectField(107); err != nil {
		return nil, nil, err
	}
	byteLen, err := d.optionalIdx()
	if err != nil {
		return nil, nil, err
	}
	if !byteLen.Valid {
		// Before V2_0_0 the strings were a list (property 102).
		return nil, nil, unsupported("string vector without byte_data_length")
	}
	if err := d.expectField(108); err != nil {
		return nil, nil, err
	}
	lengths, err := d.blob(4 * uint64(rows))
	if err != nil {
		return nil, nil, err
	}
	if err := d.expectField(109); err != nil {
		return nil, nil, err
	}
	data, err := d.blob(byteLen.Value)
	if err != nil {
		return nil, nil, err
	}
	vals := make([]string, rows)
	off := uint64(0)
	for i := range vals {
		if nulls != nil && nulls[i] {
			continue
		}
		l := uint64(binary.LittleEndian.Uint32(lengths[4*i:]))
		if l > uint64(len(data))-off {
			return nil, nil, malformed("string lengths exceed the string data")
		}
		vals[i] = string(data[off : off+l])
		off += l
	}
	if off != uint64(len(data)) {
		return nil, nil, malformed("%d bytes of string data that no row uses", uint64(len(data))-off)
	}
	return vals, nulls, nil
}

// Encode serializes the result as a complete PREPARE_RESPONSE the way a
// DuckDB v2.0.0 server writes flat VARCHAR vectors, in chunks of 2048 rows.
// The fake engine of package quacktest answers the gateway's own query with
// it.
func (r VarcharResult) Encode() []byte {
	var e encoder
	Header{Type: TypePrepareResponse}.encodeTo(&e)
	if len(r.Columns) > 0 {
		e.field(1)
		e.uvarint(uint64(len(r.Columns)))
		for range r.Columns {
			e.varcharType()
		}
		e.field(2)
		e.uvarint(uint64(len(r.Names)))
		for _, n := range r.Names {
			e.str(n)
		}
	}
	e.optBool(3, r.NeedsMoreFetch)
	if rows := r.Rows(); rows > 0 {
		e.field(4)
		e.uvarint(uint64((rows + standardVectorSize - 1) / standardVectorSize))
		for start := 0; start < rows; start += standardVectorSize {
			end := min(start+standardVectorSize, rows)
			e.boolean(true) // unique_ptr present
			e.field(300)
			e.field(100)
			e.uvarint(uint64(end - start))
			e.field(101)
			e.uvarint(uint64(len(r.Columns)))
			for range r.Columns {
				e.varcharType()
			}
			e.field(102)
			e.uvarint(uint64(len(r.Columns)))
			for _, c := range r.Columns {
				e.flatVarchar(c, start, end)
				e.endObject() // vector
			}
			e.endObject() // DataChunk
			e.endObject() // DataChunkWrapper
		}
	}
	e.field(5)
	e.hugeint(r.QueryUUID)
	e.endObject()
	return e.bytes()
}

func (e *encoder) varcharType() {
	e.field(100)
	e.uvarint(typeIDVarchar)
	e.endObject()
}

func (e *encoder) blob(b []byte) {
	e.uvarint(uint64(len(b)))
	e.buf = append(e.buf, b...)
}

// flatVarchar writes rows [start, end) of c like Vector::Serialize writes a
// flat VARCHAR vector whose validity mask exists only if a row is NULL.
func (e *encoder) flatVarchar(c VarcharColumn, start, end int) {
	rows := end - start
	hasNull := false
	for i := start; i < end; i++ {
		hasNull = hasNull || c.IsNull(i)
	}
	e.field(100)
	e.boolean(hasNull)
	if hasNull {
		// Like ValidityMask: every bit set, then the NULL rows cleared.
		mask := make([]byte, 8*((rows+63)/64))
		for i := range mask {
			mask[i] = 0xff
		}
		for i := 0; i < rows; i++ {
			if c.IsNull(start + i) {
				mask[i/8] &^= 1 << (uint(i) % 8)
			}
		}
		e.field(101)
		e.blob(mask)
	}
	lengths := make([]byte, 4*rows)
	var data []byte
	for i := 0; i < rows; i++ {
		if c.IsNull(start + i) {
			continue
		}
		v := c.Values[start+i]
		binary.LittleEndian.PutUint32(lengths[4*i:], uint32(len(v)))
		data = append(data, v...)
	}
	e.field(107)
	e.uvarint(uint64(len(data)))
	e.field(108)
	e.blob(lengths)
	e.field(109)
	e.blob(data)
}
