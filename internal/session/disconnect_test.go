package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"quackmux/internal/upstream"
	"quackmux/internal/wire"
)

// flakyBackend fails the first failures DISCONNECTs with err.
type flakyBackend struct {
	recordingBackend
	mu       sync.Mutex
	failures int
	err      error
	calls    int
}

func (b *flakyBackend) Disconnect(context.Context, string, string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if b.calls <= b.failures {
		return b.err
	}
	return nil
}

func (b *flakyBackend) disconnects() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// A DISCONNECT that fails is retried: an engine that keeps the connection
// would let the session come back after a gateway restart. A connection the
// engine no longer knows counts as closed.
func TestDisconnectOfAClosedSessionIsRetried(t *testing.T) {
	unreachable := errors.New("dial tcp: connection refused")
	gone := &upstream.EngineError{Response: wire.ErrorResponse{Message: upstream.MsgInvalidConnection}}
	for _, tc := range []struct {
		name     string
		failures int
		err      error
		want     int
	}{
		{"first try", 0, nil, 1},
		{"second try", 1, unreachable, 2},
		{"third try", 2, unreachable, 3},
		{"never", 99, unreachable, 3},
		{"engine forgot it", 99, gone, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &flakyBackend{failures: tc.failures, err: tc.err}
			m, open := newTestManager(t, liveEngines{}, b)
			m.disconnectRetries = []time.Duration{time.Millisecond, time.Millisecond}
			s := open()
			if !m.Close(s.ID, "admin", CloseMode{Disconnect: true, Force: true}) {
				t.Fatal("session not closed")
			}
			m.Wait()
			if got := b.disconnects(); got != tc.want {
				t.Fatalf("%d DISCONNECTs, want %d", got, tc.want)
			}
		})
	}
}

// A shutdown ends the retries at once.
func TestShutdownEndsDisconnectRetries(t *testing.T) {
	b := &flakyBackend{failures: 99, err: errors.New("refused")}
	m, open := newTestManager(t, liveEngines{}, b)
	m.disconnectRetries = []time.Duration{time.Hour, time.Hour}
	m.Close(open().ID, "admin", CloseMode{Disconnect: true, Force: true})
	deadline := time.Now().Add(5 * time.Second)
	for b.disconnects() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	done := make(chan struct{})
	go func() { m.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown waited for the DISCONNECT retries")
	}
}
