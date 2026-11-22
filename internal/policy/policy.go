// Package policy resolves the routing options of a new session (share level,
// subdomain, profile, group) from the client's token options and HTTP
// headers, the configured defaults and overrides, and checks them against
// the access rules.
package policy

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"quackmux/internal/auth"
	"quackmux/internal/config"
	"quackmux/internal/engine"
)

// HeaderPrefix is the prefix of option headers, e.g. X-Quackmux-Share-Level.
const HeaderPrefix = "X-Quackmux-"

// Option names (token suffix keys; headers use the dashed, title-cased form).
const (
	OptShareLevel = "share_level"
	OptSubdomain  = "subdomain"
	OptProfile    = "profile"
	OptGroup      = "group"
	OptUser       = "user"
)

var knownOptions = map[string]bool{OptShareLevel: true, OptSubdomain: true, OptProfile: true, OptGroup: true, OptUser: true}

// ErrDenied wraps policy violations; the message is safe to show to clients.
var ErrDenied = errors.New("not permitted")

// Decision is the resolved routing of a new session.
type Decision struct {
	Key engine.Key
	// MaxSessions is the per-user session limit that applies (0 = unlimited).
	MaxSessions int
}

// Options collects the options a client sent: token options win over headers.
func Options(tokenOpts url.Values, h http.Header) (map[string]string, error) {
	out := map[string]string{}
	for name := range knownOptions {
		hdr := HeaderPrefix + strings.ReplaceAll(name, "_", "-")
		if v := strings.TrimSpace(h.Get(hdr)); v != "" {
			out[name] = v
		}
	}
	for name, vals := range tokenOpts {
		if !knownOptions[name] {
			return nil, fmt.Errorf("unknown option %q in token (known: share_level, subdomain, profile, group, user)", name)
		}
		if len(vals) > 0 {
			out[name] = strings.TrimSpace(vals[len(vals)-1])
		}
	}
	return out, nil
}

// Resolve computes the engine key for a new session. sessionID is the owner
// for CONNECTION share level.
func Resolve(cfg *config.Config, id auth.Identity, opts map[string]string, sessionID string) (Decision, error) {
	var ov *config.Override
	for i := range cfg.Overrides {
		if cfg.Overrides[i].Matches(id.User, id.Groups) {
			ov = &cfg.Overrides[i]
			break
		}
	}
	pick := func(opt, overrideDefault, globalDefault string) string {
		if v := opts[opt]; v != "" {
			return v
		}
		if overrideDefault != "" {
			return overrideDefault
		}
		return globalDefault
	}
	var od config.SessionOptions
	if ov != nil {
		od = ov.Defaults
	}
	levelStr := pick(OptShareLevel, od.ShareLevel, cfg.Defaults.ShareLevel)
	level, err := engine.ParseShareLevel(levelStr)
	if err != nil {
		return Decision{}, fmt.Errorf("%w: %v", ErrDenied, err)
	}
	profileName := pick(OptProfile, od.Profile, cfg.Defaults.Profile)
	profile, ok := cfg.Profiles[profileName]
	if !ok {
		return Decision{}, fmt.Errorf("%w: unknown profile %q", ErrDenied, profileName)
	}
	subdomain := strings.ToLower(pick(OptSubdomain, od.Subdomain, cfg.Defaults.Subdomain))
	if !engine.ValidSubdomain(subdomain) {
		return Decision{}, fmt.Errorf("%w: invalid subdomain %q (lower-case letters, digits and '-', at most 32 characters)", ErrDenied, subdomain)
	}

	// Access rules: the profile's and the matching override's.
	if !allowedLevel(profile.AllowedShareLevels, level) {
		return Decision{}, fmt.Errorf("%w: profile %q does not allow share level %s", ErrDenied, profileName, level)
	}
	if !allowedPrincipal(profile.AllowedUsers, profile.AllowedGroups, id) {
		return Decision{}, fmt.Errorf("%w: user %q may not use profile %q", ErrDenied, id.User, profileName)
	}
	// CONNECTION engines have no use for a subdomain (it is dropped below).
	if level != engine.ShareConnection && len(profile.AllowedSubdomains) > 0 && !contains(profile.AllowedSubdomains, subdomain) {
		return Decision{}, fmt.Errorf("%w: profile %q does not allow subdomain %q (allowed: %s)", ErrDenied, profileName,
			subdomain, strings.Join(profile.AllowedSubdomains, ", "))
	}
	if ov != nil {
		if !allowedLevel(ov.AllowedShareLevels, level) {
			return Decision{}, fmt.Errorf("%w: share level %s is not allowed for user %q", ErrDenied, level, id.User)
		}
		if len(ov.AllowedProfiles) > 0 && !contains(ov.AllowedProfiles, profileName) {
			return Decision{}, fmt.Errorf("%w: profile %q is not allowed for user %q", ErrDenied, profileName, id.User)
		}
	}

	key := engine.Key{ShareLevel: level, Subdomain: subdomain, Profile: profileName}
	switch level {
	case engine.ShareConnection:
		key.Owner = sessionID
		key.Subdomain = "" // a dedicated engine has no use for a subdomain
	case engine.ShareUser:
		key.Owner = id.User
	case engine.ShareGroup:
		group := opts[OptGroup]
		if group == "" {
			if len(id.Groups) == 0 {
				return Decision{}, fmt.Errorf("%w: share level GROUP needs a group, but user %q has none", ErrDenied, id.User)
			}
			group = id.Groups[0]
		} else if !id.InGroup(group) {
			return Decision{}, fmt.Errorf("%w: user %q is not a member of group %q", ErrDenied, id.User, group)
		}
		key.Owner = group
	case engine.ShareServer:
		key.Owner = ""
	}

	d := Decision{Key: key, MaxSessions: cfg.Limits.MaxSessionsPerUser}
	if ov != nil && ov.MaxSessions > 0 {
		d.MaxSessions = ov.MaxSessions
	}
	return d, nil
}

func allowedLevel(allowed []string, level engine.ShareLevel) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if strings.EqualFold(a, string(level)) {
			return true
		}
	}
	return false
}

func allowedPrincipal(users, groups []string, id auth.Identity) bool {
	if len(users) == 0 && len(groups) == 0 {
		return true
	}
	for _, u := range users {
		if u == id.User || u == "*" {
			return true
		}
	}
	for _, g := range groups {
		if id.InGroup(g) {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
