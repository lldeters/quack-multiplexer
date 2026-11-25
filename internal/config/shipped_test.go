package config_test

import (
	"bufio"
	"bytes"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"quackmux/internal/config"
)

// The shipped Kubernetes configurations, relative to this package.
var (
	repoRoot       = filepath.Join("..", "..")
	baseConfig     = filepath.Join(repoRoot, "deploy", "kubernetes", "base", "config.yaml")
	shippedConfigs = []string{
		baseConfig,
		filepath.Join(repoRoot, "deploy", "kubernetes", "overlays", "ha", "config.yaml"),
		filepath.Join(repoRoot, "deploy", "kubernetes", "overlays", "dev", "config.yaml"),
	}
	exampleConfig = filepath.Join(repoRoot, "examples", "config.kubernetes.yaml")
)

const gib = int64(1) << 30

var duckdbSize = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([KMGT]i?B|B)$`)

// duckdbBytes parses a DuckDB memory size such as 3GB (10^9) or 8GiB (2^30).
func duckdbBytes(t *testing.T, s string) int64 {
	t.Helper()
	m := duckdbSize.FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("%q is no DuckDB size", s)
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatal(err)
	}
	unit := map[string]float64{
		"B": 1, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
		"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40,
	}[m[2]]
	return int64(n * unit)
}

// TestShippedProfilesBoundTheirSpill pins the spill limits of the shipped
// profiles to the rule of deploy/kubernetes/README.md ("Plattenplatz für
// Auslagerungen"): max_temp_directory_size is twice limits.memory, the
// container's ephemeral-storage limit 2 GiB above it, the request small, and
// a profile with a data volume spills into the emptyDir at /tmp.
func TestShippedProfilesBoundTheirSpill(t *testing.T) {
	for _, path := range shippedConfigs {
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for name, p := range cfg.Profiles {
			where := fmt.Sprintf("%s: profile %s", path, name)
			spill, ok := p.Settings["max_temp_directory_size"]
			if !ok {
				t.Errorf("%s sets no settings.max_temp_directory_size", where)
				continue
			}
			maxTemp := duckdbBytes(t, spill)
			memLimit, ok := p.Resources.Limits[corev1.ResourceMemory]
			if !ok {
				t.Errorf("%s has no limits.memory", where)
				continue
			}
			if got, want := maxTemp, 2*memLimit.Value(); got != want {
				t.Errorf("%s: max_temp_directory_size %s is %d bytes, want twice limits.memory (%d)", where, spill, got, want)
			}
			if m, ok := p.Settings["memory_limit"]; !ok || duckdbBytes(t, m) >= memLimit.Value() {
				t.Errorf("%s: memory_limit %q must stay below limits.memory %s", where, m, memLimit.String())
			}
			limit, ok := p.Resources.Limits[corev1.ResourceEphemeralStorage]
			if !ok {
				t.Errorf("%s has no limits.ephemeral-storage", where)
				continue
			}
			if got, want := limit.Value(), maxTemp+2*gib; got != want {
				t.Errorf("%s: limits.ephemeral-storage %s, want max_temp_directory_size plus 2 GiB (%d bytes)", where, limit.String(), want)
			}
			req, ok := p.Resources.Requests[corev1.ResourceEphemeralStorage]
			if !ok || req.Value() <= 0 || req.Value() > gib {
				t.Errorf("%s: requests.ephemeral-storage %s, want a small request (at most 1Gi)", where, req.String())
			}
			if p.Persistence != nil {
				if dir := p.Settings["temp_directory"]; !strings.HasPrefix(dir, "/tmp/") {
					t.Errorf("%s has a data volume but spills to %q, not below the emptyDir /tmp", where, dir)
				}
			}
		}
	}
}

// TestExampleShowsTheBaseValues checks the commented block of the reference
// configuration that claims to show the settings of base's default profile.
func TestExampleShowsTheBaseValues(t *testing.T) {
	const marker = "# The values of the default profile in deploy/kubernetes/base:"
	data, err := os.ReadFile(exampleConfig)
	if err != nil {
		t.Fatal(err)
	}
	// The block: the commented lines after the marker that read as YAML
	// ("settings:" and the lines indented below it).
	var block strings.Builder
	found := false
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !found {
			found = line == marker
			continue
		}
		yamlLine, ok := strings.CutPrefix(line, "# ")
		if !ok || !(yamlLine == "settings:" || strings.HasPrefix(yamlLine, "  ")) {
			break
		}
		block.WriteString(yamlLine + "\n")
	}
	if !found {
		t.Fatalf("%s: marker %q not found", exampleConfig, marker)
	}
	var shown struct {
		Settings map[string]string `json:"settings"`
	}
	if err := yaml.UnmarshalStrict([]byte(block.String()), &shown); err != nil {
		t.Fatalf("%s: the block after the marker is no settings map: %v\n%s", exampleConfig, err, block.String())
	}
	base, err := config.Load(baseConfig)
	if err != nil {
		t.Fatal(err)
	}
	if want := base.Profiles["default"].Settings; !maps.Equal(shown.Settings, want) {
		t.Errorf("%s shows the settings %v as base's, but %s has %v", exampleConfig, shown.Settings, baseConfig, want)
	}
}
