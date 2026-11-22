package policy

import (
	"errors"
	"strings"
	"testing"

	"quackmux/internal/auth"
	"quackmux/internal/config"
	"quackmux/internal/engine"
)

func TestAllowedSubdomains(t *testing.T) {
	cfg, err := config.Parse([]byte(`
auth: {mode: none}
profiles:
  default:
    allowedSubdomains: [default, etl]
`))
	if err != nil {
		t.Fatal(err)
	}
	alice := auth.Identity{User: "alice", Groups: []string{"g"}}
	for _, c := range []struct {
		opts map[string]string
		ok   bool
	}{
		{map[string]string{}, true}, // defaults.subdomain "default"
		{map[string]string{OptSubdomain: "etl"}, true},
		{map[string]string{OptSubdomain: "ETL"}, true}, // lower-cased first
		{map[string]string{OptSubdomain: "adhoc"}, false},
		{map[string]string{OptSubdomain: "adhoc", OptShareLevel: "group"}, false},
		// CONNECTION engines ignore the subdomain.
		{map[string]string{OptSubdomain: "adhoc", OptShareLevel: "connection"}, true},
	} {
		d, err := Resolve(cfg, alice, c.opts, "sid")
		if c.ok != (err == nil) {
			t.Errorf("%v: decision %+v, error %v", c.opts, d, err)
			continue
		}
		if err != nil && (!errors.Is(err, ErrDenied) || !strings.Contains(err.Error(), `does not allow subdomain "adhoc"`)) {
			t.Errorf("%v: error %v", c.opts, err)
		}
		if err == nil && d.Key.ShareLevel == engine.ShareConnection && d.Key.Subdomain != "" {
			t.Errorf("CONNECTION key with subdomain %q", d.Key.Subdomain)
		}
	}
}
