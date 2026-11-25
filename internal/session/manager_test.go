package session

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"quackmux/internal/config"
	"quackmux/internal/engine"
)

// Run skips a round only after a real stand-still, not after a slow round.
func TestPaused(t *testing.T) {
	const every = 5 * time.Second
	for gap, want := range map[time.Duration]bool{
		0:                false,
		every:            false,
		2 * every:        false,
		11 * time.Second: false,
		12 * time.Second: true,
		45 * time.Second: true,
	} {
		if got := paused(gap, every); got != want {
			t.Errorf("paused(%s, %s) = %v, want %v", gap, every, got, want)
		}
	}
}

// stubEngines knows no engines.
type stubEngines struct{}

func (stubEngines) IDByUID(engine.UID) (string, bool)    { return "", false }
func (stubEngines) Lookup(string) (engine.Target, bool)  { return engine.Target{}, false }
func (stubEngines) Exists(string) (engine.State, bool)   { return "", false }
func (stubEngines) AttachAdopted(string, string, string) {}
func (stubEngines) DetachSession(string)                 {}

// The close hook runs once for every session that closes.
func TestCloseHook(t *testing.T) {
	cfg, err := config.Parse([]byte("auth: {mode: none}\nprofiles: {default: {}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := NewSealer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(cfg, stubEngines{}, nil, sealer, slog.New(slog.DiscardHandler), nil)
	var closed []string
	m.SetCloseHook(func(s *Session) { closed = append(closed, s.ID) })
	open := func() *Session {
		r, err := m.Reserve("alice", 0)
		if err != nil {
			t.Fatal(err)
		}
		s, _, err := r.Open(OpenParams{SessionID: NewSessionID(), User: "alice", EngineID: "e1",
			BackendConnID: strings.Repeat("A", 32), HeartbeatTimeout: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := open(), open()
	if !m.Close(a.ID, "admin", CloseMode{Force: true}) || m.Close(a.ID, "admin", CloseMode{Force: true}) {
		t.Fatal("the first close must succeed, the second must not")
	}
	m.EngineGone("e1", "engine terminated")
	if len(closed) != 2 || closed[0] != a.ID || closed[1] != b.ID {
		t.Fatalf("hook saw %v, want [%s %s]", closed, a.ID, b.ID)
	}
}
