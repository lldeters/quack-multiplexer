package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"sigs.k8s.io/yaml"

	"quackmux/internal/config"
)

// defaultReloadInterval is the users file polling period when
// auth.static.reloadInterval is not set.
const defaultReloadInterval = 10 * time.Second

// generatedTokenBytes is the entropy of tokens made by GenerateToken.
const generatedTokenBytes = 32

// maxUsersFileBytes bounds the size of the users file (a variable for tests).
var maxUsersFileBytes int64 = 16 << 20

// HashToken returns the lower-case hex encoded SHA-256 of a token, the form
// in which the users file stores tokens.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// GenerateToken returns a new random token: 32 bytes from crypto/rand,
// base64url encoded without padding (43 characters that need no escaping in
// tokens, URLs or YAML).
func GenerateToken() (string, error) {
	b := make([]byte, generatedTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ValidateUsersFile reads and validates a users file (see the package
// documentation) without starting an authenticator.
func ValidateUsersFile(path string) error {
	data, err := readFileLimited(path, maxUsersFileBytes)
	if err != nil {
		return err
	}
	if _, _, err := parseUsersFile(data, time.Now()); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// usersFile is the layout of the users file.
type usersFile struct {
	// Users is a pointer to tell a missing list (an error) from an empty one.
	Users *[]userSpec `json:"users"`
}

type userSpec struct {
	Name     string      `json:"name"`
	Groups   []string    `json:"groups,omitempty"`
	Tokens   []tokenSpec `json:"tokens,omitempty"`
	Disabled bool        `json:"disabled,omitempty"`
}

type tokenSpec struct {
	SHA256      string `json:"sha256,omitempty"`
	Plain       string `json:"plain,omitempty"`
	Description string `json:"description,omitempty"`
	// Expires is an optional RFC 3339 timestamp.
	Expires string `json:"expires,omitempty"`
}

// staticUser and staticToken are the validated form of the users file.
type staticUser struct {
	name     string
	groups   []string
	disabled bool
}

type staticToken struct {
	user *staticUser
	// label names the token in logs: its description or "#<position>".
	label   string
	expires time.Time // zero: never
}

// staticState is one loaded version of the users file. It is immutable once
// published.
type staticState struct {
	// tokens maps the lower-case hex sha256 of a token to the token.
	tokens map[string]*staticToken
	users  int
	// digest of the file content, to skip reloads that change nothing.
	digest [sha256.Size]byte
}

// logEntry is a log message whose output is deferred until a users file has
// been accepted.
type logEntry struct {
	msg  string
	args []any
}

// parseUsersFile decodes and validates a users file. now is used to warn
// about expired tokens.
func parseUsersFile(data []byte, now time.Time) (*staticState, []logEntry, error) {
	var f usersFile
	if err := yaml.UnmarshalStrict(data, &f); err != nil {
		return nil, nil, fmt.Errorf("parse users file: %w", err)
	}
	if f.Users == nil {
		return nil, nil, errors.New(`users file has no "users" list (write "users: []" to reject everybody)`)
	}
	st := &staticState{tokens: map[string]*staticToken{}}
	var (
		errs     []error
		warnings []logEntry
		userAt   = map[string]int{}    // user name -> index
		tokenAt  = map[string]string{} // token hash -> where it was defined
	)
	for ui, u := range *f.Users {
		where := fmt.Sprintf("users[%d]", ui)
		if err := checkName(u.Name); err != nil {
			errs = append(errs, fmt.Errorf("%s: name %q %v", where, u.Name, err))
			continue
		}
		where = fmt.Sprintf("users[%d] (%s)", ui, u.Name)
		if prev, dup := userAt[u.Name]; dup {
			errs = append(errs, fmt.Errorf("%s: duplicate user name, first defined in users[%d]", where, prev))
			continue
		}
		userAt[u.Name] = ui
		for gi, g := range u.Groups {
			if err := checkName(g); err != nil {
				errs = append(errs, fmt.Errorf("%s: groups[%d] %q %v", where, gi, g, err))
			}
		}
		user := &staticUser{name: u.Name, groups: dedupe(u.Groups), disabled: u.Disabled}
		st.users++
		for ti, t := range u.Tokens {
			twhere := fmt.Sprintf("%s tokens[%d]", where, ti)
			hash, plain, err := tokenHash(t)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", twhere, err))
				continue
			}
			var expires time.Time
			if s := strings.TrimSpace(t.Expires); s != "" {
				if expires, err = time.Parse(time.RFC3339, s); err != nil {
					errs = append(errs, fmt.Errorf("%s: invalid expires (want RFC 3339 like 2027-01-01T00:00:00Z): %v", twhere, err))
					continue
				}
			}
			if prev, dup := tokenAt[hash]; dup {
				errs = append(errs, fmt.Errorf("%s: duplicate token, already used by %s", twhere, prev))
				continue
			}
			tokenAt[hash] = twhere
			if hash == exampleTokenSHA256 {
				// The token of the shipped example file is public: never
				// let it authenticate anybody.
				warnings = append(warnings, logEntry{
					"users file contains the public example token from the repository; it is ignored",
					[]any{"user", u.Name, "token", tokenLabel(t, ti)},
				})
				continue
			}
			tok := &staticToken{user: user, label: tokenLabel(t, ti), expires: expires}
			st.tokens[hash] = tok
			if plain {
				warnings = append(warnings, logEntry{
					"users file contains a plain-text token; store its sha256 instead (quackmux hash-token)",
					[]any{"user", u.Name, "token", tok.label},
				})
			}
			if !expires.IsZero() && !now.Before(expires) && !u.Disabled {
				warnings = append(warnings, logEntry{
					"users file contains an expired token",
					[]any{"user", u.Name, "token", tok.label, "expired", expires},
				})
			}
		}
	}
	if len(errs) > 0 {
		return nil, nil, fmt.Errorf("invalid users file: %w", errors.Join(errs...))
	}
	if st.users == 0 {
		warnings = append(warnings, logEntry{"users file defines no users; every client is rejected", nil})
	}
	return st, warnings, nil
}

// exampleTokenSHA256 is the sha256 of "change-me-example-token-0000", the
// token of the example users file in deploy/kubernetes/base/secrets.
const exampleTokenSHA256 = "cef58a2821dd799a53522c76b3e602d8534f044586a2eb12a890e09e3a8f2de2"

// tokenHash returns the lookup key of a token entry and whether the entry
// holds the token in plain text.
func tokenHash(t tokenSpec) (hash string, plain bool, err error) {
	switch {
	case t.SHA256 != "" && t.Plain != "":
		return "", false, errors.New("set either sha256 or plain, not both")
	case t.SHA256 != "":
		h := strings.ToLower(strings.TrimSpace(t.SHA256))
		if !isHexSHA256(h) {
			return "", false, errors.New("sha256 must be 64 hexadecimal digits")
		}
		return h, false, nil
	case t.Plain != "":
		if strings.Contains(t.Plain, "?") {
			return "", false, errors.New("plain token must not contain '?' (it starts the options suffix)")
		}
		return HashToken(t.Plain), true, nil
	default:
		return "", false, errors.New("needs sha256 or plain")
	}
}

// isHexSHA256 reports whether s is a lower-case hex encoded SHA-256 digest.
func isHexSHA256(s string) bool {
	if len(s) != hex.EncodedLen(sha256.Size) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func tokenLabel(t tokenSpec, index int) string {
	if t.Description != "" {
		return t.Description
	}
	return fmt.Sprintf("#%d", index+1)
}

// staticAuthenticator authenticates tokens listed in the users file.
type staticAuthenticator struct {
	path string
	log  *slog.Logger
	now  func() time.Time

	state atomic.Pointer[staticState]

	mu      sync.Mutex // serializes loads; guards stamp and lastErr
	stamp   fileStamp  // of the last file version that was loaded or tried
	lastErr string     // last reported polling error, to avoid repeating it

	stop      chan struct{}
	done      chan struct{} // closed when the watcher exits; nil without watcher
	closeOnce sync.Once
}

// fileStamp identifies a version of the users file.
type fileStamp struct {
	modTime time.Time
	size    int64
}

func stampOf(fi os.FileInfo) fileStamp { return fileStamp{modTime: fi.ModTime(), size: fi.Size()} }

func (s fileStamp) equal(o fileStamp) bool { return s.size == o.size && s.modTime.Equal(o.modTime) }

func newStatic(cfg config.StaticAuthConfig, log *slog.Logger, now func() time.Time) (*staticAuthenticator, error) {
	if cfg.File == "" {
		return nil, errors.New("auth.static.file is required for auth.mode=static")
	}
	interval := durationOr(cfg.ReloadInterval, defaultReloadInterval)
	if interval < 0 {
		return nil, fmt.Errorf("auth.static.reloadInterval must not be negative, got %s", interval)
	}
	a := &staticAuthenticator{path: cfg.File, log: log, now: now, stop: make(chan struct{})}
	a.mu.Lock()
	err := a.reloadLocked()
	a.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("load users file: %w", err)
	}
	if interval > 0 {
		a.done = make(chan struct{})
		go a.watch(interval)
	}
	return a, nil
}

// Authenticate implements Authenticator.
func (a *staticAuthenticator) Authenticate(_ context.Context, req Request) (Identity, error) {
	if req.Credential == "" {
		return Identity{}, unauthenticated("empty credential")
	}
	tok, ok := a.state.Load().tokens[HashToken(req.Credential)]
	switch {
	case !ok:
		return Identity{}, unauthenticated("unknown token")
	case tok.user.disabled:
		return Identity{}, unauthenticated("user %q is disabled", tok.user.name)
	case !tok.expires.IsZero() && !a.now().Before(tok.expires):
		return Identity{}, unauthenticated("token %q of user %q expired at %s", tok.label, tok.user.name, tok.expires.Format(time.RFC3339))
	}
	return Identity{User: tok.user.name, Groups: slices.Clone(tok.user.groups), Method: MethodStatic}, nil
}

// Reload implements Reloader: it re-reads the users file even if it looks
// unchanged. If the file is invalid, the previous users stay active.
func (a *staticAuthenticator) Reload() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.reloadLocked(); err != nil {
		return err
	}
	a.lastErr = ""
	return nil
}

// Close implements Authenticator. It stops the file watcher.
func (a *staticAuthenticator) Close() error {
	a.closeOnce.Do(func() { close(a.stop) })
	if a.done != nil {
		<-a.done
	}
	return nil
}

func (a *staticAuthenticator) watch(interval time.Duration) {
	defer close(a.done)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-a.stop:
			return
		case <-t.C:
			a.poll()
		}
	}
}

// poll loads the users file if its modification time or size changed.
func (a *staticAuthenticator) poll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	fi, err := os.Stat(a.path)
	if err != nil {
		// Forget the stamp so that the file is loaded when it comes back.
		a.stamp = fileStamp{}
		if msg := err.Error(); msg != a.lastErr {
			a.lastErr = msg
			a.log.Error("users file is not accessible; keeping the previous users", "file", a.path, "err", err)
		}
		return
	}
	st := stampOf(fi)
	if st.equal(a.stamp) {
		return
	}
	// Remember the stamp even if loading fails: an invalid version is
	// reported once, and loading is retried when the file changes again.
	a.stamp = st
	if err := a.loadLocked(); err != nil {
		a.lastErr = err.Error()
		a.log.Error("users file reload failed; keeping the previous users", "file", a.path, "err", err)
		return
	}
	if a.lastErr != "" {
		a.lastErr = ""
		a.log.Info("users file is usable again", "file", a.path)
	}
}

// reloadLocked records the file's stamp and loads it. a.mu must be held.
func (a *staticAuthenticator) reloadLocked() error {
	fi, err := os.Stat(a.path)
	if err != nil {
		a.stamp = fileStamp{}
		return err
	}
	a.stamp = stampOf(fi)
	return a.loadLocked()
}

// loadLocked reads, validates and publishes the users file. a.mu must be
// held. On error the current state is kept.
func (a *staticAuthenticator) loadLocked() error {
	data, err := readFileLimited(a.path, maxUsersFileBytes)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	old := a.state.Load()
	if old != nil && old.digest == digest {
		return nil
	}
	st, warnings, err := parseUsersFile(data, a.now())
	if err != nil {
		return fmt.Errorf("%s: %w", a.path, err)
	}
	st.digest = digest
	a.state.Store(st)
	for _, w := range warnings {
		a.log.Warn(w.msg, append([]any{"file", a.path}, w.args...)...)
	}
	msg := "users file loaded"
	if old != nil {
		msg = "users file reloaded"
	}
	a.log.Info(msg, "file", a.path, "users", st.users, "tokens", len(st.tokens))
	return nil
}
