package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestQueryTimeoutDefaultsToOff(t *testing.T) {
	c, err := Parse([]byte("auth: {mode: none}\nprofiles: {default: {}}\noverrides: [{users: [alice]}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Limits.QueryTimeout.Duration != 0 {
		t.Errorf("limits.queryTimeout defaults to %s, want 0 (off)", c.Limits.QueryTimeout.Duration)
	}
	if c.Profiles["default"].QueryTimeout != nil || c.Overrides[0].QueryTimeout != nil {
		t.Error("an unset profile or override queryTimeout must stay nil (inherit)")
	}
}

func TestQueryTimeoutValues(t *testing.T) {
	c, err := Parse([]byte(`
auth: {mode: none}
limits: {queryTimeout: 30m}
profiles:
  default: {}
  batch: {queryTimeout: 0s}
  adhoc: {queryTimeout: 90}
overrides:
  - groups: [etl]
    queryTimeout: 4h
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Limits.QueryTimeout.Duration != 30*time.Minute {
		t.Errorf("limits.queryTimeout = %s", c.Limits.QueryTimeout.Duration)
	}
	// An explicit 0 disables the limit for the profile; it is kept, not
	// replaced by the global value.
	if d := c.Profiles["batch"].QueryTimeout; d == nil || d.Duration != 0 {
		t.Errorf("profiles.batch.queryTimeout = %v, want an explicit 0", d)
	}
	if d := c.Profiles["adhoc"].QueryTimeout; d == nil || d.Duration != 90*time.Second {
		t.Errorf("profiles.adhoc.queryTimeout = %v, want 90s", d)
	}
	if d := c.Overrides[0].QueryTimeout; d == nil || d.Duration != 4*time.Hour {
		t.Errorf("overrides[0].queryTimeout = %v, want 4h", d)
	}
}

func TestQueryTimeoutMustNotBeNegative(t *testing.T) {
	for field, raw := range map[string]string{
		"limits.queryTimeout":           "limits: {queryTimeout: -1s}\n",
		"profiles.big.queryTimeout":     "profiles: {default: {}, big: {queryTimeout: -5m}}\n",
		"overrides[0].queryTimeout":     "overrides: [{users: [bob], queryTimeout: -1}]\n",
		"overrides[1].queryTimeout":     "overrides: [{users: [bob]}, {groups: [ops], queryTimeout: '-2h'}]\n",
		"profiles.default.queryTimeout": "profiles: {default: {queryTimeout: -1ms}}\n",
	} {
		_, err := Parse([]byte("auth: {mode: none}\n" + raw))
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: got %v, want an error naming the field", field, err)
		}
	}
}

// With auth.mode jwt the gateway does not know the groups of adopted
// sessions: an override by group that decides the query time limit gets a
// warning. With static and none it looks them up, and overrides by user
// name always match.
func TestQueryTimeoutWarnsAboutGroupsWithJWT(t *testing.T) {
	const jwt = "auth: {mode: jwt, jwt: {jwksURL: \"https://idp.example/jwks\"}}\nlimits: {queryTimeout: 30m}\n"
	for what, c := range map[string]struct {
		yaml string
		want []int // overrides with a warning
	}{
		"sets it":            {jwt + "overrides: [{groups: [etl], queryTimeout: 4h}]\n", []int{0}},
		"before a user rule": {jwt + "overrides: [{groups: [etl]}, {users: ['*'], queryTimeout: 1h}]\n", []int{0}},
		"only users":         {jwt + "overrides: [{users: [bob], queryTimeout: 4h}]\n", nil},
		"no limit decided":   {jwt + "overrides: [{groups: [etl], maxSessions: 3}]\n", nil},
		"static auth": {strings.Replace(jwt, "mode: jwt, jwt: {jwksURL: \"https://idp.example/jwks\"}",
			"mode: static, static: {file: users.yaml}", 1) + "overrides: [{groups: [etl], queryTimeout: 4h}]\n", nil},
	} {
		cfg, err := Parse([]byte(c.yaml))
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		var got []int
		for _, w := range cfg.Warnings() {
			for i := range cfg.Overrides {
				if strings.HasPrefix(w, fmt.Sprintf("overrides[%d] matches by groups and decides the query time limit", i)) {
					got = append(got, i)
				}
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: warnings for overrides %v, want %v", what, got, c.want)
		}
	}
}
