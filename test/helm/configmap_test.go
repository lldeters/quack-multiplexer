package helm

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// TestChartConfigMapName checks that the ConfigMap's name carries a hash of
// its content and that Helm keeps it. During an HA rollout whose new pods
// fail (a configuration the new gateway refuses, an image that cannot be
// pulled), the pods of the earlier ReplicaSet keep serving; when one of them
// restarts (lease loss, OOM, eviction), it must come back with the
// configuration it was started with. With one ConfigMap name for every
// configuration it would read the new one and could fail as well, until no
// gateway is left.
func TestChartConfigMapName(t *testing.T) {
	name := func(t *testing.T, objs []object) string {
		t.Helper()
		var configMaps []string
		for _, o := range objs {
			if str(o, "kind") == "ConfigMap" {
				configMaps = append(configMaps, str(o, "metadata", "name"))
			}
		}
		n := mountedConfigMap(t, objs)
		if len(configMaps) != 1 || configMaps[0] != n {
			t.Fatalf("ConfigMaps %v: want exactly the one the gateway mounts, %s", configMaps, n)
		}
		sum := sha256.Sum256([]byte(configText(t, objs)))
		if want := "quackmux-config-" + hex.EncodeToString(sum[:])[:10]; n != want {
			t.Errorf("ConfigMap %s: want %s (the hash of config.yaml)", n, want)
		}
		if p := str(mustFind(t, objs, "ConfigMap", n), "metadata", "annotations", "helm.sh/resource-policy"); p != "keep" {
			t.Errorf("ConfigMap %s: helm.sh/resource-policy %q, want keep: pods of earlier ReplicaSets mount it", n, p)
		}
		return n
	}
	base := name(t, render(t, "quackmux"))
	// The same values give the same name: an upgrade without a change of
	// the configuration rolls nothing out.
	if again := name(t, render(t, "quackmux")); again != base {
		t.Errorf("two renderings of the same values name the ConfigMap %s and %s", base, again)
	}
	for _, args := range [][]string{
		{"--set", "config.limits.maxEngines=80"},
		{"--set", "ha.enabled=true"},
	} {
		if n := name(t, render(t, "quackmux", args...)); n == base {
			t.Errorf("%v: the changed configuration has the ConfigMap name %s of the default one", args, n)
		}
	}
}
