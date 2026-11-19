package session

import (
	"errors"
	"strings"
	"testing"

	"quackmux/internal/engine"
)

func testSealer(t *testing.T, key string) *Sealer {
	t.Helper()
	s, err := NewSealer([]byte(key))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSealOpenRoundTrip(t *testing.T) {
	s := testSealer(t, "a-very-secret-session-key-0123456789")
	cases := []Route{
		{SessionID: NewSessionID(), EngineUID: engine.UIDOf("quack-user-alice-x7k2p"), BackendConnID: strings.Repeat("AB", 16),
			User: "alice", CreatedUnix: 1763550000, HeartbeatTimeout: 60},
		{SessionID: NewSessionID(), EngineUID: engine.UIDOf("e"), BackendConnID: strings.Repeat("0f", 16),
			User: "bob@example.com", CreatedUnix: 1, HeartbeatTimeout: 300},
		{SessionID: NewSessionID(), EngineUID: engine.UIDOf("e2"), BackendConnID: "not-hex-but-ok", User: ""},
	}
	for _, r := range cases {
		id, err := s.Seal(r)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(id, "qm") || strings.Contains(id, r.BackendConnID) || (r.User != "" && strings.Contains(id, r.User)) {
			t.Fatalf("id leaks route details: %s", id)
		}
		if len(id) > MaxIDLength {
			t.Fatalf("id of %d bytes exceeds MaxIDLength", len(id))
		}
		got, err := s.Open(id)
		if err != nil || got != r {
			t.Fatalf("round trip:\n got %+v %v\nwant %+v", got, err, r)
		}
	}
}

func TestIDLengthStaysWithinClientLimit(t *testing.T) {
	s := testSealer(t, "a-very-secret-session-key-0123456789")
	r := Route{SessionID: NewSessionID(), EngineUID: engine.UIDOf("x"), BackendConnID: strings.Repeat("F", 32),
		User: strings.Repeat("u", MaxUserBytes), CreatedUnix: 1 << 31, HeartbeatTimeout: 1 << 20}
	id, err := s.Seal(r)
	if err != nil {
		t.Fatal(err)
	}
	// 64 + len(connection_id) + 36 (stream uuid) must fit the client's 256-byte header buffer.
	if len(id) > MaxIDLength || 64+len(id)+36 > 256 {
		t.Fatalf("id of %d bytes breaks the client's SEND_DATA header budget", len(id))
	}
	got, _ := s.Open(id)
	if got.HeartbeatTimeout != 0xFFFF {
		t.Fatalf("heartbeat timeout must saturate, got %d", got.HeartbeatTimeout)
	}
	// Long user names are stored as a digest.
	long := strings.Repeat("v", 300)
	r.User = long
	id, err = s.Seal(r)
	if err != nil || len(id) > MaxIDLength {
		t.Fatalf("long user: %d bytes, %v", len(id), err)
	}
	got, _ = s.Open(id)
	if got.User != StoredUser(long) || !strings.HasPrefix(got.User, "~") {
		t.Fatalf("long user stored as %q", got.User)
	}
	// Backend ids that do not fit are refused rather than producing an id the client chokes on.
	r.User = "alice"
	r.BackendConnID = strings.Repeat("z", 65)
	if _, err := s.Seal(r); err == nil {
		t.Fatal("oversized backend id accepted")
	}
}

func TestOpenRejectsForeignAndTampered(t *testing.T) {
	s1 := testSealer(t, "key-number-one-0123456789")
	s2 := testSealer(t, "key-number-two-0123456789")
	id, err := s1.Seal(Route{SessionID: NewSessionID(), EngineUID: engine.UIDOf("e"), BackendConnID: strings.Repeat("a", 32)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Open(id); !errors.Is(err, ErrInvalidID) {
		t.Fatal("foreign key accepted")
	}
	b := []byte(id)
	b[len(b)-3] ^= 1
	if _, err := s1.Open(string(b)); !errors.Is(err, ErrInvalidID) {
		t.Fatal("tampered id accepted")
	}
	for _, bad := range []string{"", "qm", "qm!!!", "0123456789abcdef0123456789abcdef", "qx" + id[2:], id + strings.Repeat("A", 100)} {
		if _, err := s1.Open(bad); !errors.Is(err, ErrInvalidID) {
			t.Fatalf("%q accepted", bad)
		}
	}
	for _, sid := range []string{"short", "not base64!!!!!!", NewSessionID() + "AAAA"} {
		if _, err := s1.Seal(Route{SessionID: sid, BackendConnID: "x"}); err == nil {
			t.Fatalf("malformed session id %q accepted as nonce", sid)
		}
	}
	if _, err := NewSealer([]byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
}

func FuzzOpen(f *testing.F) {
	s, _ := NewSealer([]byte("fuzz-key-0123456789abcdef"))
	id, _ := s.Seal(Route{SessionID: NewSessionID(), EngineUID: engine.UIDOf("e"), BackendConnID: strings.Repeat("a", 32), User: "u"})
	f.Add(id)
	f.Add("qmAAAA")
	f.Fuzz(func(t *testing.T, in string) {
		_, _ = s.Open(in) // must never panic
	})
}
