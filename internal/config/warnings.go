package config

import (
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
)

// Warnings returns what is valid but probably not meant, as sentences for
// "quackmux validate" and the start-up log.
func (c *Config) Warnings() []string {
	_, warnings := c.checkDefaults()
	names := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := c.Profiles[name]
		ps := p.Persistence
		if ps == nil {
			continue
		}
		if c.Auth.Mode == "none" {
			warnings = append(warnings, fmt.Sprintf("profiles.%s.persistence with auth.mode none: every client "+
				"chooses its user name, so it can open the data volume of any user and create new volumes "+
				"without bound (one per name); use authentication outside development", name))
		}
		// A file on the volume persists; ":memory:" or a path elsewhere does
		// not, unless the init SQL attaches files on the volume.
		if db := path.Clean(p.Database); !strings.HasPrefix(db, ps.MountPath+"/") && !strings.Contains(p.InitSQL, ps.MountPath) {
			warnings = append(warnings, fmt.Sprintf("profiles.%s.database %q is not on the data volume (%s): its "+
				"tables are lost when the engine ends; without database the engine uses %s",
				name, p.Database, ps.MountPath, path.Join(ps.MountPath, DefaultPersistentDatabase)))
		}
	}
	return warnings
}

// defaultsContext is the routing that a session without options gets: the
// global defaults, or those of an override for the users it matches (each
// field falls back to the global default).
type defaultsContext struct {
	field     string // "defaults" or "overrides[i].defaults"
	level     string
	profile   string
	subdomain string
	override  *Override
}

// defaultContexts lists the defaults that some session can get. Overrides
// after one that matches every user ("*") never apply, and neither do the
// global defaults then.
func (c *Config) defaultContexts() []defaultsContext {
	pick := func(v, def string) string {
		if v != "" {
			return v
		}
		return def
	}
	var out []defaultsContext
	everyone := false
	for i := range c.Overrides {
		o := &c.Overrides[i]
		out = append(out, defaultsContext{
			field:     fmt.Sprintf("overrides[%d].defaults", i),
			level:     strings.ToUpper(pick(o.Defaults.ShareLevel, c.Defaults.ShareLevel)),
			profile:   pick(o.Defaults.Profile, c.Defaults.Profile),
			subdomain: strings.ToLower(pick(o.Defaults.Subdomain, c.Defaults.Subdomain)),
			override:  o,
		})
		for _, u := range o.Users {
			everyone = everyone || u == "*"
		}
		if everyone {
			return out
		}
	}
	return append(out, defaultsContext{
		field: "defaults", level: strings.ToUpper(c.Defaults.ShareLevel), profile: c.Defaults.Profile,
		subdomain: strings.ToLower(c.Defaults.Subdomain),
	})
}

// checkDefaults finds defaults that the rules refuse: every session without
// the option would be denied. For profiles with persistence and for
// allowedSubdomains, which are newer than the other rules, that is an
// error; otherwise a warning, since such configurations were accepted
// before.
func (c *Config) checkDefaults() (errs []error, warnings []string) {
	for _, d := range c.defaultContexts() {
		p, ok := c.Profiles[d.profile]
		if !ok {
			continue // reported by Validate
		}
		if len(p.AllowedShareLevels) > 0 && !containsFold(p.AllowedShareLevels, d.level) {
			msg := fmt.Sprintf("%s: share level %s is not in profiles.%s.allowedShareLevels (%s), so every session "+
				"without the share_level option is refused", d.field, d.level, d.profile, strings.Join(p.AllowedShareLevels, ", "))
			if p.Persistence != nil {
				errs = append(errs, fmt.Errorf("%s", msg))
			} else {
				warnings = append(warnings, msg)
			}
		}
		if d.level != "CONNECTION" && len(p.AllowedSubdomains) > 0 && !slices.Contains(p.AllowedSubdomains, d.subdomain) {
			errs = append(errs, fmt.Errorf("%s: subdomain %q is not in profiles.%s.allowedSubdomains (%s), so every "+
				"session without the subdomain option is refused", d.field, d.subdomain, d.profile,
				strings.Join(p.AllowedSubdomains, ", ")))
		}
		if o := d.override; o != nil {
			if len(o.AllowedShareLevels) > 0 && !containsFold(o.AllowedShareLevels, d.level) {
				warnings = append(warnings, fmt.Sprintf("%s: share level %s is not in the allowedShareLevels of the "+
					"override (%s), so every session of its users without the share_level option is refused",
					d.field, d.level, strings.Join(o.AllowedShareLevels, ", ")))
			}
			if len(o.AllowedProfiles) > 0 && !slices.Contains(o.AllowedProfiles, d.profile) {
				warnings = append(warnings, fmt.Sprintf("%s: profile %q is not in the allowedProfiles of the override "+
					"(%s), so every session of its users without the profile option is refused",
					d.field, d.profile, strings.Join(o.AllowedProfiles, ", ")))
			}
		}
	}
	return errs, warnings
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}
