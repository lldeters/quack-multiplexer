package session

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
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

// newTestManager returns a manager for tests whose sessions are on engine
// e1 of engines.
func newTestManager(t *testing.T, engines Engines, backend Backend) (*Manager, func() *Session) {
	t.Helper()
	cfg, err := config.Parse([]byte("auth: {mode: none}\nprofiles: {default: {}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := NewSealer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(cfg, engines, backend, sealer, slog.New(slog.DiscardHandler), nil)
	open := func() *Session {
		t.Helper()
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
	return m, open
}

// The close hook runs once for every session that closes.
func TestCloseHook(t *testing.T) {
	m, open := newTestManager(t, stubEngines{}, nil)
	var closed []string
	m.SetCloseHook(func(s *Session, keep bool) <-chan struct{} {
		if keep {
			t.Errorf("close of %s: keep without an engine to disconnect", s.ID)
		}
		closed = append(closed, s.ID)
		return nil
	})
	a, b := open(), open()
	if !m.Close(a.ID, "admin", CloseMode{Force: true}) || m.Close(a.ID, "admin", CloseMode{Force: true}) {
		t.Fatal("the first close must succeed, the second must not")
	}
	m.EngineGone("e1", "engine terminated")
	if len(closed) != 2 || closed[0] != a.ID || closed[1] != b.ID {
		t.Fatalf("hook saw %v, want [%s %s]", closed, a.ID, b.ID)
	}
}

// liveEngines has engine e1.
type liveEngines struct{ stubEngines }

func (liveEngines) Lookup(id string) (engine.Target, bool) {
	return engine.Target{ID: id, Endpoint: "http://e1.invalid"}, id == "e1"
}

// recordingBackend records the control messages the manager sends.
type recordingBackend struct {
	mu   sync.Mutex
	sent []string
}

func (b *recordingBackend) add(msg string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = append(b.sent, msg)
	return nil
}

func (b *recordingBackend) messages() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.sent...)
}

func (b *recordingBackend) Disconnect(context.Context, string, string) error {
	return b.add("DISCONNECT")
}

func (b *recordingBackend) CancelAll(context.Context, string, string) error { return b.add("CANCEL") }

func (b *recordingBackend) Heartbeat(context.Context, string, string) error {
	return b.add("HEARTBEAT")
}

// A close hook can hold the DISCONNECT of a closed session's engine
// connection: the manager sends the CANCEL of the close at once, the
// DISCONNECT once the hook's channel is closed, and renews the lease of the
// connection meanwhile. A connection the engine no longer has is not held,
// and Shutdown does not wait for a hold.
func TestCloseHookHoldsTheDisconnect(t *testing.T) {
	b := &recordingBackend{}
	m, open := newTestManager(t, liveEngines{}, b)
	m.cfg.Session.BackendHeartbeatTimeout.Duration = 300 * time.Millisecond // a keep-alive every 100ms
	release := make(chan struct{})
	var kept []bool
	m.SetCloseHook(func(_ *Session, keep bool) <-chan struct{} {
		kept = append(kept, keep)
		if keep {
			return release
		}
		return nil
	})

	m.Close(open().ID, "admin", CloseMode{Disconnect: true, Cancel: true, Force: true})
	time.Sleep(500 * time.Millisecond)
	sent := b.messages()
	if len(sent) < 2 || sent[0] != "CANCEL" || slices.Contains(sent, "DISCONNECT") {
		t.Fatalf("while the hook holds the session: %v, want CANCEL and keep-alives", sent)
	}
	for _, msg := range sent[1:] {
		if msg != "HEARTBEAT" {
			t.Fatalf("while the hook holds the session: %v, want CANCEL and keep-alives", sent)
		}
	}
	close(release)
	m.Wait()
	if sent := b.messages(); sent[len(sent)-1] != "DISCONNECT" {
		t.Fatalf("after the release: %v, want the DISCONNECT last", sent)
	}

	b.sent = nil
	m.ClosedByBackend(open(), "Invalid connection id")
	m.Wait()
	if sent := b.messages(); len(kept) != 2 || kept[1] || !slices.Equal(sent, []string{"DISCONNECT"}) {
		t.Fatalf("a connection the engine dropped: keep=%v, sent %v", kept, sent)
	}

	b.sent = nil
	release = make(chan struct{}) // never closed
	m.Close(open().ID, "client disconnect", CloseMode{Disconnect: true, Force: true})
	m.Shutdown()
	if sent := b.messages(); len(kept) != 3 || !kept[2] || len(sent) == 0 || sent[len(sent)-1] != "DISCONNECT" {
		t.Fatalf("at shutdown: keep=%v, sent %v, want the held DISCONNECT", kept, sent)
	}
}
