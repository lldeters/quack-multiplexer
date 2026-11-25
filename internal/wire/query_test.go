package wire

import (
	"errors"
	"math"
	"strings"
	"testing"
)

var testUUIDs = []Hugeint{
	{},
	{Upper: 1, Lower: 2},
	{Upper: -1, Lower: math.MaxUint64},
	{Upper: math.MinInt64, Lower: 0},
	{Upper: math.MaxInt64, Lower: 1 << 63},
	{Upper: -6066789547612528377, Lower: 12129815394120402617},
}

// prepareBody is the body (without header) of a PREPARE_REQUEST.
func prepareBody(t *testing.T, p PrepareRequest) []byte {
	t.Helper()
	msg := p.Encode(Header{ConnectionID: "c"})
	_, n, err := ParseHeader(msg)
	if err != nil {
		t.Fatal(err)
	}
	return msg[n:]
}

// The scanner finds the uuid behind SQL texts of any length, whatever pieces
// the body arrives in.
func TestPrepareUUIDScanner(t *testing.T) {
	for _, sqlLen := range []int{0, 1, 127, 128, 16383, 16384, 1 << 20} {
		for _, u := range testUUIDs {
			for _, inline := range []OptionalIdx{{}, SomeIdx(0), SomeIdx(24576)} {
				body := prepareBody(t, PrepareRequest{SQL: strings.Repeat("x", sqlLen), QueryUUID: u, InlineRows: inline})
				got, err := PrepareQueryUUID(body)
				if err != nil || got != u {
					t.Fatalf("SQL of %d bytes, uuid %v: got %v, %v", sqlLen, u, got, err)
				}
				for _, piece := range []int{1, 2, 3, 7, 4096} {
					if sqlLen > 20000 && piece < 4096 {
						continue
					}
					var s PrepareUUIDScanner
					for rest := body; len(rest) > 0; {
						k := min(piece, len(rest))
						if n, err := s.Write(rest[:k]); n != k || err != nil {
							t.Fatalf("Write = %d, %v", n, err)
						}
						rest = rest[k:]
					}
					if got, ok := s.UUID(); !ok || got != u || s.Err() != nil {
						t.Fatalf("SQL of %d bytes in pieces of %d: got %v, %v, %v", sqlLen, piece, got, ok, s.Err())
					}
				}
			}
		}
	}
}

// Before the uuid has passed, the scanner reports nothing; a body that ends
// there is truncated, not malformed.
func TestPrepareQueryUUIDTruncated(t *testing.T) {
	body := prepareBody(t, PrepareRequest{SQL: "SELECT 42", QueryUUID: Hugeint{Upper: 300, Lower: 70000}})
	uuidStart := 2 + 1 + len("SELECT 42") // field 1, length, text
	for n := 0; n < uuidStart+2+2+3; n++ {
		if _, err := PrepareQueryUUID(body[:n]); !errors.Is(err, ErrTruncated) {
			t.Fatalf("%d of %d bytes: %v, want ErrTruncated", n, len(body), err)
		}
	}
}

func TestPrepareQueryUUIDMalformed(t *testing.T) {
	for name, body := range map[string][]byte{
		"inline_rows first": {0x03, 0x00, 0x00, 0xff, 0xff},
		"sql_query twice":   {0x01, 0x00, 0x01, 'x', 0x01, 0x00, 0x00},
		"terminator first":  {0xff, 0xff},
		// DuckDB reads a string length as uint32: at most 5 bytes.
		"length of 6 bytes": {0x01, 0x00, 0x80, 0x80, 0x80, 0x80, 0x80, 0x00},
		// int64 and uint64: at most 10 bytes.
		"upper of 11 bytes": append([]byte{0x02, 0x00}, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x00, 0x00),
	} {
		if _, err := PrepareQueryUUID(body); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v, want ErrMalformed", name, err)
		}
	}
}

// Encodings DuckDB accepts although its own serializer never writes them:
// the gateway must read the same uuid the engine does.
func TestPrepareQueryUUIDLikeDuckDB(t *testing.T) {
	for name, c := range map[string]struct {
		body []byte
		want Hugeint
	}{
		// Non-minimal: 2 as 82 80 80 00, 5 as 85 00.
		"padded varints": {[]byte{0x02, 0x00, 0x82, 0x80, 0x80, 0x00, 0x85, 0x00}, Hugeint{Upper: 2, Lower: 5}},
		// A 5-byte string length: bits beyond 32 are dropped (5 + 1<<32 reads as 5).
		"length with high bits": {append([]byte{0x01, 0x00, 0x85, 0x80, 0x80, 0x80, 0x10}, "abcde\x02\x00\x01\x07"...), Hugeint{Upper: 1, Lower: 7}},
		// The 10th byte of a uint64 keeps only its lowest bit.
		"lower with high bits": {[]byte{0x02, 0x00, 0x00, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}, Hugeint{Lower: math.MaxUint64}},
		// A signed value is sign-extended from the last byte it has.
		"negative upper": {[]byte{0x02, 0x00, 0x7f, 0x00}, Hugeint{Upper: -1}},
		// Ten bytes: no sign extension; 0x7f << 63 keeps one bit.
		"upper of 10 bytes": {[]byte{0x02, 0x00, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x7f, 0x00}, Hugeint{Upper: math.MinInt64}},
	} {
		got, err := PrepareQueryUUID(c.body)
		if err != nil || got != c.want {
			t.Errorf("%s: got %v, %v, want %v", name, got, err, c.want)
		}
	}
}

func TestLeadingQueryUUID(t *testing.T) {
	for _, u := range testUUIDs {
		fetch := FetchRequest{QueryUUID: u, BatchIndex: 7, AckIndex: 6}.Encode(Header{ConnectionID: "c"})
		for _, msg := range [][]byte{fetch, EncodeCancel("c", u)} {
			_, n, err := ParseHeader(msg)
			if err != nil {
				t.Fatal(err)
			}
			body := msg[n:]
			if got, err := LeadingQueryUUID(body); err != nil || got != u {
				t.Fatalf("uuid %v: got %v, %v", u, got, err)
			}
			// Bodies that end inside the uuid are truncated.
			for k := 0; k < 4; k++ {
				if _, err := LeadingQueryUUID(body[:k]); !errors.Is(err, ErrTruncated) {
					t.Fatalf("%d bytes: %v, want ErrTruncated", k, err)
				}
			}
		}
	}
	if got, err := LeadingQueryUUID([]byte{0x01, 0x00, 0x83, 0x00, 0x84, 0x80, 0x00, 0xff, 0xff}); err != nil || got != (Hugeint{Upper: 3, Lower: 4}) {
		t.Fatalf("padded varints: got %v, %v", got, err)
	}
	if _, err := LeadingQueryUUID([]byte{0x02, 0x00, 0x01, 0x01, 0xff, 0xff}); !errors.Is(err, ErrMalformed) {
		t.Fatalf("batch index first: %v, want ErrMalformed", err)
	}
}

func TestFetchResponseHead(t *testing.T) {
	for _, f := range []FetchResponseHead{
		{ChunkCount: 1, BatchIndex: SomeIdx(1)},
		{ChunkCount: 147, BatchIndex: SomeIdx(300)},
		{TotalBatches: SomeIdx(1)},
		{TotalBatches: SomeIdx(0)},
		{},
	} {
		msg := append(f.Encode(Header{}), 0x64, 0x00, 0x01) // the start of a blob
		got, err := DecodeFetchResponseHead(msg)
		if err != nil || got != f {
			t.Fatalf("%+v: got %+v, %v", f, got, err)
		}
		if got.Terminal() != (f.ChunkCount == 0) {
			t.Fatalf("%+v: Terminal() = %v", f, got.Terminal())
		}
	}
	if _, err := DecodeFetchResponseHead(EncodeEmpty(Header{Type: TypeSuccessResponse})); !errors.Is(err, ErrMalformed) {
		t.Fatalf("SUCCESS_RESPONSE: %v, want ErrMalformed", err)
	}
	msg := FetchResponseHead{ChunkCount: 2, BatchIndex: SomeIdx(5)}.Encode(Header{})
	if _, err := DecodeFetchResponseHead(msg[:len(msg)-1]); !errors.Is(err, ErrTruncated) {
		t.Fatalf("cut head: %v, want ErrTruncated", err)
	}
}

// The recorded traffic of a real DuckDB client: the scanner and
// LeadingQueryUUID agree with the full decoders, and the heads of the
// recorded FETCH answers decode, data batches and the answer beyond the end.
func TestQueryUUIDsOfRealTraffic(t *testing.T) {
	prepares, leading, data, terminal := 0, 0, 0, 0
	for _, m := range loadRealTraffic(t) {
		h, n, err := ParseHeader(m.msg)
		if err != nil {
			continue
		}
		if h.Type == TypeFetchResponse {
			// Truncated recordings hold the head: it comes first.
			f, err := DecodeFetchResponseHead(m.msg)
			if err != nil {
				t.Fatalf("%s: %v", m.name, err)
			}
			if f.Terminal() {
				terminal++
			} else {
				data++
			}
			continue
		}
		if m.truncated {
			continue
		}
		switch h.Type {
		case TypePrepareRequest:
			p, err := DecodePrepareRequestBody(m.msg[n:])
			if err != nil {
				t.Fatalf("%s: %v", m.name, err)
			}
			if got, err := PrepareQueryUUID(m.msg[n:]); err != nil || got != p.QueryUUID {
				t.Errorf("%s: PrepareQueryUUID = %v, %v, want %v", m.name, got, err, p.QueryUUID)
			}
			prepares++
		case TypeFetchRequest, TypeCancelRequest:
			d := newDecoder(m.msg[n:])
			if err := d.expectField(1); err != nil {
				t.Fatalf("%s: %v", m.name, err)
			}
			want, err := d.hugeint()
			if err != nil {
				t.Fatalf("%s: %v", m.name, err)
			}
			if got, err := LeadingQueryUUID(m.msg[n:]); err != nil || got != want {
				t.Errorf("%s: LeadingQueryUUID = %v, %v, want %v", m.name, got, err, want)
			}
			leading++
		}
	}
	if prepares == 0 || leading == 0 || data == 0 || terminal == 0 {
		t.Fatalf("checked %d PREPARE, %d FETCH or CANCEL requests and %d data and %d terminal FETCH answers, want some of each",
			prepares, leading, data, terminal)
	}
	t.Logf("checked %d PREPARE, %d FETCH or CANCEL requests and %d data and %d terminal FETCH answers", prepares, leading, data, terminal)
}
