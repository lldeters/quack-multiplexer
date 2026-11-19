package kubernetes

import (
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/validate/content"
	"k8s.io/apimachinery/pkg/util/validation"

	"quackmux/internal/engine"
)

var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func TestNewIDIsValidPodName(t *testing.T) {
	e := newTestEnv(t, testConfig(t, ""))
	long := strings.Repeat("Very-Long_Owner.Name ", 10)
	cases := []struct {
		key    engine.Key
		warm   bool
		prefix string // expected name without the random suffix
	}{
		{userKey, false, "quack-user-alice-"},
		{engine.Key{ShareLevel: engine.ShareGroup, Owner: "Team A/B", Subdomain: "etl", Profile: "default"}, false, "quack-group-team-a-b-"},
		{engine.Key{ShareLevel: engine.ShareServer, Subdomain: "default", Profile: "default"}, false, "quack-server-"},
		{engine.Key{ShareLevel: engine.ShareConnection, Owner: "3f1c2a9e-session-id", Profile: "default"}, false, "quack-connection-conn-"},
		{engine.Key{ShareLevel: engine.ShareConnection, Profile: "Large_Profile"}, true, "quack-warm-large-profile-"},
		{engine.Key{ShareLevel: engine.ShareUser, Owner: "jürgen@example.com", Profile: "default"}, false, "quack-user-j-rgen-example-com-"},
		{engine.Key{ShareLevel: engine.ShareUser, Owner: "--__--", Profile: "default"}, false, "quack-user-"},
		{engine.Key{ShareLevel: engine.ShareUser, Owner: long, Profile: "default"}, false, "quack-user-very-long-owner-name-very-long-owner-name-very-"},
	}
	for _, c := range cases {
		id := e.p.NewID(c.key, c.warm)
		if len(id) > 63 || !dns1123Label.MatchString(id) || strings.Contains(id, "--") {
			t.Errorf("NewID(%v, %v) = %q is not a valid DNS-1123 label", c.key, c.warm, id)
		}
		if errs := validation.IsDNS1123Label(id); len(errs) > 0 {
			t.Errorf("NewID(%v) = %q: %v", c.key, id, errs)
		}
		if !strings.HasPrefix(id, c.prefix) || len(id) != len(c.prefix)+suffixLength {
			t.Errorf("NewID(%v, %v) = %q, want %q + %d random characters", c.key, c.warm, id, c.prefix, suffixLength)
		}
		suffix := id[len(id)-suffixLength:]
		if strings.Trim(suffix, suffixAlphabet) != "" {
			t.Errorf("suffix %q of %q is not [a-z0-9]", suffix, id)
		}
	}
}

func TestNewIDPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"":                             "quack",
		"Duck_DB":                      "duck-db",
		"---":                          "quack",
		"Team.Engines":                 "team-engines",
		strings.Repeat("abcdef", 10):   "abcdefabcdefabcdefabcdefabcdef",
		strings.Repeat("a", 29) + "-b": strings.Repeat("a", 29),
	} {
		if got := namePrefix(in); got != want {
			t.Errorf("namePrefix(%q) = %q, want %q", in, got, want)
		}
	}
	cfg := testConfig(t, "provider: {kubernetes: {namePrefix: My_Engines}}\n")
	e := newTestEnv(t, cfg)
	if id := e.p.NewID(userKey, false); !strings.HasPrefix(id, "my-engines-user-alice-") {
		t.Errorf("NewID with prefix: %q", id)
	}
}

func TestNewIDSkipsExistingNames(t *testing.T) {
	e := newTestEnv(t, testConfig(t, ""))
	suffixes := []string{"aaaaa", "aaaaa", "bbbbb", "ccccc"}
	e.p.newSuffix = func() string {
		s := suffixes[0]
		suffixes = suffixes[1:]
		return s
	}
	// A launched engine that the informer has not reported yet.
	e.p.storeToken("quack-user-alice-aaaaa", "t")
	if id := e.p.NewID(userKey, false); id != "quack-user-alice-bbbbb" {
		t.Fatalf("NewID = %q, want the first unused name", id)
	}
}

func TestDNSPartAndLabelValue(t *testing.T) {
	for in, want := range map[string]string{
		"Alice":         "alice",
		"a__b..c":       "a-b-c",
		"-x-":           "x",
		"日本":            "",
		"user@corp.com": "user-corp-com",
	} {
		if got := dnsPart(in); got != want {
			t.Errorf("dnsPart(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"default":                       "default",
		"Large_Profile.v2":              "Large_Profile.v2",
		"with space":                    "with-space",
		"_x_":                           "x",
		"":                              "",
		strings.Repeat("a", 70):         strings.Repeat("a", 63),
		strings.Repeat("a", 62) + "._b": strings.Repeat("a", 62),
	} {
		got := labelValue(in)
		if got != want {
			t.Errorf("labelValue(%q) = %q, want %q", in, got, want)
		}
		if errs := content.IsLabelValue(got); len(errs) > 0 {
			t.Errorf("labelValue(%q) = %q is invalid: %v", in, got, errs)
		}
	}
}
