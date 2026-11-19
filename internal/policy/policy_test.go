package policy

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"quackmux/internal/auth"
	"quackmux/internal/config"
	"quackmux/internal/engine"
)

func testConfig(t *testing.T, extra string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte("auth: {mode: none}\n" + extra))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var alice = auth.Identity{User: "alice", Groups: []string{"analytics", "ops"}}

func TestOptionsPrecedence(t *testing.T) {
	h := http.Header{}
	h.Set("X-Quackmux-Share-Level", "server")
	h.Set("X-Quackmux-Subdomain", "fromheader")
	opts, err := Options(url.Values{"share_level": {"connection"}}, h)
	if err != nil {
		t.Fatal(err)
	}
	if opts[OptShareLevel] != "connection" || opts[OptSubdomain] != "fromheader" {
		t.Fatalf("precedence wrong: %v", opts)
	}
	if _, err := Options(url.Values{"colour": {"blue"}}, nil); err == nil {
		t.Fatal("unknown option accepted")
	}
}

func TestResolveDefaultsAndLevels(t *testing.T) {
	c := testConfig(t, "")
	d, err := Resolve(c, alice, map[string]string{}, "sess1")
	if err != nil {
		t.Fatal(err)
	}
	if d.Key != (engine.Key{ShareLevel: engine.ShareUser, Owner: "alice", Subdomain: "default", Profile: "default"}) {
		t.Fatalf("default key %+v", d.Key)
	}
	d, _ = Resolve(c, alice, map[string]string{OptShareLevel: "connection", OptSubdomain: "x"}, "sess1")
	if d.Key.Owner != "sess1" || d.Key.Subdomain != "" || d.Key.ShareLevel != engine.ShareConnection {
		t.Fatalf("connection key %+v", d.Key)
	}
	d, _ = Resolve(c, alice, map[string]string{OptShareLevel: "group"}, "s")
	if d.Key.Owner != "analytics" {
		t.Fatalf("primary group not used: %+v", d.Key)
	}
	d, _ = Resolve(c, alice, map[string]string{OptShareLevel: "group", OptGroup: "ops"}, "s")
	if d.Key.Owner != "ops" {
		t.Fatalf("explicit group not used: %+v", d.Key)
	}
	d, _ = Resolve(c, alice, map[string]string{OptShareLevel: "SERVER", OptSubdomain: "BI"}, "s")
	if d.Key.Owner != "" || d.Key.Subdomain != "bi" {
		t.Fatalf("server key %+v", d.Key)
	}
	for _, opts := range []map[string]string{
		{OptShareLevel: "group", OptGroup: "admins"},
		{OptShareLevel: "cluster"},
		{OptProfile: "missing"},
		{OptSubdomain: "-bad-"},
		{OptSubdomain: strings.Repeat("a", 40)},
	} {
		if _, err := Resolve(c, alice, opts, "s"); !errors.Is(err, ErrDenied) {
			t.Errorf("%v: want ErrDenied, got %v", opts, err)
		}
	}
	if _, err := Resolve(c, auth.Identity{User: "bob"}, map[string]string{OptShareLevel: "group"}, "s"); !errors.Is(err, ErrDenied) {
		t.Error("GROUP without groups must be denied")
	}
}

func TestProfileAccessRules(t *testing.T) {
	c := testConfig(t, `
profiles:
  default: {}
  gpu:
    allowedShareLevels: [USER, CONNECTION]
    allowedGroups: [ml]
`)
	ml := auth.Identity{User: "carol", Groups: []string{"ml"}}
	if _, err := Resolve(c, ml, map[string]string{OptProfile: "gpu"}, "s"); err != nil {
		t.Fatalf("ml member denied: %v", err)
	}
	if _, err := Resolve(c, ml, map[string]string{OptProfile: "gpu", OptShareLevel: "server"}, "s"); !errors.Is(err, ErrDenied) {
		t.Fatal("share level restriction not enforced")
	}
	if _, err := Resolve(c, alice, map[string]string{OptProfile: "gpu"}, "s"); !errors.Is(err, ErrDenied) {
		t.Fatal("group restriction not enforced")
	}
}

func TestOverrides(t *testing.T) {
	c := testConfig(t, `
limits: {maxSessionsPerUser: 5}
profiles:
  default: {}
  etl: {}
overrides:
  - groups: [ops]
    defaults: {shareLevel: SERVER, profile: etl, subdomain: ops}
    allowedShareLevels: [SERVER, USER]
    maxSessions: 50
  - users: ["*"]
    allowedProfiles: [default]
`)
	d, err := Resolve(c, alice, map[string]string{}, "s")
	if err != nil {
		t.Fatal(err)
	}
	if d.Key.ShareLevel != engine.ShareServer || d.Key.Profile != "etl" || d.Key.Subdomain != "ops" || d.MaxSessions != 50 {
		t.Fatalf("override defaults not applied: %+v", d)
	}
	if _, err := Resolve(c, alice, map[string]string{OptShareLevel: "connection"}, "s"); !errors.Is(err, ErrDenied) {
		t.Fatal("override share level restriction not enforced")
	}
	bob := auth.Identity{User: "bob"}
	d, err = Resolve(c, bob, map[string]string{}, "s")
	if err != nil || d.MaxSessions != 5 || d.Key.Profile != "default" {
		t.Fatalf("second override: %+v %v", d, err)
	}
	if _, err := Resolve(c, bob, map[string]string{OptProfile: "etl"}, "s"); !errors.Is(err, ErrDenied) {
		t.Fatal("allowedProfiles not enforced")
	}
}
