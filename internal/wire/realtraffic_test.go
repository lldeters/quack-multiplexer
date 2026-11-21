package wire

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The fixtures in testdata/duckdb-v2.0.0 are real Quack messages between a
// DuckDB v2.0.0 client and server (quack extension f96420e), recorded with a
// capturing reverse proxy; testdata/duckdb-v2.0.0/README.md says how. Files
// named *.truncated.bin hold only the first bytes of a large message.
//
// QUACK_CAPTURE_DIR may point to a directory of further recordings in the
// layout of the capturing proxy (NNNN-req.bin, NNNN-resp.bin and NNNN.meta,
// whose first line is "METHOD PATH"); its POST /quack exchanges are checked
// the same way.
const realTrafficDir = "testdata/duckdb-v2.0.0"

type realMessage struct {
	name      string
	msg       []byte
	truncated bool
}

func loadRealTraffic(t *testing.T) []realMessage {
	t.Helper()
	var out []realMessage
	read := func(f string) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, realMessage{name: f, msg: b, truncated: strings.HasSuffix(f, ".truncated.bin")})
	}
	fixtures, err := filepath.Glob(filepath.Join(realTrafficDir, "*.bin"))
	if err != nil || len(fixtures) == 0 {
		t.Fatalf("no fixtures in %s (%v)", realTrafficDir, err)
	}
	sort.Strings(fixtures)
	for _, f := range fixtures {
		read(f)
	}
	if dir := os.Getenv("QUACK_CAPTURE_DIR"); dir != "" {
		metas, err := filepath.Glob(filepath.Join(dir, "*.meta"))
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(metas)
		n := len(out)
		for _, m := range metas {
			meta, err := os.ReadFile(m)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(meta, []byte("POST /quack\n")) {
				continue // health probes (GET /) and CORS preflights
			}
			base := strings.TrimSuffix(m, ".meta")
			read(base + "-req.bin")
			if _, err := os.Stat(base + "-resp.bin"); err == nil {
				read(base + "-resp.bin")
			}
		}
		t.Logf("checking %d recorded messages from QUACK_CAPTURE_DIR=%s", len(out)-n, dir)
	}
	return out
}

func realFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(realTrafficDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRealTrafficRoundTrip decodes every recorded message and encodes it
// again: the gateway rewrites headers and re-encodes handshakes, so its
// encoder must reproduce DuckDB's bytes exactly.
func TestRealTrafficRoundTrip(t *testing.T) {
	seen := map[MessageType]int{}
	for _, m := range loadRealTraffic(t) {
		h, n, err := ParseHeader(m.msg)
		if err != nil {
			t.Errorf("%s: header: %v", m.name, err)
			continue
		}
		seen[h.Type]++
		if got := h.Encode(); !bytes.Equal(got, m.msg[:n]) {
			t.Errorf("%s: header re-encoded as %x, recorded %x", m.name, got, m.msg[:n])
		}
		if got, err := ReplaceHeader(m.msg, h); err != nil || !bytes.Equal(got, m.msg) {
			t.Errorf("%s: ReplaceHeader with the same header changed the message (err %v)", m.name, err)
		}
		if m.truncated {
			continue
		}
		if err := checkBody(h, m.msg, n); err != "" {
			t.Errorf("%s (%s): %s", m.name, h.Type, err)
		}
	}
	// The fixtures must keep covering every message type the gateway handles.
	for _, typ := range []MessageType{
		TypeConnectionRequest, TypeConnectionResponse, TypePrepareRequest, TypePrepareResponse,
		TypeFetchRequest, TypeFetchResponse, TypeSendDataRequest, TypeSendDataResponse,
		TypeSuccessResponse, TypeDisconnect, TypeCancelRequest, TypeAcknowledgement, TypeHeartbeatRequest,
		TypeErrorResponse,
	} {
		if seen[typ] == 0 {
			t.Errorf("no recorded %s message", typ)
		}
	}
}

// checkBody decodes and re-encodes the body of the message types the gateway
// decodes or writes itself. The bodies of the other types (results, fetched
// batches, uploads) pass the gateway untouched behind the rewritten header.
func checkBody(h Header, msg []byte, n int) string {
	same := func(got []byte) string {
		if !bytes.Equal(got, msg) {
			return "re-encoded as\n" + hexDump(got) + "recorded\n" + hexDump(msg)
		}
		return ""
	}
	switch h.Type {
	case TypeConnectionRequest:
		_, c, err := DecodeConnectionRequest(msg)
		if err != nil {
			return err.Error()
		}
		if c.MinSupportedVersion > ProtocolVersion || c.MaxSupportedVersion < ProtocolVersion {
			return "client does not support the gateway's protocol version"
		}
		return same(c.Encode(h))
	case TypeConnectionResponse:
		_, c, err := DecodeConnectionResponse(msg)
		if err != nil {
			return err.Error()
		}
		if c.QuackVersion != ProtocolVersion {
			return "unexpected quack version"
		}
		return same(c.Encode(h))
	case TypeErrorResponse:
		e, err := DecodeErrorResponse(msg)
		if err != nil {
			return err.Error()
		}
		if len(e.ExtraInfo) > 1 {
			// DuckDB writes an unordered_map in hash order, the encoder sorts.
			return ""
		}
		enc, err := ReplaceHeader(e.Encode(), h)
		if err != nil {
			return err.Error()
		}
		return same(enc)
	case TypePrepareRequest:
		p, err := DecodePrepareRequestBody(msg[n:])
		if err != nil {
			return err.Error()
		}
		return same(p.Encode(h))
	case TypeFetchRequest:
		f, err := DecodeFetchRequestBody(msg[n:])
		if err != nil {
			return err.Error()
		}
		return same(f.Encode(h))
	case TypeCancelRequest:
		d := newDecoder(msg[n:])
		if err := d.expectField(1); err != nil {
			return err.Error()
		}
		uuid, err := d.hugeint()
		if err != nil {
			return err.Error()
		}
		enc, err := ReplaceHeader(EncodeCancel(h.ConnectionID, uuid), h)
		if err != nil {
			return err.Error()
		}
		return same(enc)
	case TypeAcknowledgement:
		d := newDecoder(msg[n:])
		var uuid Hugeint
		if ok, err := d.optionalField(1); err != nil {
			return err.Error()
		} else if ok {
			if uuid, err = d.hugeint(); err != nil {
				return err.Error()
			}
		}
		enc, err := ReplaceHeader(EncodeAcknowledgement(h.ConnectionID, uuid), h)
		if err != nil {
			return err.Error()
		}
		return same(enc)
	case TypeHeartbeatRequest, TypeDisconnect, TypeSuccessResponse:
		return same(EncodeEmpty(h))
	}
	return ""
}

func hexDump(b []byte) string {
	var s strings.Builder
	for i := 0; i < len(b); i += 32 {
		end := min(i+32, len(b))
		s.WriteString("  " + hex.EncodeToString(b[i:end]) + "\n")
	}
	return s.String()
}

// TestRealTrafficGatewayAnswers checks that the messages the gateway writes
// itself in place of the engine are byte for byte what DuckDB's server sends
// in the same situation.
func TestRealTrafficGatewayAnswers(t *testing.T) {
	cases := []struct {
		fixture string
		got     []byte
	}{
		{"error-invalid-connection-id.bin", ErrorResponse{Message: "Invalid connection id", ExceptionType: ExceptionInvalidInput}.Encode()},
		{"error-authentication-failed.bin", ErrorResponse{Message: "Authentication failed", ExceptionType: ExceptionInvalidInput}.Encode()},
		{"heartbeat-response.bin", EncodeEmpty(Header{Type: TypeSuccessResponse})},
		{"disconnect-response.bin", EncodeEmpty(Header{Type: TypeSuccessResponse})},
	}
	for _, c := range cases {
		if want := realFixture(t, c.fixture); !bytes.Equal(c.got, want) {
			t.Errorf("%s: gateway writes\n%sDuckDB wrote\n%s", c.fixture, hexDump(c.got), hexDump(want))
		}
	}
}

// TestRealTrafficServerErrorTexts pins the texts of the server errors the
// gateway recognizes in engine answers.
func TestRealTrafficServerErrorTexts(t *testing.T) {
	for fixture, want := range map[string]string{
		"error-invalid-connection-id.bin": "Invalid connection id",
		"error-lease-expired.bin":         "Connection heartbeat lease expired",
		"error-authentication-failed.bin": "Authentication failed",
	} {
		e, err := DecodeErrorResponse(realFixture(t, fixture))
		if err != nil {
			t.Fatalf("%s: %v", fixture, err)
		}
		if e.Message != want || e.ExceptionType != ExceptionInvalidInput || e.MustInvalidate {
			t.Errorf("%s: got %+v, want message %q of type %q", fixture, e, want, ExceptionInvalidInput)
		}
	}
}

// TestRealTrafficHandshake pins what a DuckDB v2.0.0 client sends and what the
// server answers, including the fields the gateway rewrites.
func TestRealTrafficHandshake(t *testing.T) {
	h, req, err := DecodeConnectionRequest(realFixture(t, "connection-request.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if h.ConnectionID != "" || !h.ClientQueryID.Valid {
		t.Errorf("request header %+v: want no connection id and a client query id", h)
	}
	if req.ClientDuckDBVersion != "v2.0.0" || req.ClientPlatform != "linux_amd64" ||
		req.MinSupportedVersion != ProtocolVersion || req.MaxSupportedVersion != ProtocolVersion ||
		req.ClientID == "" || req.HeartbeatTimeoutSeconds != 3 || req.AuthString != "capture-token-0123456789" {
		t.Errorf("unexpected request %+v", req)
	}
	rh, resp, err := DecodeConnectionResponse(realFixture(t, "connection-response.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rh.ConnectionID) != 32 || strings.ToUpper(rh.ConnectionID) != rh.ConnectionID || rh.ClientQueryID.Valid {
		t.Errorf("response header %+v: want a 32 character upper-case connection id and no client query id", rh)
	}
	if resp.ServerDuckDBVersion != "v2.0.0" || resp.QuackVersion != ProtocolVersion || resp.HeartbeatTimeoutSeconds != 3 {
		t.Errorf("unexpected response %+v", resp)
	}
}
