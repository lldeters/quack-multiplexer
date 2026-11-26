package proxy_test

import (
	"sync"
	"testing"
	"time"

	"quackmux/internal/wire"
)

// credRevoker revokes credentials (harness credentials are "cred-<user>").
type credRevoker struct {
	mu  sync.Mutex
	why map[string]string
}

func (r *credRevoker) Revoked(_, credential, _ string, _ time.Time) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.why[credential]
}

// Revoking a credential ends the sessions opened with it at the next session
// check: the client gets the reason, the engine a DISCONNECT, and the other
// sessions on the engine keep running.
func TestRevokedAccessEndsOpenSessions(t *testing.T) {
	h := newHarness(t, "", nil)
	rv := &credRevoker{why: map[string]string{}}
	h.sessions.SetRevoker(rv)
	alice := h.connect("tok?user=alice&share_level=server", nil)
	bob := h.connect("tok?user=bob&share_level=server", nil)
	srv := h.prov.Server(h.sessionEngine(alice))
	mustType(t, h.query(alice, "SELECT 1"), wire.TypePrepareResponse)

	rv.mu.Lock()
	rv.why["cred-alice"] = "the token was removed"
	rv.mu.Unlock()
	h.sessions.Reap()
	h.sessions.Wait()

	mustError(t, h.query(alice, "SELECT 1"), "access revoked: the token was removed")
	mustType(t, h.query(bob, "SELECT 1"), wire.TypePrepareResponse)
	if n := srv.CountType(wire.TypeDisconnect); n != 1 {
		t.Fatalf("%d DISCONNECTs reached the engine, want 1", n)
	}
	// A new connection with a valid credential works.
	mustType(t, h.query(h.connect("tok?user=carol&share_level=server", nil), "SELECT 1"), wire.TypePrepareResponse)
}
