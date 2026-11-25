package policy

import (
	"testing"
	"time"
)

func TestQueryTimeoutPrecedence(t *testing.T) {
	c := testConfig(t, `
limits: {queryTimeout: 30m}
profiles:
  default: {}
  batch: {queryTimeout: 0s}
  adhoc: {queryTimeout: 5m}
overrides:
  - users: [etl-bot]
    queryTimeout: 6h
  - groups: [ops]
    allowedProfiles: [default, batch, adhoc]
  - groups: [analytics]
    queryTimeout: 10m
`)
	for _, tc := range []struct {
		name    string
		user    string
		groups  []string
		profile string
		want    time.Duration
		source  string
	}{
		{"global limit", "carol", nil, "default", 30 * time.Minute, "limits.queryTimeout"},
		{"profile replaces the global limit", "carol", nil, "adhoc", 5 * time.Minute, "profiles.adhoc.queryTimeout"},
		{"explicit 0 on the profile disables it", "carol", nil, "batch", 0, "profiles.batch.queryTimeout"},
		{"user override wins over the profile", "etl-bot", nil, "batch", 6 * time.Hour, "overrides[0].queryTimeout"},
		{"group override wins over the profile", "dave", []string{"analytics"}, "adhoc", 10 * time.Minute, "overrides[2].queryTimeout"},
		// The first matching override counts, even if it sets no limit:
		// then the profile's applies, not that of a later override.
		{"first match without a limit falls back", "erin", []string{"ops", "analytics"}, "adhoc", 5 * time.Minute, "profiles.adhoc.queryTimeout"},
		{"first match without a limit, global", "erin", []string{"ops", "analytics"}, "default", 30 * time.Minute, "limits.queryTimeout"},
		{"unknown profile uses the global limit", "carol", nil, "gone", 30 * time.Minute, "limits.queryTimeout"},
		// After a gateway restart the groups of a session are unknown.
		{"adopted session: no groups", "dave", nil, "adhoc", 5 * time.Minute, "profiles.adhoc.queryTimeout"},
	} {
		if got, source := QueryTimeout(c, tc.user, tc.groups, tc.profile); got != tc.want || source != tc.source {
			t.Errorf("%s: QueryTimeout(%s, %v, %s) = %s, %s, want %s, %s", tc.name, tc.user, tc.groups, tc.profile,
				got, source, tc.want, tc.source)
		}
		if got := QueryTimeoutLimit(c, tc.user, tc.groups, tc.profile); got != tc.want {
			t.Errorf("%s: QueryTimeoutLimit(%s, %v, %s) = %s, want %s", tc.name, tc.user, tc.groups, tc.profile, got, tc.want)
		}
	}
}

func TestQueryTimeoutConfigured(t *testing.T) {
	for raw, want := range map[string]bool{
		"":                             false,
		"limits: {queryTimeout: 0s}\n": false,
		"limits: {queryTimeout: 1m}\n": true,
		"profiles: {default: {queryTimeout: 0s}}\n":                             false,
		"profiles: {default: {}, big: {queryTimeout: 1h}}\n":                    true,
		"overrides: [{users: [alice], queryTimeout: 0s}]\n":                     false,
		"overrides: [{groups: [analytics], queryTimeout: 15m}]\n":               true,
		"limits: {queryTimeout: 1m}\nprofiles: {default: {queryTimeout: 0s}}\n": true,
	} {
		if got := QueryTimeoutConfigured(testConfig(t, raw)); got != want {
			t.Errorf("%q: QueryTimeoutConfigured = %v, want %v", raw, got, want)
		}
	}
}
