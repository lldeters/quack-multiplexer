package auth

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"quackmux/internal/config"
	"quackmux/internal/session"
)

// longUser does not fit into a connection id: adopted sessions know only
// its stored form.
var longUser = strings.Repeat("a-very-long-user-name-", 3)

func revokeUsersYAML(alice string) string {
	return fmt.Sprintf(`
users:
%s
  - name: %s
    tokens:
      - sha256: %q
  - name: carol
    groups: [analytics]
    tokens:
      - sha256: %q
        expires: "2026-06-01T00:00:00Z"
`, alice, longUser, HashToken("long-token"), HashToken("carol-token"))
}

const aliceEntry = `
  - name: alice
    groups: [analytics, admins]
    tokens:
      - sha256: %q
      - plain: alice-plain-token`

func TestStaticRevoked(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "users.yaml", []byte(revokeUsersYAML(fmt.Sprintf(aliceEntry, HashToken("alice-token")))))
	clock := newFakeClock(staticNow)
	a, _ := newTestStatic(t, path, 0, clock.Now)

	cred := func(token string) string {
		t.Helper()
		id, err := authStatic(a, token)
		if err != nil {
			t.Fatal(err)
		}
		if id.Credential != HashToken(token) || !id.Expires.IsZero() {
			t.Fatalf("identity of %q: credential %q, expires %v; want the token hash and no expiry", token, id.Credential, id.Expires)
		}
		return id.Credential
	}
	alice, alicePlain, long, carol := cred("alice-token"), cred("alice-plain-token"), cred("long-token"), cred("carol-token")
	now := clock.Now()

	check := func(t *testing.T, user, credential, group, want string) {
		t.Helper()
		if got := a.Revoked(user, credential, group, now); got != want {
			t.Fatalf("Revoked(%q, %q, %q) = %q, want %q", user, credential, group, got, want)
		}
	}
	reload := func(t *testing.T, content string) {
		t.Helper()
		rewrite(t, path, content, 2)
		if err := a.Reload(); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("unchanged file", func(t *testing.T) {
		check(t, "alice", alice, "", "")
		check(t, "alice", alicePlain, "analytics", "")
		check(t, "alice", "", "admins", "") // adopted session
		check(t, "alice", alice, "ops", `the user is no longer a member of group "ops"`)
		check(t, "mallory", "", "", "the user was removed")
		check(t, "carol", alice, "", "the token was removed") // another user's token
		check(t, "alice", HashToken("unknown"), "", "the token was removed")
	})
	t.Run("long user name", func(t *testing.T) {
		stored := session.StoredUser(longUser)
		if stored == longUser {
			t.Fatalf("test user %q fits into an id; make it longer", longUser)
		}
		check(t, longUser, long, "", "")
		check(t, stored, long, "", "")
		check(t, stored, "", "", "")
		check(t, stored, alice, "", "the token was removed")
	})
	t.Run("token expiry", func(t *testing.T) {
		check(t, "carol", carol, "analytics", "")
		now = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		t.Cleanup(func() { now = clock.Now() })
		check(t, "carol", carol, "analytics", "the token expired")
		check(t, "carol", "", "analytics", "") // adopted: the token is not known
	})

	t.Run("token removed", func(t *testing.T) {
		reload(t, revokeUsersYAML(`
  - name: alice
    groups: [analytics, admins]
    tokens:
      - plain: alice-plain-token`))
		check(t, "alice", alice, "", "the token was removed")
		check(t, "alice", alicePlain, "", "")
	})
	t.Run("token moved to another user", func(t *testing.T) {
		reload(t, revokeUsersYAML(`
  - name: alice
    tokens:
      - plain: alice-plain-token
  - name: dave
    tokens:
      - sha256: `+fmt.Sprintf("%q", HashToken("alice-token"))))
		check(t, "alice", alice, "", "the token was removed")
	})
	t.Run("left a group", func(t *testing.T) {
		reload(t, revokeUsersYAML(`
  - name: alice
    groups: [admins]
    tokens:
      - plain: alice-plain-token`))
		check(t, "alice", alicePlain, "admins", "")
		check(t, "alice", alicePlain, "analytics", `the user is no longer a member of group "analytics"`)
	})
	t.Run("token expiry extended", func(t *testing.T) {
		reload(t, strings.Replace(revokeUsersYAML(fmt.Sprintf(aliceEntry, HashToken("alice-token"))),
			"2026-06-01T00:00:00Z", "2027-06-01T00:00:00Z", 1))
		now = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		t.Cleanup(func() { now = clock.Now() })
		check(t, "carol", carol, "", "")
	})
	t.Run("user disabled", func(t *testing.T) {
		reload(t, revokeUsersYAML(`
  - name: alice
    disabled: true
    tokens:
      - plain: alice-plain-token`))
		check(t, "alice", alicePlain, "", "the user is disabled")
		check(t, "alice", "", "", "the user is disabled")
	})
	t.Run("user removed", func(t *testing.T) {
		reload(t, revokeUsersYAML(""))
		check(t, "alice", alicePlain, "", "the user was removed")
		check(t, "alice", "", "", "the user was removed")
		check(t, "carol", carol, "", "")
	})
	t.Run("invalid file keeps the previous one", func(t *testing.T) {
		rewrite(t, path, "users: [", 3)
		if err := a.Reload(); err == nil {
			t.Fatal("Reload accepted an invalid users file")
		}
		check(t, "carol", carol, "", "")
		check(t, "alice", "", "", "the user was removed")
	})
	t.Run("empty list", func(t *testing.T) {
		rewrite(t, path, "users: []\n", 4)
		if err := a.Reload(); err != nil {
			t.Fatal(err)
		}
		check(t, "carol", carol, "", "the user was removed")
		check(t, session.StoredUser(longUser), "", "", "the user was removed")
	})
}

// TestStoredNameMatchesSession pins storedName to the form that connection
// ids hold.
func TestStoredNameMatchesSession(t *testing.T) {
	for _, n := range []int{0, 1, session.MaxUserBytes - 1, session.MaxUserBytes, session.MaxUserBytes + 1, 200} {
		user := strings.Repeat("ü", n/2) + strings.Repeat("x", n%2)
		if got, want := storedName(user), session.StoredUser(user); got != want {
			t.Fatalf("storedName(%d bytes) = %q, session.StoredUser = %q", len(user), got, want)
		}
	}
}

// main hands the authenticator to the session manager if it is a
// Revalidator: auth.mode static must stay one.
func TestNewStaticIsARevalidator(t *testing.T) {
	path := writeFile(t, t.TempDir(), "users.yaml", []byte("users: []\n"))
	a, err := New(config.AuthConfig{Mode: "static", Static: config.StaticAuthConfig{File: path}}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, ok := a.(Revalidator); !ok {
		t.Fatalf("auth.New(static) returns %T, which is no Revalidator", a)
	}
}

var _ session.Revoker = Revalidator(nil)
