package session

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"quackmux/internal/engine"
	"quackmux/internal/wire"
)

// fakeRevoker revokes the users in why and records what it was asked.
type fakeRevoker struct {
	mu    sync.Mutex
	why   map[string]string
	asked []string
}

func (r *fakeRevoker) Revoked(user, credential, group string, _ time.Time) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, user+"|"+credential+"|"+group)
	return r.why[user]
}

func (r *fakeRevoker) set(user, why string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.why[user] = why
}

// adoptEngines knows engine e1 by its UID.
type adoptEngines struct{ liveEngines }

func (adoptEngines) IDByUID(uid engine.UID) (string, bool) { return "e1", uid == engine.UIDOf("e1") }

func openFor(t *testing.T, m *Manager, user string, key engine.Key, credential string, expires time.Time) *Session {
	t.Helper()
	r, err := m.Reserve(user, 0)
	if err != nil {
		t.Fatal(err)
	}
	s, _, err := r.Open(OpenParams{SessionID: NewSessionID(), User: user, EngineID: "e1", EngineKey: key,
		BackendConnID: strings.Repeat("A", 32), HeartbeatTimeout: time.Minute,
		Credential: credential, CredentialExpires: expires})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func closeReason(s *Session) string {
	if c, closed := s.Closed(); closed {
		return c.Reason
	}
	return ""
}

// Reap ends the sessions whose access the revoker revoked, also while a
// query runs, which it cancels; the other sessions stay. The revoker gets
// the credential and, for GROUP sessions, the group.
func TestReapClosesRevokedSessions(t *testing.T) {
	b := &recordingBackend{}
	m, _ := newTestManager(t, liveEngines{}, b)
	rv := &fakeRevoker{why: map[string]string{}}
	m.SetRevoker(rv)

	user := engine.Key{ShareLevel: engine.ShareUser, Owner: "alice", Profile: "default"}
	group := engine.Key{ShareLevel: engine.ShareGroup, Owner: "analytics", Profile: "default"}
	alice := openFor(t, m, "alice", user, "cred-a", time.Time{})
	busy := openFor(t, m, "bob", group, "cred-b", time.Time{})
	carol := openFor(t, m, "carol", group, "", time.Time{})
	if _, ok := busy.Begin(m.now(), wire.TypePrepareRequest); !ok {
		t.Fatal("Begin failed")
	}

	m.Reap()
	for _, s := range []*Session{alice, busy, carol} {
		if r := closeReason(s); r != "" {
			t.Fatalf("session of %s closed (%q) before anything was revoked", s.User, r)
		}
	}
	rv.mu.Lock()
	asked := slices.Sorted(slices.Values(rv.asked))
	rv.mu.Unlock()
	if want := []string{"alice|cred-a|", "bob|cred-b|analytics", "carol||analytics"}; !slices.Equal(asked, want) {
		t.Fatalf("revoker asked %q, want %q", asked, want)
	}

	rv.set("bob", "the token was removed")
	m.Reap()
	m.Wait()
	if r := closeReason(busy); r != "access revoked: the token was removed" {
		t.Fatalf("busy session: close reason %q", r)
	}
	if r := closeReason(alice) + closeReason(carol); r != "" {
		t.Fatalf("a session that was not revoked closed: %q", r)
	}
	if got := b.messages(); !slices.Equal(got, []string{"CANCEL", "DISCONNECT"}) {
		t.Fatalf("messages to the engine %q, want CANCEL then DISCONNECT", got)
	}
	if _, c, err := m.Resolve(mustSeal(t, m, busy)); err != nil || c == nil || c.Reason != "access revoked: the token was removed" {
		t.Fatalf("the client of the closed session sees %+v, %v", c, err)
	}
}

// A session whose credential expires ends at the next Reap after the expiry.
func TestReapClosesSessionsWhoseCredentialExpired(t *testing.T) {
	m, _ := newTestManager(t, liveEngines{}, &recordingBackend{})
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	m.SetClock(func() time.Time { return now })
	key := engine.Key{ShareLevel: engine.ShareUser, Owner: "alice", Profile: "default"}
	s := openFor(t, m, "alice", key, "", now.Add(time.Minute))
	forever := openFor(t, m, "alice", key, "", time.Time{})

	now = now.Add(59 * time.Second)
	m.Reap()
	if r := closeReason(s); r != "" {
		t.Fatalf("closed before the expiry: %q", r)
	}
	now = now.Add(time.Second)
	m.Reap()
	m.Wait()
	if r := closeReason(s); r != "access revoked: the credentials expired" {
		t.Fatalf("close reason %q", r)
	}
	if r := closeReason(forever); r != "" {
		t.Fatalf("a session without expiry closed: %q", r)
	}
}

// A session of a revoked user is not adopted: its first request after a
// gateway restart fails, and the engine connection is closed.
func TestRevokedSessionIsNotAdopted(t *testing.T) {
	b := &recordingBackend{}
	m, _ := newTestManager(t, adoptEngines{}, b)
	rv := &fakeRevoker{why: map[string]string{"alice": "the user was removed"}}
	m.SetRevoker(rv)
	id, err := m.sealer.Seal(Route{SessionID: NewSessionID(), EngineUID: engine.UIDOf("e1"),
		BackendConnID: strings.Repeat("A", 32), User: "alice", CreatedUnix: m.now().Unix(), HeartbeatTimeout: 60})
	if err != nil {
		t.Fatal(err)
	}
	s, c, err := m.Resolve(id)
	if err != nil || s != nil || c == nil || c.Reason != "access revoked: the user was removed" {
		t.Fatalf("Resolve = %v, %+v, %v; want the session closed as revoked", s, c, err)
	}
	m.Wait()
	if got := b.messages(); !slices.Equal(got, []string{"CANCEL", "DISCONNECT"}) {
		t.Fatalf("messages to the engine %q, want CANCEL and DISCONNECT without a confirming HEARTBEAT", got)
	}
	if n := m.Count(); n != 0 {
		t.Fatalf("%d sessions open, want none", n)
	}

	// A user who may run is adopted as before.
	rv.set("alice", "")
	id, _ = m.sealer.Seal(Route{SessionID: NewSessionID(), EngineUID: engine.UIDOf("e1"),
		BackendConnID: strings.Repeat("B", 32), User: "alice", CreatedUnix: m.now().Unix(), HeartbeatTimeout: 60})
	if s, c, err := m.Resolve(id); s == nil || c != nil || err != nil {
		t.Fatalf("Resolve = %v, %+v, %v; want the session adopted", s, c, err)
	}
}

// blockingRevoker revokes everybody; its first call waits for release.
type blockingRevoker struct {
	calls   chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingRevoker) Revoked(string, string, string, time.Time) string {
	first := false
	r.once.Do(func() { first = true })
	r.calls <- struct{}{}
	if first {
		<-r.release
	}
	return "the user was removed"
}

// A request that comes while another one adopts a revoked session does not
// get the session either: it is never visible.
func TestConcurrentRequestsDoNotSeeARevokedSession(t *testing.T) {
	b := &recordingBackend{}
	m, _ := newTestManager(t, adoptEngines{}, b)
	rv := &blockingRevoker{calls: make(chan struct{}, 2), release: make(chan struct{})}
	m.SetRevoker(rv)
	id, err := m.sealer.Seal(Route{SessionID: NewSessionID(), EngineUID: engine.UIDOf("e1"),
		BackendConnID: strings.Repeat("A", 32), User: "alice", CreatedUnix: m.now().Unix(), HeartbeatTimeout: 60})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		s *Session
		c *Closed
	}
	first := make(chan result, 1)
	go func() {
		s, c, _ := m.Resolve(id)
		first <- result{s, c}
	}()
	<-rv.calls // the first adoption asks the revoker and waits
	s, c, err := m.Resolve(id)
	if err != nil || s != nil || c == nil || !strings.HasPrefix(c.Reason, "access revoked") {
		t.Fatalf("second request: %v, %+v, %v; want the session closed as revoked", s, c, err)
	}
	close(rv.release)
	if r := <-first; r.s != nil || r.c == nil || !strings.HasPrefix(r.c.Reason, "access revoked") {
		t.Fatalf("first request: %v, %+v; want the session closed as revoked", r.s, r.c)
	}
	m.Wait()
	if got := b.messages(); !slices.Equal(got, []string{"CANCEL", "DISCONNECT"}) {
		t.Fatalf("messages to the engine %q, want one CANCEL and one DISCONNECT", got)
	}
	if n := m.Count(); n != 0 {
		t.Fatalf("%d sessions open, want none", n)
	}
}

func mustSeal(t *testing.T, m *Manager, s *Session) string {
	t.Helper()
	id, err := m.sealer.Seal(Route{SessionID: s.ID, EngineUID: engine.UIDOf(s.EngineID), BackendConnID: s.BackendConnID,
		User: s.User, CreatedUnix: s.CreatedAt.Unix(), HeartbeatTimeout: uint64(s.HeartbeatTimeout / time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	return id
}
