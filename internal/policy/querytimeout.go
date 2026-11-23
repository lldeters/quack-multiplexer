package policy

import (
	"fmt"
	"time"

	"quackmux/internal/config"
)

// QueryTimeout resolves the time limit for the queries of a session of user
// (with groups) on profile: the queryTimeout of the first override that
// matches the user, if it sets one, else the profile's, else
// limits.queryTimeout. 0 means no limit. It also returns the setting the
// limit comes from (such as "profiles.adhoc.queryTimeout"), for messages.
//
// Like Resolve, only the first matching override counts. For a session
// adopted after a gateway restart the groups are unknown (connection ids do
// not carry them), so only overrides that match by user name apply there.
func QueryTimeout(cfg *config.Config, user string, groups []string, profile string) (time.Duration, string) {
	for i := range cfg.Overrides {
		if cfg.Overrides[i].Matches(user, groups) {
			if d := cfg.Overrides[i].QueryTimeout; d != nil {
				return d.Duration, fmt.Sprintf("overrides[%d].queryTimeout", i)
			}
			break
		}
	}
	if p, ok := cfg.Profiles[profile]; ok && p.QueryTimeout != nil {
		return p.QueryTimeout.Duration, "profiles." + profile + ".queryTimeout"
	}
	return cfg.Limits.QueryTimeout.Duration, "limits.queryTimeout"
}

// QueryTimeoutConfigured reports whether any query time limit is configured
// at all: without one, the gateway does not track queries.
func QueryTimeoutConfigured(cfg *config.Config) bool {
	if cfg.Limits.QueryTimeout.Duration > 0 {
		return true
	}
	for _, o := range cfg.Overrides {
		if o.QueryTimeout != nil && o.QueryTimeout.Duration > 0 {
			return true
		}
	}
	for _, p := range cfg.Profiles {
		if p.QueryTimeout != nil && p.QueryTimeout.Duration > 0 {
			return true
		}
	}
	return false
}
