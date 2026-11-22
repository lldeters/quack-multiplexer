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
