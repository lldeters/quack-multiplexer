package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Identity is an authenticated principal.
type Identity struct {
	User   string
	Groups []string
	// Method names the authenticator that produced the identity.
	Method string
}

// Authentication methods (Identity.Method).
const (
	MethodNone   = "none"
	MethodStatic = "static"
	MethodJWT    = "jwt"
)

// InGroup reports whether the identity belongs to group g.
func (i Identity) InGroup(g string) bool {
	for _, have := range i.Groups {
		if have == g {
			return true
		}
	}
	return false
}

// ErrUnauthenticated is returned (possibly wrapped) when a credential is
// missing, unknown, expired or otherwise invalid. Its text is safe to show to
// clients; details belong in logs only.
var ErrUnauthenticated = errors.New("authentication failed")

// Request carries what an authenticator may look at.
type Request struct {
	// Credential is the token without its option suffix.
	Credential string
	// Options are the options parsed from the token suffix (read-only).
	Options url.Values
	// HTTP is the incoming HTTP request (headers, remote address).
	HTTP *http.Request
}

// Authenticator validates credentials. Implementations must be safe for
// concurrent use.
type Authenticator interface {
	Authenticate(ctx context.Context, req Request) (Identity, error)
	// Close releases background resources (file watchers, JWKS refresh).
	Close() error
}

// Reloader is implemented by authenticators that can re-read their
// configuration on demand, for example on SIGHUP (auth.mode=static). On
// error the previous configuration stays active.
type Reloader interface {
	Reload() error
}

// ParseToken splits a Quack token into the credential and the options that
// follow the first '?', encoded like a URL query:
//
//	secret-token?share_level=connection&profile=large
//
// Option names are case-insensitive and normalized to lower case with '-'
// replaced by '_'. Values keep the order in which they appear, also across
// differently spelled names of the same option ("Profile=a&profile=b").
func ParseToken(token string) (credential string, opts url.Values, err error) {
	credential, query, found := strings.Cut(token, "?")
	opts = url.Values{}
	if !found || query == "" {
		return credential, opts, nil
	}
	// Same syntax as url.ParseQuery, which however loses the relative order
	// of names that only become equal after normalization.
	for query != "" {
		var pair string
		pair, query, _ = strings.Cut(query, "&")
		if strings.Contains(pair, ";") {
			return "", nil, errors.New("invalid options in token: invalid semicolon separator")
		}
		if pair == "" {
			continue
		}
		rawName, rawValue, _ := strings.Cut(pair, "=")
		name, err := url.QueryUnescape(rawName)
		if err != nil {
			return "", nil, fmt.Errorf("invalid options in token: %w", err)
		}
		value, err := url.QueryUnescape(rawValue)
		if err != nil {
			return "", nil, fmt.Errorf("invalid options in token: %w", err)
		}
		name = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "-", "_")
		opts[name] = append(opts[name], value)
	}
	return credential, opts, nil
}
