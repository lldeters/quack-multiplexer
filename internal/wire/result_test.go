package wire

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The listing of the orphan sweep as DuckDB v2.0.0 answers it: three
// connections, the control connection that asked among them.
func TestDecodeRealActiveConnections(t *testing.T) {
	msg := realFixture(t, "prepare-active-connections-response.bin")
	r, err := DecodeVarcharResult(msg)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"8E8611981BC5FC8F31D3E62D6941046F", "1BDC8D3D410553029C89D8498C097046", "17915F4A77DB49A42268416DCCAA4DE0"}
	if len(r.Names) != 1 || r.Names[0] != "connection_id" || r.Rows() != len(want) || r.NeedsMoreFetch || !r.QueryUUID.IsZero() {
		t.Fatalf("unexpected result %+v", r)
	}
	for i, id := range want {
		if r.Columns[0].Values[i] != id || r.Columns[0].IsNull(i) {
			t.Errorf("row %d: %q, want %q", i, r.Columns[0].Values[i], id)
		}
	}
	// The fake engine answers with the encoder: it must write what DuckDB
	// writes.
	if got := r.Encode(); !bytes.Equal(got, msg) {
		t.Errorf("re-encoded as\n%srecorded\n%s", hexDump(got), hexDump(msg))
	}
}

// Real results of other shapes: NULLs (a validity mask), a constant vector
// (1500 rows, more than the server merges), two chunks (2048 + 952 rows) and
// a result held back for FETCH (inline_rows = 0).
func TestDecodeRealVarcharResults(t *testing.T) {
	cases := []struct {
		fixture string
		rows    int
		more    bool
		row     func(i int) (string, bool) // value and NULL of row i
		flat    bool                       // the encoder reproduces the message
	}{
		{"prepare-varchar-nulls-response.bin", 7, false, func(i int) (string, bool) {
			if i%3 == 0 {
				return "", true
			}
			return fmt.Sprintf("v%d", i), false
		}, true},
		{"prepare-varchar-constant-response.bin", 1500, false, func(int) (string, bool) { return "quack", false }, false},
		{"prepare-varchar-chunks-response.bin", 3000, false, func(i int) (string, bool) { return fmt.Sprintf("row %d", i), false }, true},
		{"prepare-varchar-fetch-response.bin", 0, true, nil, true},
	}
	for _, c := range cases {
		msg := realFixture(t, c.fixture)
		r, err := DecodeVarcharResult(msg)
		if err != nil {
			t.Errorf("%s: %v", c.fixture, err)
			continue
		}
		if r.Rows() != c.rows || r.NeedsMoreFetch != c.more || len(r.Names) != 1 || r.QueryUUID != (Hugeint{Upper: 7, Lower: 9}) {
			t.Errorf("%s: %d rows (more %v), names %v, uuid %s", c.fixture, r.Rows(), r.NeedsMoreFetch, r.Names, r.QueryUUID)
			continue
		}
		for i := 0; i < c.rows; i++ {
			v, null := c.row(i)
			if got := r.Columns[0]; got.Values[i] != v || got.IsNull(i) != null {
				t.Errorf("%s: row %d is %q (NULL %v), want %q (NULL %v)", c.fixture, i, got.Values[i], got.IsNull(i), v, null)
				break
			}
		}
		if got := r.Encode(); c.flat && !bytes.Equal(got, msg) {
			t.Errorf("%s: re-encoded as\n%srecorded\n%s", c.fixture, hexDump(got), hexDump(msg))
		}
	}
}

// A result with a column of another type is refused, not guessed at.
func TestDecodeRealUnsupportedResult(t *testing.T) {
	_, err := DecodeVarcharResult(realFixture(t, "prepare-integer-response.bin"))
	if !errors.Is(err, ErrUnsupportedResult) {
		t.Fatalf("err %v, want ErrUnsupportedResult", err)
	}
}

// quack_active_connections() of DuckDB v2.0.0 fails beyond 2048 connections:
// the server writes every row into one vector of 2048.
func TestRealActiveConnectionsLimit(t *testing.T) {
	e, err := DecodeErrorResponse(realFixture(t, "error-active-connections-limit.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.Message, "out of range for vector with capacity 2048") || e.MustInvalidate {
		t.Fatalf("unexpected error %+v", e)
	}
}

func TestVarcharResultRoundTrip(t *testing.T) {
	many := make([]string, 5000)
	for i := range many {
		many[i] = strings.Repeat("x", i%7)
	}
	nulls := make([]bool, len(many))
	for i := range nulls {
		nulls[i] = i%129 == 0
	}
	for _, r := range []VarcharResult{
		{QueryUUID: Hugeint{Upper: -1, Lower: 3}},
		{Names: []string{"a"}, Columns: []VarcharColumn{{}}, NeedsMoreFetch: true},
		{Names: []string{"a", "b"}, Columns: []VarcharColumn{{Values: []string{"1", ""}}, {Values: []string{"", "ü"}, Null: []bool{true, false}}}},
		{Names: []string{"many"}, Columns: []VarcharColumn{{Values: many, Null: nulls}}},
	} {
		got, err := DecodeVarcharResult(r.Encode())
		if err != nil {
			t.Fatalf("%v", err)
		}
		if got.Rows() != r.Rows() || got.NeedsMoreFetch != r.NeedsMoreFetch || got.QueryUUID != r.QueryUUID || len(got.Names) != len(r.Names) {
			t.Fatalf("got %+v", got)
		}
		for c := range r.Columns {
			for i, v := range r.Columns[c].Values {
				null := r.Columns[c].IsNull(i)
				if null {
					v = "" // the value of a NULL row is not sent
				}
				if got.Columns[c].Values[i] != v || got.Columns[c].IsNull(i) != null {
					t.Fatalf("column %d row %d differs", c, i)
				}
			}
		}
	}
}

// Broken or unexpected messages fail with the error class the caller can
// tell apart, never with a panic or a wrong result.
func TestDecodeVarcharResultRejects(t *testing.T) {
	good := VarcharResult{Names: []string{"connection_id"}, Columns: []VarcharColumn{{Values: []string{"AB", "CDE"}}}}.Encode()
	for name, c := range map[string]struct {
		msg  []byte
		want error
	}{
		"not a prepare response": {EncodeEmpty(Header{Type: TypeSuccessResponse}), ErrMalformed},
		"truncated":              {good[:len(good)-3], ErrTruncated},
		"trailing bytes":         {append(append([]byte{}, good...), 0), ErrMalformed},
		// The second string claims one byte more than there is.
		"lengths beyond the data": {bytes.Replace(good, []byte{0x03, 0, 0, 0}, []byte{0x04, 0, 0, 0}, 1), ErrMalformed},
		// A dictionary vector (vector_type 3) in place of the flat one.
		"dictionary vector": {bytes.Replace(good, []byte{0x64, 0x00, 0x00, 0x6b, 0x00}, []byte{0x5a, 0x00, 0x03, 0x64, 0x00, 0x00, 0x6b, 0x00}, 1), ErrUnsupportedResult},
		// VARCHAR with type info (a collation, for example).
		"type info": {bytes.Replace(good, []byte{0x64, 0x00, 0x19, 0xff, 0xff}, []byte{0x64, 0x00, 0x19, 0x65, 0x00}, 1), ErrUnsupportedResult},
	} {
		if _, err := DecodeVarcharResult(c.msg); !errors.Is(err, c.want) {
			t.Errorf("%s: err %v, want %v", name, err, c.want)
		}
	}
}
