package auth

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"quackmux/internal/config"
)

// New builds the authenticator selected by cfg.Mode ("none", "static" or
// "jwt"). Unset fields get the same defaults as in config.ApplyDefaults.
// The caller must Close the authenticator to stop its background work
// (users file polling, JWKS refresh).
func New(cfg config.AuthConfig, log *slog.Logger) (Authenticator, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = log.With("component", "auth")
	switch mode := strings.ToLower(strings.TrimSpace(cfg.Mode)); mode {
	case MethodNone:
		return newNone(cfg.None, log)
	case MethodStatic:
		return newStatic(cfg.Static, log, time.Now)
	case MethodJWT:
		return newJWT(cfg.JWT, log, defaultJWTOptions())
	default:
		return nil, fmt.Errorf("auth.mode must be none, static or jwt, got %q", cfg.Mode)
	}
}

// unauthenticated wraps ErrUnauthenticated with a reason for the logs. The
// reason must never contain the credential.
func unauthenticated(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnauthenticated, fmt.Sprintf(format, args...))
}

// durationOr returns d, or def if d was not configured explicitly.
func durationOr(d config.Duration, def time.Duration) time.Duration {
	if !d.IsSet() {
		return def
	}
	return d.Duration
}

// maxNameLen bounds user and group names from trusted sources.
const maxNameLen = 256

// checkName validates a user or group name that comes from a trusted source
// (the users file, the configuration or a verified JWT). The returned error
// completes a sentence like `user "x" ...`.
func checkName(s string) error {
	switch {
	case s == "":
		return errors.New("must not be empty")
	case len(s) > maxNameLen:
		return fmt.Errorf("is longer than %d bytes", maxNameLen)
	case !utf8.ValidString(s):
		return errors.New("is not valid UTF-8")
	case strings.TrimSpace(s) != s:
		return errors.New("has leading or trailing white space")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return errors.New("contains control characters")
		}
	}
	return nil
}

// maxOptionUserLen bounds user names chosen by clients (auth.mode=none).
const maxOptionUserLen = 64

// validOptionUser reports whether s may be used as a user name that the
// client chose itself: 1-64 characters of [A-Za-z0-9._@-].
func validOptionUser(s string) bool {
	if s == "" || len(s) > maxOptionUserLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '@', c == '-':
		default:
			return false
		}
	}
	return true
}

// dedupe returns the distinct elements of in, in order of first appearance.
// It returns nil for an empty input.
func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// readFileLimited reads a file but refuses files larger than limit bytes (a
// misconfigured path such as /dev/zero must not exhaust memory).
func readFileLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	return data, nil
}
