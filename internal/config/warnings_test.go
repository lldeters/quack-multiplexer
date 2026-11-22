package config

import (
	"strings"
	"testing"
)

// A default profile whose rules refuse the default share level or subdomain
// refuses every session that does not name them. With persistence or
// allowedSubdomains (both new) that is an error; with the older rules alone
// a warning, since such configurations were accepted before.
func TestDefaultsAgainstTheirProfile(t *testing.T) {
	const probe = `
auth: {mode: static, static: {file: users.yaml}}
defaults: {shareLevel: CONNECTION, profile: p}
profiles:
  p:
    allowedShareLevels: [USER]
    allowedSubdomains: [main]
    persistence: {size: 1Gi}
`
	_, err := Parse([]byte(probe))
	if err == nil || !strings.Contains(err.Error(), "defaults: share level CONNECTION is not in profiles.p.allowedShareLevels") {
		t.Fatalf("share level: %v", err)
	}
	_, err = Parse([]byte(strings.Replace(probe, "shareLevel: CONNECTION", "shareLevel: USER", 1)))
	if err == nil || !strings.Contains(err.Error(), `defaults: subdomain "default" is not in profiles.p.allowedSubdomains`) {
		t.Fatalf("subdomain: %v", err)
	}
	c, err := Parse([]byte(strings.Replace(probe, "shareLevel: CONNECTION, profile: p", "shareLevel: user, subdomain: Main, profile: p", 1)))
	if err != nil {
		t.Fatalf("defaults that the profile allows: %v", err)
	}
	if w := c.Warnings(); len(w) != 0 {
		t.Errorf("warnings: %v", w)
	}
	// An override's defaults fall back to the global ones field by field.
	override := strings.Replace(probe, "shareLevel: CONNECTION, profile: p", "shareLevel: USER, subdomain: main, profile: p", 1) +
		"overrides:\n  - users: [bob]\n    defaults: {subdomain: other}\n"
	if _, err := Parse([]byte(override)); err == nil || !strings.Contains(err.Error(), `overrides[0].defaults: subdomain "other"`) {
		t.Fatalf("override: %v", err)
	}
	// After an override for every user ("*"), the global defaults never apply.
	everyone := strings.Replace(probe, "shareLevel: CONNECTION, profile: p", "shareLevel: CONNECTION", 1) +
		"  default: {}\noverrides:\n  - users: [\"*\"]\n    defaults: {shareLevel: USER, subdomain: main, profile: p}\n"
	if _, err := Parse([]byte(everyone)); err != nil {
		t.Fatalf("defaults that no session gets: %v", err)
	}

	// Without the newer rules: valid as before, with a warning.
	older := `
auth: {mode: static, static: {file: users.yaml}}
defaults: {shareLevel: CONNECTION}
profiles:
  default: {allowedShareLevels: [USER]}
overrides:
  - groups: [etl]
    defaults: {profile: default}
    allowedShareLevels: [SERVER]
    allowedProfiles: [other]
  - groups: [x]
`
	c, err = Parse([]byte(strings.Replace(older, "allowedProfiles: [other]", "allowedProfiles: [default]", 1)))
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(c.Warnings(), "\n")
	for _, want := range []string{
		"defaults: share level CONNECTION is not in profiles.default.allowedShareLevels",
		"overrides[0].defaults: share level CONNECTION is not in profiles.default.allowedShareLevels",
		"overrides[0].defaults: share level CONNECTION is not in the allowedShareLevels of the override",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("no warning %q in:\n%s", want, w)
		}
	}
}

// A persistent profile whose database is not a file on its volume keeps its
// tables in memory, and per-user volumes without authentication protect
// nothing: both are valid, but warned about.
func TestPersistenceWarnings(t *testing.T) {
	const base = `
auth: {mode: static, static: {file: users.yaml}}
defaults: {shareLevel: USER, profile: personal}
profiles:
  personal:
    allowedShareLevels: [USER]
    allowedSubdomains: [default]
    persistence: {size: 10Gi}
`
	for what, c := range map[string]struct {
		yaml string
		want string // "" for no warning
	}{
		"default database":            {base, ""},
		"explicit file on the volume": {strings.Replace(base, "persistence:", "database: /data/sub/x.duckdb\n    persistence:", 1), ""},
		"memory": {strings.Replace(base, "persistence:", "database: \":memory:\"\n    persistence:", 1),
			`profiles.personal.database ":memory:" is not on the data volume (/data)`},
		"file elsewhere": {strings.Replace(base, "persistence:", "database: /tmp/x.duckdb\n    persistence:", 1),
			`profiles.personal.database "/tmp/x.duckdb" is not on the data volume`},
		"next to the mount path": {strings.Replace(base, "persistence:", "database: /data2/x.duckdb\n    persistence:", 1),
			"is not on the data volume"},
		"memory with files attached in initSQL": {strings.Replace(base, "persistence:",
			"database: \":memory:\"\n    initSQL: \"ATTACH '/data/lake.duckdb' AS lake;\"\n    persistence:", 1), ""},
		"auth none": {strings.Replace(base, "mode: static, static: {file: users.yaml}", "mode: none", 1),
			"profiles.personal.persistence with auth.mode none"},
	} {
		cfg, err := Parse([]byte(c.yaml))
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		w := strings.Join(cfg.Warnings(), "\n")
		switch {
		case c.want == "" && w != "":
			t.Errorf("%s: unexpected warnings: %s", what, w)
		case c.want != "" && !strings.Contains(w, c.want):
			t.Errorf("%s: want a warning with %q, got %q", what, c.want, w)
		}
	}
}
