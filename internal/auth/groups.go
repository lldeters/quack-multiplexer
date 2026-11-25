package auth

import "slices"

// GroupLookup is implemented by authenticators that know the groups of a
// user without a credential: static (the users file as it is now) and none
// (the default groups). The gateway asks it for the sessions it adopts after
// a restart or a leader fail-over, whose connection ids carry the user but
// not the groups. A JWT carries the groups itself, so the jwt authenticator
// cannot know them later.
type GroupLookup interface {
	// GroupsOf returns the groups of user, or nil if the user has none or
	// is unknown.
	GroupsOf(user string) []string
}

// GroupsOf implements GroupLookup.
func (a *staticAuthenticator) GroupsOf(user string) []string {
	if u := a.state.Load().byName[user]; u != nil {
		return slices.Clone(u.groups)
	}
	return nil
}

// GroupsOf implements GroupLookup: every user has the default groups.
func (a *noneAuthenticator) GroupsOf(string) []string { return slices.Clone(a.groups) }
