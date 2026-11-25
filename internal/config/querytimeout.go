package config

import (
	"fmt"
	"sort"
)

// validateQueryTimeouts checks limits.queryTimeout and the profile and
// override values that replace it. 0 disables the limit, so only negative
// durations are wrong.
func (c *Config) validateQueryTimeouts(add func(format string, args ...any)) {
	check := func(field string, d *Duration) {
		if d != nil && d.Duration < 0 {
			add("%s must not be negative, got %s (0s disables the limit)", field, d.Duration)
		}
	}
	check("limits.queryTimeout", &c.Limits.QueryTimeout)
	for i := range c.Overrides {
		check(fmt.Sprintf("overrides[%d].queryTimeout", i), c.Overrides[i].QueryTimeout)
	}
	names := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		check("profiles."+name+".queryTimeout", c.Profiles[name].QueryTimeout)
	}
}

// queryTimeoutWarnings warns about overrides by group that decide the query
// time limit when the gateway cannot know the groups of every session: a
// connection id carries the user, not the groups, so after a gateway
// restart or a leader fail-over the gateway asks the authenticator for them,
// and the jwt authenticator cannot tell (the groups came with the token).
// Such an override then does not match, and the limit of the next one that
// matches applies (or the profile's, or limits.queryTimeout).
func (c *Config) queryTimeoutWarnings() []string {
	if c.Auth.Mode != "jwt" {
		return nil
	}
	var warnings []string
	for i, o := range c.Overrides {
		if len(o.Groups) == 0 {
			continue
		}
		decides := o.QueryTimeout != nil
		for _, later := range c.Overrides[i+1:] {
			decides = decides || (len(later.Users) > 0 && later.QueryTimeout != nil)
		}
		if decides {
			warnings = append(warnings, fmt.Sprintf("overrides[%d] matches by groups and decides the query time limit: "+
				"with auth.mode jwt the gateway does not know the groups of the sessions it adopts after a restart "+
				"or a leader fail-over, so their queries get the limit as if this override did not match", i))
		}
	}
	return warnings
}
