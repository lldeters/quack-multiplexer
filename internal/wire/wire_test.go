package wire

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math"
	"strings"
	"testing"
)

// invalidIdxBytes is ULEB128(2^64-1): how WriteProperty<optional_idx> encodes
// an unset optional_idx.
const invalidIdxHex = "ffffffffffffffffff01"

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

// The golden encodings below are derived by hand from DuckDB's
// BinarySerializer rules and the generated serialize_quack_message.cpp.

func TestHeaderGoldenNoConnection(t *testing.T) {
	got := Header{Type: TypeConnectionRequest}.Encode()
	// field 1 (01 00), type=1; field 2 omitted (empty); field 3 (03 00) INVALID; terminator
	want := mustHex(t, "0100 01 0300 "+invalidIdxHex+" ffff")
	if !bytes.Equal(got, want) {
		t.Fatalf("header mismatch\n got %x\nwant %x", got, want)
	}
}

func TestHeaderGoldenWithConnectionAndQueryID(t *testing.T) {
	got := Header{Type: TypeHeartbeatRequest, ConnectionID: "abc", ClientQueryID: SomeIdx(300)}.Encode()
	// type 16 = 0x10; "abc" = 03 616263; 300 = ULEB128 ac 02
	want := mustHex(t, "0100 10 0200 03616263 0300 ac02 ffff")
	if !bytes.Equal(got, want) {
		t.Fatalf("header mismatch\n got %x\nwant %x", got, want)
	}
}

func TestParseHeaderRoundTrip(t *testing.T) {
	cases := []Header{
		{Type: TypeConnectionRequest},
		{Type: TypePrepareRequest, ConnectionID: strings.Repeat("f", 32)},
		{Type: TypeFetchRequest, ConnectionID: "x", ClientQueryID: SomeIdx(0)},
		{Type: TypeErrorResponse, ClientQueryID: SomeIdx(math.MaxUint64 - 1)},
	}
	for _, h := range cases {
		enc := h.Encode()
		msg := append(append([]byte{}, enc...), 0xde, 0xad) // trailing body bytes
		got, n, err := ParseHeader(msg)
		if err != nil {
			t.Fatalf("%v: %v", h, err)
		}
		if got != h || n != len(enc) {
			t.Fatalf("round trip mismatch: got %+v n=%d, want %+v n=%d", got, n, h, len(enc))
		}
	}
}

func TestParseHeaderTruncated(t *testing.T) {
	enc := Header{Type: TypeDisconnect, ConnectionID: "0123456789abcdef"}.Encode()
	for i := 0; i < len(enc); i++ {
		if _, _, err := ParseHeader(enc[:i]); !errors.Is(err, ErrTruncated) {
			t.Fatalf("prefix %d: want ErrTruncated, got %v", i, err)
		}
	}
}

func TestParseHeaderMalformed(t *testing.T) {
	bad := [][]byte{
		mustHex(t, "0200 01"),                                // wrong first field
		mustHex(t, "0100 01 0400 00 ffff"),                   // field 4 where 3 is required
		mustHex(t, "0100 01 0300 00 0500"),                   // missing terminator
		mustHex(t, "0100 8002 0300 00 ffff"),                 // type 256 does not fit uint8
		mustHex(t, "0100 01 0300 ffffffffffffffffff7f ffff"), // ULEB128 overflow
	}
	for i, b := range bad {
		if _, _, err := ParseHeader(b); !errors.Is(err, ErrMalformed) {
			t.Fatalf("case %d: want ErrMalformed, got %v", i, err)
		}
	}
}

func TestVarintsMatchDuckDB(t *testing.T) {
	cases := []struct {
		v    int64
		want string
	}{
		{0, "00"}, {1, "01"}, {-1, "7f"}, {63, "3f"}, {64, "c000"}, {-64, "40"}, {-65, "bf7f"},
		{math.MaxInt64, "ffffffffffffffffff00"}, {math.MinInt64, "8080808080808080807f"},
	}
	for _, c := range cases {
		var e encoder
		e.svarint(c.v)
		if hex.EncodeToString(e.bytes()) != c.want {
			t.Fatalf("svarint(%d) = %x, want %s", c.v, e.bytes(), c.want)
		}
		got, err := newDecoder(e.bytes()).svarint()
		if err != nil || got != c.v {
			t.Fatalf("decode svarint(%d) = %d, %v", c.v, got, err)
		}
	}
	ucases := []struct {
		v    uint64
		want string
	}{
		{0, "00"}, {127, "7f"}, {128, "8001"}, {300, "ac02"}, {math.MaxUint64, invalidIdxHex},
	}
	for _, c := range ucases {
		var e encoder
		e.uvarint(c.v)
		if hex.EncodeToString(e.bytes()) != c.want {
			t.Fatalf("uvarint(%d) = %x, want %s", c.v, e.bytes(), c.want)
		}
		got, err := newDecoder(e.bytes()).uvarint()
		if err != nil || got != c.v {
			t.Fatalf("decode uvarint(%d) = %d, %v", c.v, got, err)
		}
	}
}

func TestConnectionRequestGoldenAndRoundTrip(t *testing.T) {
	req := ConnectionRequest{
		AuthString:              "asdf",
		ClientDuckDBVersion:     "v2.0.0",
		ClientPlatform:          "linux_amd64",
		MinSupportedVersion:     3,
		MaxSupportedVersion:     3,
		ClientID:                "client-1",
		HeartbeatTimeoutSeconds: 60,
	}
	enc := req.Encode(Header{})
	want := mustHex(t, "0100 01 0300 "+invalidIdxHex+" ffff"+
		"0100 04 61736466"+
		"0200 06 76322e302e30"+
		"0300 0b 6c696e75785f616d643634"+
		"0400 03"+
		"0500 03"+
		"0600 08 636c69656e742d31"+
		"0700 3c"+
		"ffff")
	if !bytes.Equal(enc, want) {
		t.Fatalf("encoding mismatch\n got %x\nwant %x", enc, want)
	}
	h, got, err := DecodeConnectionRequest(enc)
	if err != nil {
		t.Fatal(err)
	}
	if h.Type != TypeConnectionRequest || got != req {
		t.Fatalf("round trip mismatch: %+v %+v", h, got)
	}
	if !got.SupportsVersion(ProtocolVersion) || got.SupportsVersion(4) {
		t.Fatal("version range check wrong")
	}
}

func TestConnectionRequestDefaultsOmitted(t *testing.T) {
	// Empty client id and zero values are omitted, exactly like
	// WritePropertyWithDefault does it.
	enc := ConnectionRequest{AuthString: "tok", MinSupportedVersion: 3, MaxSupportedVersion: 3, HeartbeatTimeoutSeconds: 1}.Encode(Header{})
	_, n, _ := ParseHeader(enc)
	want := mustHex(t, "0100 03 746f6b 0400 03 0500 03 0700 01 ffff")
	if !bytes.Equal(enc[n:], want) {
		t.Fatalf("body mismatch\n got %x\nwant %x", enc[n:], want)
	}
	_, got, err := DecodeConnectionRequest(enc)
	if err != nil || got.ClientID != "" || got.ClientPlatform != "" {
		t.Fatalf("decode: %+v %v", got, err)
	}
}

func TestConnectionRequestRejectsUnknownField(t *testing.T) {
	enc := ConnectionRequest{AuthString: "tok", MinSupportedVersion: 3, MaxSupportedVersion: 3}.Encode(Header{})
	// replace the terminator by an unknown field 9
	enc = append(enc[:len(enc)-2], mustHex(t, "0900 01 ffff")...)
	if _, _, err := DecodeConnectionRequest(enc); !errors.Is(err, ErrMalformed) {
		t.Fatalf("want ErrMalformed, got %v", err)
	}
}

func TestConnectionResponseRoundTrip(t *testing.T) {
	resp := ConnectionResponse{ServerDuckDBVersion: "v2.0.0", ServerPlatform: "linux_arm64", QuackVersion: 3, HeartbeatTimeoutSeconds: 45}
	enc := resp.Encode(Header{ConnectionID: "00112233445566778899aabbccddeeff"})
	h, got, err := DecodeConnectionResponse(enc)
	if err != nil {
		t.Fatal(err)
	}
	if h.Type != TypeConnectionResponse || h.ConnectionID != "00112233445566778899aabbccddeeff" || got != resp {
		t.Fatalf("mismatch: %+v %+v", h, got)
	}
	// Rewrite the connection id the way the gateway does it.
	rewritten, err := ReplaceHeader(enc, Header{Type: TypeConnectionResponse, ConnectionID: "qmx1_gateway-session"})
	if err != nil {
		t.Fatal(err)
	}
	h2, got2, err := DecodeConnectionResponse(rewritten)
	if err != nil || h2.ConnectionID != "qmx1_gateway-session" || got2 != resp {
		t.Fatalf("rewrite mismatch: %+v %+v %v", h2, got2, err)
	}
}

func TestErrorResponseGolden(t *testing.T) {
	enc := ErrorResponse{Message: "boom", ExceptionType: ExceptionInvalidInput, MustInvalidate: true}.Encode()
	want := mustHex(t, "0100 64 0300 "+invalidIdxHex+" ffff"+
		"0100 04 626f6f6d"+
		"0200 0d 496e76616c696420496e707574"+
		"0400 01"+
		"ffff")
	if !bytes.Equal(enc, want) {
		t.Fatalf("encoding mismatch\n got %x\nwant %x", enc, want)
	}
	got, err := DecodeErrorResponse(enc)
	if err != nil || got.Message != "boom" || !got.MustInvalidate || got.ExceptionType != ExceptionInvalidInput {
		t.Fatalf("decode: %+v %v", got, err)
	}
}

func TestErrorResponseExtraInfo(t *testing.T) {
	in := ErrorResponse{Message: "x", ExtraInfo: map[string]string{"b": "2", "a": "1"}}
	enc := in.Encode()
	_, n, _ := ParseHeader(enc)
	// map: count 2, then {0:"a",1:"1"} {0:"b",1:"2"} in sorted order
	want := mustHex(t, "0100 01 78 0300 02 0000 01 61 0100 01 31 ffff 0000 01 62 0100 01 32 ffff ffff")
	if !bytes.Equal(enc[n:], want) {
		t.Fatalf("body mismatch\n got %x\nwant %x", enc[n:], want)
	}
	got, err := DecodeErrorResponse(enc)
	if err != nil || got.ExtraInfo["a"] != "1" || got.ExtraInfo["b"] != "2" || got.MustInvalidate {
		t.Fatalf("decode: %+v %v", got, err)
	}
}

func TestPrepareRequest(t *testing.T) {
	p := PrepareRequest{SQL: "SELECT 42", QueryUUID: Hugeint{Upper: 1, Lower: 2}}
	enc := p.Encode(Header{ConnectionID: "c"})
	h, n, err := ParseHeader(enc)
	if err != nil || h.Type != TypePrepareRequest {
		t.Fatalf("%+v %v", h, err)
	}
	want := mustHex(t, "0100 09 53454c454354203432 0200 01 02 ffff")
	if !bytes.Equal(enc[n:], want) {
		t.Fatalf("body mismatch\n got %x\nwant %x", enc[n:], want)
	}
	got, err := DecodePrepareRequestBody(enc[n:])
	if err != nil || got != p {
		t.Fatalf("decode: %+v %v", got, err)
	}
	sql, cut, err := PeekPrepareSQL(enc[n:n+6], 100)
	if err != nil || sql != "SEL" || !cut {
		t.Fatalf("peek partial: %q %v %v", sql, cut, err)
	}
	sql, cut, err = PeekPrepareSQL(enc[n:], 6)
	if err != nil || sql != "SELECT" || !cut {
		t.Fatalf("peek limited: %q %v %v", sql, cut, err)
	}
	p.InlineRows = SomeIdx(0)
	enc = p.Encode(Header{ConnectionID: "c"})
	_, n, _ = ParseHeader(enc)
	got, err = DecodePrepareRequestBody(enc[n:])
	if err != nil || got != p {
		t.Fatalf("decode with inline rows: %+v %v", got, err)
	}
}

func TestControlMessages(t *testing.T) {
	cancel := EncodeCancel("conn", Hugeint{})
	h, n, err := ParseHeader(cancel)
	if err != nil || h.Type != TypeCancelRequest || h.ConnectionID != "conn" {
		t.Fatalf("%+v %v", h, err)
	}
	if !bytes.Equal(cancel[n:], mustHex(t, "0100 00 00 ffff")) {
		t.Fatalf("cancel body %x", cancel[n:])
	}
	disc := EncodeEmpty(Header{Type: TypeDisconnect, ConnectionID: "conn"})
	_, n, _ = ParseHeader(disc)
	if !bytes.Equal(disc[n:], mustHex(t, "ffff")) {
		t.Fatalf("disconnect body %x", disc[n:])
	}
	ack := EncodeAcknowledgement("conn", Hugeint{})
	_, n, _ = ParseHeader(ack)
	if !bytes.Equal(ack[n:], mustHex(t, "ffff")) {
		t.Fatalf("ack body %x", ack[n:])
	}
	f := FetchRequest{QueryUUID: Hugeint{Upper: -5, Lower: 9}, BatchIndex: 2, AckIndex: 1}
	enc := f.Encode(Header{ConnectionID: "conn"})
	_, n, _ = ParseHeader(enc)
	got, err := DecodeFetchRequestBody(enc[n:])
	if err != nil || got != f {
		t.Fatalf("fetch: %+v %v", got, err)
	}
}

func FuzzParseHeader(f *testing.F) {
	f.Add(Header{Type: TypeConnectionRequest}.Encode())
	f.Add(Header{Type: TypeFetchRequest, ConnectionID: "abc", ClientQueryID: SomeIdx(7)}.Encode())
	f.Add([]byte{0x01, 0x00})
	f.Fuzz(func(t *testing.T, b []byte) {
		h, n, err := ParseHeader(b)
		if err != nil {
			return
		}
		if n > len(b) {
			t.Fatalf("header length %d beyond input %d", n, len(b))
		}
		// A successfully parsed header must re-encode to something that parses to the same value.
		h2, _, err := ParseHeader(h.Encode())
		if err != nil || h2 != h {
			t.Fatalf("re-encode mismatch %+v vs %+v (%v)", h, h2, err)
		}
	})
}

func FuzzDecodeConnectionRequest(f *testing.F) {
	f.Add(ConnectionRequest{AuthString: "a", MinSupportedVersion: 3, MaxSupportedVersion: 3, HeartbeatTimeoutSeconds: 60}.Encode(Header{}))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, c, err := DecodeConnectionRequest(b)
		if err != nil {
			return
		}
		_, c2, err := DecodeConnectionRequest(c.Encode(Header{}))
		if err != nil || c2 != c {
			t.Fatalf("re-encode mismatch %+v vs %+v (%v)", c, c2, err)
		}
	})
}
