package auth

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"quackmux/internal/config"
)

// defaultNoneUser is the user of auth.mode=none when neither the client nor
// the configuration names one.
const defaultNoneUser = "anonymous"

// noneAuthenticator accepts every credential (development only).
type noneAuthenticator struct {
	defaultUser string
	groups      []string
}

func newNone(cfg config.NoneAuthConfig, log *slog.Logger) (*noneAuthenticator, error) {
	user := cfg.DefaultUser
	if user == "" {
		user = defaultNoneUser
	}
	if err := checkName(user); err != nil {
		return nil, fmt.Errorf("auth.none.defaultUser %q %v", user, err)
	}
	for i, g := range cfg.DefaultGroups {
		if err := checkName(g); err != nil {
			return nil, fmt.Errorf("auth.none.defaultGroups[%d] %q %v", i, g, err)
		}
	}
	a := &noneAuthenticator{defaultUser: user, groups: dedupe(cfg.DefaultGroups)}
	log.Warn("authentication is disabled (auth.mode=none): every client is accepted and may choose its user name; use this only for development",
		"defaultUser", a.defaultUser, "defaultGroups", a.groups)
	return a, nil
}

// Authenticate implements Authenticator. The credential is ignored; the
// user comes from the "user" option or the configured default.
func (a *noneAuthenticator) Authenticate(_ context.Context, req Request) (Identity, error) {
	user := a.defaultUser
	if u := req.Options.Get("user"); u != "" {
		if !validOptionUser(u) {
			return Identity{}, unauthenticated("invalid user name in option \"user\" (want 1-%d characters of A-Z, a-z, 0-9, '.', '_', '@', '-')", maxOptionUserLen)
		}
		user = u
	}
	return Identity{User: user, Groups: slices.Clone(a.groups), Method: MethodNone}, nil
}

// Close implements Authenticator.
func (a *noneAuthenticator) Close() error { return nil }
