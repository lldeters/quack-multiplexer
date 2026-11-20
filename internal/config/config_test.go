package config

import (
	"strings"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte("auth: {mode: none}\n"))
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"session.idleTimeout", c.Session.IdleTimeout.Duration, 6 * time.Hour},
		{"session.maxLifetime", c.Session.MaxLifetime.Duration, 0},
		{"engine.startupTimeout", c.Engine.StartupTimeout.Duration, 180 * time.Second},
		{"engine.idle USER", c.Engine.IdleTimeout["USER"].Duration, 30 * time.Minute},
		{"engine.idle CONNECTION", c.Engine.IdleTimeout["CONNECTION"].Duration, 0},
		{"engine.recoveryGrace", c.Engine.RecoveryGrace.Duration, 180 * time.Second},
		{"server.idleTimeout", c.Server.IdleTimeout.Duration, 300 * time.Second},
	}
	for _, ch := range checks {
		if ch.got != ch.want {
			t.Errorf("%s = %s, want %s", ch.name, ch.got, ch.want)
		}
	}
	// Without an admin token, the admin API listens on the loopback interface.
	if c.Server.Listen != ":9494" || c.Admin.Listen != "127.0.0.1:9495" || c.Defaults.ShareLevel != "USER" ||
		c.Defaults.Profile != "default" || c.Provider.Type != "kubernetes" {
		t.Errorf("unexpected defaults: %+v %+v %+v %+v", c.Server.Listen, c.Admin.Listen, c.Defaults, c.Provider.Type)
	}
	p := c.Profiles["default"]
	if p.Image != DefaultEngineImage || p.Database != ":memory:" || p.Pool.Size != 1 || p.Pool.SelectPolicy != "RANDOM" {
		t.Errorf("unexpected profile defaults: %+v", p)
	}
	if !*c.Engine.ReplaceOnSpecChange {
		t.Error("replaceOnSpecChange should default to true")
	}
}

func TestExplicitZeroDurationsAreKept(t *testing.T) {
	c, err := Parse([]byte(`
auth: {mode: none}
session:
  idleTimeout: 0
  maxLifetime: "0"
engine:
  idleTimeout: {user: 0s, server: 2h}
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Session.IdleTimeout.Duration != 0 {
		t.Errorf("explicit idleTimeout 0 was replaced by %s", c.Session.IdleTimeout.Duration)
	}
	if c.Engine.IdleTimeout["USER"].Duration != 0 || c.Engine.IdleTimeout["SERVER"].Duration != 2*time.Hour ||
		c.Engine.IdleTimeout["GROUP"].Duration != 30*time.Minute {
		t.Errorf("engine idle timeouts wrong: %+v", c.Engine.IdleTimeout)
	}
	if got := c.EngineIdleTimeout("user", "default"); got != 0 {
		t.Errorf("EngineIdleTimeout(user) = %s", got)
	}
}

func TestDurationForms(t *testing.T) {
	for in, want := range map[string]time.Duration{`"90s"`: 90 * time.Second, `120`: 120 * time.Second, `"1.5"`: 1500 * time.Millisecond, `"2h30m"`: 150 * time.Minute} {
		var d Duration
		if err := d.UnmarshalJSON([]byte(in)); err != nil || d.Duration != want || !d.IsSet() {
			t.Errorf("%s -> %s %v", in, d.Duration, err)
		}
	}
	var d Duration
	if err := d.UnmarshalJSON([]byte(`"soon"`)); err == nil {
		t.Error("invalid duration accepted")
	}
}

func TestProfileOverrides(t *testing.T) {
	c, err := Parse([]byte(`
auth: {mode: none}
profiles:
  default: {}
  big:
    idleTimeout: 5m
    startupTimeout: 10m
    maxLifetime: 1h
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.EngineIdleTimeout("USER", "big") != 5*time.Minute || c.EngineStartupTimeout("big") != 10*time.Minute ||
		c.EngineMaxLifetime("big") != time.Hour || c.EngineMaxLifetime("default") != 0 {
		t.Error("profile overrides not applied")
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"unknown field":         "auth: {mode: none}\nbogus: 1\n",
		"auth mode":             "auth: {mode: ldap}\n",
		"static without file":   "auth: {mode: static}\n",
		"jwt without keys":      "auth: {mode: jwt}\n",
		"bad share level":       "auth: {mode: none}\ndefaults: {shareLevel: CLUSTER}\n",
		"missing profile":       "auth: {mode: none}\ndefaults: {profile: nope}\n",
		"override matches none": "auth: {mode: none}\noverrides: [{defaults: {shareLevel: USER}}]\n",
		"bad pool policy":       "auth: {mode: none}\nprofiles: {default: {pool: {selectPolicy: FASTEST}}}\n",
		"static no engines":     "auth: {mode: none}\nprovider: {type: static}\n",
		"static connection":     "auth: {mode: none}\nprovider: {type: static, static: {engines: [{name: a, url: 'http://x', token: t, shareLevel: CONNECTION}]}}\n",
		"recovery too short":    "auth: {mode: none}\nengine: {recoveryGrace: 10s}\n",
		"bad setting name":      "auth: {mode: none}\nprofiles: {default: {settings: {'threads; DROP': '1'}}}\n",
		"ha needs key":          "auth: {mode: none}\nha: {enabled: true}\n",
	}
	for name, raw := range cases {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func TestOverrideMatches(t *testing.T) {
	o := Override{Users: []string{"alice"}, Groups: []string{"ops"}}
	if !o.Matches("alice", nil) || !o.Matches("bob", []string{"dev", "ops"}) || o.Matches("bob", []string{"dev"}) {
		t.Error("override matching wrong")
	}
	if !(Override{Users: []string{"*"}}).Matches("anyone", nil) {
		t.Error("wildcard user must match")
	}
}

func TestValidationMessageListsAllProblems(t *testing.T) {
	_, err := Parse([]byte("auth: {mode: ldap}\ndefaults: {shareLevel: CLUSTER}\n"))
	if err == nil || !strings.Contains(err.Error(), "auth.mode") || !strings.Contains(err.Error(), "CLUSTER") {
		t.Fatalf("want both problems reported, got %v", err)
	}
}

func TestAdminListenDefaultFollowsToken(t *testing.T) {
	c, err := Parse([]byte("auth: {mode: none}\nadmin: {tokenFile: /etc/quackmux/admin/token}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Admin.Listen != ":9495" {
		t.Fatalf("admin.listen %q with a token, want all interfaces", c.Admin.Listen)
	}
}

func TestAllowedOriginsDefaultFollowsAuthMode(t *testing.T) {
	for mode, want := range map[string]int{"none": 0, "static": 1} {
		yaml := "auth: {mode: " + mode + "}\n"
		if mode == "static" {
			yaml = "auth: {mode: static, static: {file: users.yaml}}\n"
		}
		c, err := Parse([]byte(yaml))
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Server.AllowedOrigins) != want {
			t.Fatalf("auth.mode %s: allowedOrigins %v", mode, c.Server.AllowedOrigins)
		}
	}
}
