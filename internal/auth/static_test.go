package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"quackmux/internal/config"
)

var staticNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func staticUsersYAML() string {
	return fmt.Sprintf(`
users:
  - name: alice
    groups: [analytics, admins, analytics]
    tokens:
      - sha256: %q
        description: laptop
        expires: "2030-01-01T00:00:00Z"
      - plain: alice-plain-token
  - name: bob
    disabled: true
    tokens:
      - sha256: %q
        expires: "2025-01-01T00:00:00Z"
  - name: carol
    tokens:
      - sha256: %q
        description: old laptop
        expires: "2025-06-01T00:00:00Z"
  - name: dave
    tokens:
      - sha256: %q
  - name: erin
`, HashToken("alice-token"), HashToken("bob-token"), HashToken("carol-token"), strings.ToUpper(HashToken("dave-token")))
}

// rewrite replaces the file and gives it a distinct modification time, so
// that tests do not depend on the file system's timestamp resolution.
func rewrite(t *testing.T, path, content string, version int) {
	t.Helper()
	// Write a temporary file and rename it over path, so that a poll never
	// sees the file truncated or half written: os.WriteFile truncates first,
	// and a poll in between read an empty, invalid users file.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2026, 1, 1, 0, 0, version, 0, time.UTC)
	if err := os.Chtimes(tmp, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func newTestStatic(t *testing.T, path string, interval time.Duration, now func() time.Time) (*staticAuthenticator, *syncBuffer) {
	t.Helper()
	log, buf := testLogger()
	a, err := newStatic(config.StaticAuthConfig{File: path, ReloadInterval: config.D(interval)}, log, now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a, buf
}

func authStatic(a *staticAuthenticator, credential string) (Identity, error) {
	return a.Authenticate(context.Background(), Request{Credential: credential})
}

func TestStaticAuthenticate(t *testing.T) {
	path := writeFile(t, t.TempDir(), "users.yaml", []byte(staticUsersYAML()))
	clock := newFakeClock(staticNow)
	a, _ := newTestStatic(t, path, 0, clock.Now)
	tests := []struct {
		name       string
		credential string
		want       Identity
		wantErr    string
	}{
		{name: "hashed token", credential: "alice-token", want: Identity{User: "alice", Groups: []string{"analytics", "admins"}, Method: MethodStatic}},
		{name: "plain token", credential: "alice-plain-token", want: Identity{User: "alice", Groups: []string{"analytics", "admins"}, Method: MethodStatic}},
		{name: "upper-case hash in file", credential: "dave-token", want: Identity{User: "dave", Method: MethodStatic}},
		{name: "disabled user", credential: "bob-token", wantErr: `user "bob" is disabled`},
		{name: "expired token", credential: "carol-token", wantErr: `token "old laptop" of user "carol" expired at 2025-06-01T00:00:00Z`},
		{name: "empty credential", credential: "", wantErr: "empty credential"},
		{name: "unknown token", credential: "mallory-token", wantErr: "unknown token"},
		{name: "no trimming", credential: " alice-token", wantErr: "unknown token"},
		{name: "hash is not a credential", credential: HashToken("alice-token"), wantErr: "unknown token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := authStatic(a, tt.credential)
			if tt.wantErr != "" {
				if !errors.Is(err, ErrUnauthenticated) || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want ErrUnauthenticated with %q", err, tt.wantErr)
				}
				if tt.credential != "" && strings.Contains(err.Error(), tt.credential) {
					t.Fatalf("error %q contains the credential", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(id, tt.want) {
				t.Fatalf("identity = %+v, want %+v", id, tt.want)
			}
		})
	}
}

func TestStaticExpiryBoundary(t *testing.T) {
	expires := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	content := fmt.Sprintf("users:\n  - name: u\n    tokens:\n      - sha256: %s\n        expires: %q\n", HashToken("tok"), expires.Format(time.RFC3339))
	path := writeFile(t, t.TempDir(), "users.yaml", []byte(content))
	clock := newFakeClock(expires.Add(-time.Second))
	a, _ := newTestStatic(t, path, 0, clock.Now)
	if _, err := authStatic(a, "tok"); err != nil {
		t.Fatalf("one second before expiry: %v", err)
	}
	clock.Add(time.Second)
	if _, err := authStatic(a, "tok"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("at expiry: error = %v, want ErrUnauthenticated", err)
	}
}

func TestStaticGroupsAreCopied(t *testing.T) {
	path := writeFile(t, t.TempDir(), "users.yaml", []byte(staticUsersYAML()))
	a, _ := newTestStatic(t, path, 0, func() time.Time { return staticNow })
	id, err := authStatic(a, "alice-token")
	if err != nil {
		t.Fatal(err)
	}
	id.Groups[0] = "changed"
	id, _ = authStatic(a, "alice-token")
	if id.Groups[0] != "analytics" {
		t.Fatalf("groups were shared: %v", id.Groups)
	}
}

func TestStaticLoadWarnings(t *testing.T) {
	path := writeFile(t, t.TempDir(), "users.yaml", []byte(staticUsersYAML()))
	_, buf := newTestStatic(t, path, 0, func() time.Time { return staticNow })
	logs := buf.String()
	for _, want := range []string{"plain-text token", "user=alice", "expired token", "user=carol", "users file loaded", "users=5", "tokens=5"} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "alice-plain-token") {
		t.Errorf("log contains a plain token:\n%s", logs)
	}
	// Disabled users' tokens are not reported as expired.
	if strings.Contains(logs, "user=bob") {
		t.Errorf("log mentions the disabled user:\n%s", logs)
	}
}

func TestParseUsersFileErrors(t *testing.T) {
	h1, h2 := HashToken("t1"), HashToken("t2")
	tests := []struct {
		name    string
		yaml    string
		wantErr []string
	}{
		{name: "empty file", yaml: "", wantErr: []string{`no "users" list`}},
		{name: "null users", yaml: "users:\n", wantErr: []string{`no "users" list`}},
		{name: "other document", yaml: "people: []\n", wantErr: []string{"unknown field"}},
		{name: "not YAML", yaml: "users: [\n", wantErr: []string{"parse users file"}},
		{name: "users is not a list", yaml: "users: alice\n", wantErr: []string{"parse users file"}},
		{name: "unknown user field", yaml: "users:\n  - name: a\n    disable: true\n", wantErr: []string{"unknown field", "disable"}},
		{name: "unknown token field", yaml: "users:\n  - name: a\n    tokens:\n      - sha265: " + h1 + "\n", wantErr: []string{"unknown field", "sha265"}},
		{name: "duplicate key", yaml: "users:\n  - name: a\n    name: b\n", wantErr: []string{"already set"}},
		{name: "empty name", yaml: "users:\n  - groups: [x]\n", wantErr: []string{`users[0]: name "" must not be empty`}},
		{name: "name with control character", yaml: "users:\n  - name: \"a\\tb\"\n", wantErr: []string{"control characters"}},
		{name: "duplicate user", yaml: "users:\n  - name: a\n  - name: b\n  - name: a\n", wantErr: []string{"users[2] (a): duplicate user name, first defined in users[0]"}},
		{name: "empty group", yaml: "users:\n  - name: a\n    groups: [x, \"\"]\n", wantErr: []string{`users[0] (a): groups[1] "" must not be empty`}},
		{name: "invalid hex", yaml: "users:\n  - name: a\n    tokens:\n      - sha256: xyz\n", wantErr: []string{"users[0] (a) tokens[0]: sha256 must be 64 hexadecimal digits"}},
		{name: "short hash", yaml: "users:\n  - name: a\n    tokens:\n      - sha256: " + h1[:63] + "\n", wantErr: []string{"64 hexadecimal digits"}},
		{name: "non-hex digit", yaml: "users:\n  - name: a\n    tokens:\n      - sha256: g" + h1[1:] + "\n", wantErr: []string{"64 hexadecimal digits"}},
		{name: "sha256 and plain", yaml: "users:\n  - name: a\n    tokens:\n      - sha256: " + h1 + "\n        plain: t1\n", wantErr: []string{"either sha256 or plain"}},
		{name: "neither sha256 nor plain", yaml: "users:\n  - name: a\n    tokens:\n      - description: x\n", wantErr: []string{"needs sha256 or plain"}},
		{name: "plain with question mark", yaml: "users:\n  - name: a\n    tokens:\n      - plain: \"a?b\"\n", wantErr: []string{"must not contain '?'"}},
		{name: "invalid expires", yaml: "users:\n  - name: a\n    tokens:\n      - sha256: " + h1 + "\n        expires: 2027-13-01\n", wantErr: []string{"invalid expires"}},
		{name: "date without time", yaml: "users:\n  - name: a\n    tokens:\n      - sha256: " + h1 + "\n        expires: \"2027-01-01\"\n", wantErr: []string{"invalid expires"}},
		{
			name:    "duplicate token across users",
			yaml:    "users:\n  - name: a\n    tokens:\n      - sha256: " + h1 + "\n  - name: b\n    tokens:\n      - sha256: " + h2 + "\n      - sha256: " + strings.ToUpper(h1) + "\n",
			wantErr: []string{"users[1] (b) tokens[1]: duplicate token, already used by users[0] (a) tokens[0]"},
		},
		{
			name:    "duplicate token within a user",
			yaml:    "users:\n  - name: a\n    tokens:\n      - sha256: " + h1 + "\n      - sha256: " + h1 + "\n",
			wantErr: []string{"users[0] (a) tokens[1]: duplicate token"},
		},
		{
			name:    "plain token equals a hashed one",
			yaml:    "users:\n  - name: a\n    tokens:\n      - sha256: " + h1 + "\n  - name: b\n    tokens:\n      - plain: t1\n",
			wantErr: []string{"users[1] (b) tokens[0]: duplicate token"},
		},
		{
			name:    "all errors are reported",
			yaml:    "users:\n  - name: \"\"\n  - name: b\n    tokens:\n      - sha256: nope\n",
			wantErr: []string{"users[0]: name", "users[1] (b) tokens[0]: sha256"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, _, err := parseUsersFile([]byte(tt.yaml), staticNow)
			if err == nil {
				t.Fatalf("no error, state %+v", st)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
			for _, secret := range []string{"t1", "t2"} {
				if strings.Contains(err.Error(), HashToken(secret)) {
					t.Errorf("error %q contains a token hash", err)
				}
			}
		})
	}
}

func TestParseUsersFileEmptyList(t *testing.T) {
	st, warnings, err := parseUsersFile([]byte("users: []\n"), staticNow)
	if err != nil {
		t.Fatal(err)
	}
	if st.users != 0 || len(st.tokens) != 0 {
		t.Fatalf("state = %+v", st)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0].msg, "no users") {
		t.Fatalf("warnings = %+v", warnings)
	}
}

func TestStaticReloadOnChange(t *testing.T) {
	path := writeFile(t, t.TempDir(), "users.yaml", nil)
	rewrite(t, path, "users:\n  - name: alice\n    tokens:\n      - plain: token-1\n", 1)
	a, buf := newTestStatic(t, path, 10*time.Millisecond, time.Now)
	if _, err := authStatic(a, "token-1"); err != nil {
		t.Fatal(err)
	}

	rewrite(t, path, "users:\n  - name: bob\n    groups: [g]\n    tokens:\n      - plain: token-2\n", 2)
	eventually(t, 5*time.Second, func() bool { _, err := authStatic(a, "token-2"); return err == nil }, "the reloaded token")
	if _, err := authStatic(a, "token-1"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("removed token still works: %v", err)
	}
	id, _ := authStatic(a, "token-2")
	if id.User != "bob" || !reflect.DeepEqual(id.Groups, []string{"g"}) {
		t.Fatalf("identity = %+v", id)
	}
	if !strings.Contains(buf.String(), "users file reloaded") {
		t.Errorf("log lacks the reload:\n%s", buf)
	}
}

func TestStaticInvalidReloadKeepsState(t *testing.T) {
	path := writeFile(t, t.TempDir(), "users.yaml", nil)
	rewrite(t, path, "users:\n  - name: alice\n    tokens:\n      - plain: token-1\n", 1)
	a, buf := newTestStatic(t, path, 10*time.Millisecond, time.Now)

	rewrite(t, path, "users:\n  - name: alice\n    tokens:\n      - sha256: not-a-hash\n", 2)
	eventually(t, 5*time.Second, func() bool { return strings.Contains(buf.String(), "reload failed") }, "the reload error")
	if _, err := authStatic(a, "token-1"); err != nil {
		t.Fatalf("old token rejected after an invalid reload: %v", err)
	}
	// The broken version is reported once, not on every poll.
	time.Sleep(60 * time.Millisecond)
	if n := strings.Count(buf.String(), "reload failed"); n != 1 {
		t.Errorf("reload error logged %d times:\n%s", n, buf)
	}

	rewrite(t, path, "users:\n  - name: alice\n    tokens:\n      - plain: token-3\n", 3)
	eventually(t, 5*time.Second, func() bool { _, err := authStatic(a, "token-3"); return err == nil }, "the repaired file")
	eventually(t, 5*time.Second, func() bool { return strings.Contains(buf.String(), "usable again") }, "the recovery log")
}

func TestStaticFileRemovedKeepsState(t *testing.T) {
	path := writeFile(t, t.TempDir(), "users.yaml", nil)
	rewrite(t, path, "users:\n  - name: alice\n    tokens:\n      - plain: token-1\n", 1)
	a, buf := newTestStatic(t, path, 10*time.Millisecond, time.Now)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, func() bool { return strings.Contains(buf.String(), "not accessible") }, "the stat error")
	time.Sleep(60 * time.Millisecond)
	if n := strings.Count(buf.String(), "not accessible"); n != 1 {
		t.Errorf("stat error logged %d times", n)
	}
	if _, err := authStatic(a, "token-1"); err != nil {
		t.Fatalf("old token rejected while the file is missing: %v", err)
	}

	// The file comes back with the same content and time stamp: no reload
	// is needed, but polling must recover.
	rewrite(t, path, "users:\n  - name: alice\n    tokens:\n      - plain: token-1\n", 1)
	eventually(t, 5*time.Second, func() bool { return strings.Contains(buf.String(), "usable again") }, "the recovery")
	if _, err := authStatic(a, "token-1"); err != nil {
		t.Fatal(err)
	}
}

func TestStaticPollSkipsUnchangedContent(t *testing.T) {
	path := writeFile(t, t.TempDir(), "users.yaml", nil)
	content := "users:\n  - name: alice\n    tokens:\n      - plain: token-1\n"
	rewrite(t, path, content, 1)
	a, buf := newTestStatic(t, path, 0, time.Now)
	before := a.state.Load()

	rewrite(t, path, content, 2) // touched only
	a.poll()
	if a.state.Load() != before {
		t.Error("identical content was re-published")
	}
	if strings.Contains(buf.String(), "reloaded") {
		t.Errorf("identical content was reloaded:\n%s", buf)
	}

	rewrite(t, path, content+"  - name: bob\n", 3)
	a.poll()
	if a.state.Load() == before || a.state.Load().users != 2 {
		t.Error("changed content was not loaded")
	}
}

func TestStaticReloadMethod(t *testing.T) {
	path := writeFile(t, t.TempDir(), "users.yaml", nil)
	rewrite(t, path, "users:\n  - name: alice\n    tokens:\n      - plain: token-1\n", 1)
	a, _ := newTestStatic(t, path, 0, time.Now) // no watcher

	var r Reloader = a                                                                    // static authenticators support forced reloads
	rewrite(t, path, "users:\n  - name: alice\n    tokens:\n      - plain: token-2\n", 1) // same stamp
	if _, err := authStatic(a, "token-2"); err == nil {
		t.Fatal("reloaded without a watcher")
	}
	if err := r.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, err := authStatic(a, "token-2"); err != nil {
		t.Fatalf("after Reload: %v", err)
	}

	rewrite(t, path, "users: {\n", 2)
	if err := r.Reload(); err == nil || !strings.Contains(err.Error(), "parse users file") {
		t.Fatalf("Reload of an invalid file: %v", err)
	}
	if _, err := authStatic(a, "token-2"); err != nil {
		t.Fatalf("an invalid Reload dropped the users: %v", err)
	}
}

func TestStaticCloseStopsWatcher(t *testing.T) {
	path := writeFile(t, t.TempDir(), "users.yaml", nil)
	rewrite(t, path, "users:\n  - name: alice\n    tokens:\n      - plain: token-1\n", 1)
	log, _ := testLogger()
	a, err := newStatic(config.StaticAuthConfig{File: path, ReloadInterval: config.D(5 * time.Millisecond)}, log, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 3 { // concurrent and repeated Close calls are fine
		wg.Add(1)
		go func() { defer wg.Done(); _ = a.Close() }()
	}
	wg.Wait()
	select {
	case <-a.done:
	default:
		t.Fatal("watcher still running after Close")
	}
	rewrite(t, path, "users:\n  - name: alice\n    tokens:\n      - plain: token-2\n", 2)
	time.Sleep(30 * time.Millisecond)
	if _, err := authStatic(a, "token-2"); err == nil {
		t.Fatal("file reloaded after Close")
	}
	if _, err := authStatic(a, "token-1"); err != nil {
		t.Fatalf("Close dropped the users: %v", err)
	}
}

func TestStaticDefaultReloadInterval(t *testing.T) {
	path := writeFile(t, t.TempDir(), "users.yaml", []byte("users: []\n"))
	a, err := newStatic(config.StaticAuthConfig{File: path}, discardLogger(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if a.done == nil {
		t.Error("no watcher for an unset reload interval (default 10s)")
	}
	a.Close()
	if _, err := newStatic(config.StaticAuthConfig{File: path, ReloadInterval: config.D(-time.Second)}, discardLogger(), time.Now); err == nil {
		t.Error("negative reload interval accepted")
	}
}

func TestStaticFileTooLarge(t *testing.T) {
	old := maxUsersFileBytes
	maxUsersFileBytes = 64
	defer func() { maxUsersFileBytes = old }()
	path := writeFile(t, t.TempDir(), "users.yaml", []byte("users:\n"+strings.Repeat("  - name: someone\n", 10)))
	_, err := newStatic(config.StaticAuthConfig{File: path, ReloadInterval: config.D(0)}, discardLogger(), time.Now)
	if err == nil || !strings.Contains(err.Error(), "larger than 64 bytes") {
		t.Fatalf("error = %v", err)
	}
}

func TestExampleUsersFile(t *testing.T) {
	path := filepath.Join("testdata", "users.example.yaml")
	if err := ValidateUsersFile(path); err != nil {
		t.Fatal(err)
	}
	clock := newFakeClock(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	a, _ := newTestStatic(t, path, 0, clock.Now)
	tests := []struct {
		token, user string
		groups      []string
		wantErr     string
	}{
		{token: "example-alice-laptop", user: "alice", groups: []string{"analytics", "admins"}},
		{token: "dev-only-token", user: "alice", groups: []string{"analytics", "admins"}},
		{token: "example-bob-ci", user: "bob", groups: []string{"analytics"}},
		{token: "example-mallory", wantErr: "disabled"},
	}
	for _, tt := range tests {
		id, err := authStatic(a, tt.token)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("%s: error = %v, want %q", tt.token, err, tt.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tt.token, err)
			continue
		}
		if id.User != tt.user || !reflect.DeepEqual(id.Groups, tt.groups) {
			t.Errorf("%s: identity = %+v", tt.token, id)
		}
	}
	clock.Add(365 * 24 * time.Hour) // past 2027-01-01
	if _, err := authStatic(a, "example-alice-laptop"); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired example token: %v", err)
	}
}

func TestValidateUsersFile(t *testing.T) {
	dir := t.TempDir()
	if err := ValidateUsersFile(writeFile(t, dir, "ok.yaml", []byte("users: []\n"))); err != nil {
		t.Errorf("valid file: %v", err)
	}
	bad := writeFile(t, dir, "bad.yaml", []byte("users:\n  - name: a\n  - name: a\n"))
	if err := ValidateUsersFile(bad); err == nil || !strings.Contains(err.Error(), "bad.yaml") {
		t.Errorf("invalid file: %v", err)
	}
	if err := ValidateUsersFile(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestHashToken(t *testing.T) {
	vectors := map[string]string{
		"":    "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"abc": "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
	}
	for in, want := range vectors {
		if got := HashToken(in); got != want {
			t.Errorf("HashToken(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestGenerateToken(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		tok, err := GenerateToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) != 43 {
			t.Fatalf("token %q has %d characters, want 43", tok, len(tok))
		}
		raw, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil || len(raw) != 32 {
			t.Fatalf("token %q is not 32 base64url bytes: %v", tok, err)
		}
		if strings.ContainsAny(tok, "?&=+/") {
			t.Fatalf("token %q contains characters that need escaping", tok)
		}
		if cred, opts, err := ParseToken(tok); err != nil || cred != tok || len(opts) != 0 {
			t.Fatalf("ParseToken(%q) = %q, %v, %v", tok, cred, opts, err)
		}
		if seen[tok] {
			t.Fatalf("duplicate token %q", tok)
		}
		seen[tok] = true
	}
}

// The token of the example users file shipped with the manifests is public:
// it never authenticates anybody, and the gateway says so.
func TestStaticIgnoresTheExampleToken(t *testing.T) {
	yaml := "users:\n  - name: alice\n    tokens:\n      - sha256: \"" + exampleTokenSHA256 + "\"\n"
	path := writeFile(t, t.TempDir(), "users.yaml", []byte(yaml))
	a, buf := newTestStatic(t, path, 0, func() time.Time { return staticNow })
	if _, err := a.Authenticate(context.Background(), Request{Credential: "change-me-example-token-0000"}); err == nil {
		t.Fatal("the public example token authenticated")
	}
	if !strings.Contains(buf.String(), "public example token") {
		t.Fatalf("no warning about the example token:\n%s", buf.String())
	}
}
