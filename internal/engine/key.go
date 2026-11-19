// Package engine manages engines: DuckDB processes running a Quack server
// (normally one Kubernetes pod each) that the gateway routes sessions to.
package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// ShareLevel decides which sessions share an engine (as in Apache Kyuubi).
type ShareLevel string

const (
	// ShareConnection gives every session (every ATTACH) its own engine. The
	// engine is terminated when the session ends.
	ShareConnection ShareLevel = "CONNECTION"
	// ShareUser shares one engine between all sessions of the same user.
	ShareUser ShareLevel = "USER"
	// ShareGroup shares one engine between all sessions of the same group.
	ShareGroup ShareLevel = "GROUP"
	// ShareServer shares one engine between all sessions of all users.
	ShareServer ShareLevel = "SERVER"
)

// ShareLevels lists all share levels in order of decreasing isolation.
var ShareLevels = []ShareLevel{ShareConnection, ShareUser, ShareGroup, ShareServer}

// ParseShareLevel parses a share level case-insensitively.
func ParseShareLevel(s string) (ShareLevel, error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "CONNECTION":
		return ShareConnection, nil
	case "USER":
		return ShareUser, nil
	case "GROUP":
		return ShareGroup, nil
	case "SERVER":
		return ShareServer, nil
	}
	return "", fmt.Errorf("unknown share level %q (want CONNECTION, USER, GROUP or SERVER)", s)
}

// subdomainRE restricts subdomains to label-safe names, like Kyuubi's
// [a-zA-Z_0-9-]{1,14} rule but a little more permissive in length.
var subdomainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// ValidSubdomain reports whether s is an acceptable subdomain.
func ValidSubdomain(s string) bool { return subdomainRE.MatchString(s) }

// Key identifies the engine a session must use. Sessions with equal keys share
// an engine; for CONNECTION share level the owner is the session id, so no two
// sessions are equal.
type Key struct {
	ShareLevel ShareLevel
	// Owner is the user (USER), the group (GROUP), "" (SERVER) or the session
	// id (CONNECTION).
	Owner     string
	Subdomain string
	Profile   string
	// PoolIndex selects one of the engines of a pool (pool size > 1).
	PoolIndex int
}

// String renders the key in a stable, human readable form. Components are
// path-escaped, so owners containing '/' or '#' cannot collide.
func (k Key) String() string {
	return fmt.Sprintf("%s/%s/%s/%s#%d", k.ShareLevel, url.PathEscape(k.Owner), url.PathEscape(k.Subdomain),
		url.PathEscape(k.Profile), k.PoolIndex)
}

// Base returns the key without pool index (the identity of the whole pool).
func (k Key) Base() Key {
	k.PoolIndex = 0
	return k
}

// Hash is a label-safe digest of the key.
func (k Key) Hash() string {
	sum := sha256.Sum256([]byte(k.String()))
	return hex.EncodeToString(sum[:])[:32]
}

// UID is a short, stable identifier of an engine, derived from its id. It is
// what gateway connection ids carry (they must stay short, see session).
type UID [8]byte

// UIDOf derives the UID of an engine id.
func UIDOf(engineID string) UID {
	sum := sha256.Sum256([]byte("quackmux/engine-uid\x00" + engineID))
	var u UID
	copy(u[:], sum[:8])
	return u
}

// ParseKey parses the output of Key.String.
func ParseKey(s string) (Key, error) {
	var k Key
	head, idx, ok := strings.Cut(s, "#")
	if !ok {
		return k, fmt.Errorf("invalid engine key %q", s)
	}
	parts := strings.Split(head, "/")
	if len(parts) != 4 {
		return k, fmt.Errorf("invalid engine key %q", s)
	}
	lvl, err := ParseShareLevel(parts[0])
	if err != nil {
		return k, err
	}
	k.ShareLevel = lvl
	for i, dst := range []*string{&k.Owner, &k.Subdomain, &k.Profile} {
		if *dst, err = url.PathUnescape(parts[i+1]); err != nil {
			return k, fmt.Errorf("invalid engine key %q: %w", s, err)
		}
	}
	if _, err := fmt.Sscanf(idx, "%d", &k.PoolIndex); err != nil {
		return k, fmt.Errorf("invalid pool index in engine key %q", s)
	}
	return k, nil
}
