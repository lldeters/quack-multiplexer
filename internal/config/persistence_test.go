package config

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

const persistentProfile = `
auth: {mode: none}
profiles:
  default: {}
  personal:
    allowedShareLevels: [USER, GROUP]
    allowedSubdomains: [default]
    persistence:
      size: 10Gi
`

func TestPersistenceDefaults(t *testing.T) {
	c, err := Parse([]byte(persistentProfile))
	if err != nil {
		t.Fatal(err)
	}
	p := c.Profiles["personal"]
	ps := p.Persistence
	if ps.MountPath != "/data" || ps.AccessMode != corev1.ReadWriteOncePod || p.Database != "/data/engine.duckdb" ||
		ps.Size.String() != "10Gi" || ps.StorageClassName != "" {
		t.Fatalf("defaults: %+v database %q", ps, p.Database)
	}
	// Other profiles keep :memory:, an explicit database is kept.
	if c.Profiles["default"].Database != ":memory:" {
		t.Error("a profile without persistence lost :memory:")
	}
	c, err = Parse([]byte(strings.Replace(persistentProfile, "persistence:", "database: ':memory:'\n    persistence:\n      mountPath: /var/lib/quack", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if p := c.Profiles["personal"]; p.Database != ":memory:" || p.Persistence.MountPath != "/var/lib/quack" {
		t.Errorf("explicit values replaced: %q %q", p.Database, p.Persistence.MountPath)
	}
}

func TestPersistenceValidation(t *testing.T) {
	for want, mutate := range map[string]func(string) string{
		"needs provider.type kubernetes": func(s string) string { return s + "provider: {type: process}\n" },
		"allowedShareLevels must be set": func(s string) string { return strings.Replace(s, "allowedShareLevels: [USER, GROUP]", "", 1) },
		"CONNECTION cannot be used": func(s string) string {
			return strings.Replace(s, "[USER, GROUP]", "[USER, CONNECTION]", 1)
		},
		"allowedSubdomains must be set": func(s string) string { return strings.Replace(s, "allowedSubdomains: [default]", "", 1) },
		"not a valid subdomain":         func(s string) string { return strings.Replace(s, "[default]", "[Default]", 1) },
		"pool.size must be 1": func(s string) string {
			return strings.Replace(s, "persistence:", "pool: {size: 2}\n    persistence:", 1)
		},
		"warmPool.size must be 0": func(s string) string {
			return strings.Replace(s, "persistence:", "warmPool: {size: 1}\n    persistence:", 1)
		},
		"size is required": func(s string) string { return strings.Replace(s, "size: 10Gi", "storageClassName: fast", 1) },
		"accessMode must be": func(s string) string {
			return strings.Replace(s, "size: 10Gi", "size: 10Gi\n      accessMode: ReadWriteMany", 1)
		},
		"clean absolute path": func(s string) string {
			return strings.Replace(s, "size: 10Gi", "size: 10Gi\n      mountPath: data", 1)
		},
		"other than / and /tmp": func(s string) string {
			return strings.Replace(s, "size: 10Gi", "size: 10Gi\n      mountPath: /tmp", 1)
		},
	} {
		_, err := Parse([]byte(mutate(persistentProfile)))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want error containing %q, got %v", want, err)
		}
	}
	// allowedSubdomains works without persistence, too.
	if _, err := Parse([]byte("auth: {mode: none}\nprofiles:\n  default: {allowedSubdomains: [default, etl]}\n")); err != nil {
		t.Errorf("allowedSubdomains without persistence: %v", err)
	}
}
