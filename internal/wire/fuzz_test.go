package wire

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// seedCaptures adds every captured DuckDB v2.0.0 message to the corpus: whole,
// and without its header (the body decoders take the body).
func seedCaptures(f *testing.F) {
	f.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "duckdb-v2.0.0", "*.bin"))
	if err != nil || len(files) == 0 {
		f.Fatalf("no captures: %v", err)
	}
	for _, name := range files {
		b, err := os.ReadFile(name)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
		if _, n, err := ParseHeader(b); err == nil && n <= len(b) {
			f.Add(b[n:])
		}
	}
	f.Add([]byte{})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f})
}

// FuzzDecoders feeds arbitrary bytes to every decoder that reads client or
// engine input. A decoder must return a value or an error: no panic, no
// hang, and no result that points beyond the input.
func FuzzDecoders(f *testing.F) {
	seedCaptures(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _, _ = DecodeConnectionRequest(b)
		_, _, _ = DecodeConnectionResponse(b)
		_, _ = DecodeErrorResponse(b)
		_, _ = DecodePrepareRequestBody(b)
		_, _ = DecodeFetchRequestBody(b)
		_, _ = DecodeFetchResponseHead(b)
		_, _ = PrepareQueryUUID(b)
		_, _ = LeadingQueryUUID(b)
		for _, limit := range []int{0, 1, 64, 1 << 20} {
			if sql, _, err := PeekPrepareSQL(b, limit); err == nil && len(sql) > len(b) {
				t.Fatalf("PeekPrepareSQL returned %d bytes of SQL from %d bytes of input", len(sql), len(b))
			}
		}
		if r, err := DecodeVarcharResult(b); err == nil {
			rows := r.Rows()
			for _, c := range r.Columns {
				if len(c.Values) != rows {
					t.Fatalf("column with %d values in a result of %d rows", len(c.Values), rows)
				}
				for i := range c.Values {
					_ = c.IsNull(i)
				}
			}
		}
	})
}

// FuzzPrepareUUIDScanner: the streaming scanner must agree with the
// one-shot parser however the body is split into writes, and never panic.
func FuzzPrepareUUIDScanner(f *testing.F) {
	seedCaptures(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		want, wantErr := PrepareQueryUUID(b)
		for _, step := range []int{1, 2, 3, 7, len(b) + 1} {
			var s PrepareUUIDScanner
			for rest := b; len(rest) > 0; {
				n := min(step, len(rest))
				if _, err := s.Write(rest[:n]); err != nil {
					t.Fatalf("Write returned %v: the scanner must not fail the stream it watches", err)
				}
				rest = rest[n:]
			}
			got, ok := s.UUID()
			if wantErr == nil && (!ok || got != want) {
				t.Fatalf("step %d: scanner found %v (ok=%v), PrepareQueryUUID %v", step, got, ok, want)
			}
			if ok && wantErr == nil && got != want {
				t.Fatalf("step %d: scanner %v differs from %v", step, got, want)
			}
		}
		_ = bytes.MinRead
	})
}
