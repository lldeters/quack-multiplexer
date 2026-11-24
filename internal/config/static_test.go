package config

import (
	"strings"
	"testing"
)

// The static provider does not start engines, so the settings, init SQL and
// post-start SQL of its profiles do nothing. A hardening that the operator
// expects from postStartSQL must not be missing silently.
func TestStaticProfilesWarnAboutLaunchFields(t *testing.T) {
	const raw = `
auth: {mode: none}
defaults: {shareLevel: SERVER}
provider:
  type: static
  static:
    engines: [{name: shared, url: 'http://engine:9494', token: t, shareLevel: SERVER}]
profiles:
  default:
    settings: {threads: "4"}
    initSQL: ".read /usr/local/share/quackmux/authorize.sql"
    postStartSQL: "SET enable_external_access = false;"
  other: {description: "nothing to start"}
`
	c, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(c.Warnings(), "\n")
	for _, want := range []string{
		"profiles.default.settings has no effect with provider.type static",
		"profiles.default.initSQL has no effect with provider.type static: the gateway does not start these engines; set QUACKMUX_INIT_SQL on the engines instead",
		"profiles.default.postStartSQL has no effect with provider.type static: the gateway does not start these engines; set QUACKMUX_POST_START_SQL on the engines instead",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("no warning %q in:\n%s", want, w)
		}
	}
	if strings.Contains(w, "profiles.other") {
		t.Errorf("warning for a profile without launch fields:\n%s", w)
	}

	// Providers that start engines run all three.
	c, err = Parse([]byte(strings.Replace(raw, "type: static", "type: process", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if w := c.Warnings(); len(w) != 0 {
		t.Errorf("warnings with provider.type process: %v", w)
	}
}
