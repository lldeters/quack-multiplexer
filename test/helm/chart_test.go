package helm

import (
	"bytes"
	"encoding/base64"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"quackmux/internal/auth"
	"quackmux/internal/config"
	"quackmux/internal/engine/kubernetes"
	"quackmux/internal/ha"
)

// The shipped configurations of deploy/kubernetes and the chart's copies.
var (
	baseConfig     = filepath.Join("deploy", "kubernetes", "base", "config.yaml")
	haConfig       = filepath.Join("deploy", "kubernetes", "overlays", "ha", "config.yaml")
	devConfig      = filepath.Join("deploy", "kubernetes", "overlays", "dev", "config.yaml")
	chartBaseCopy  = filepath.Join("deploy", "helm", "quackmux", "files", "config.yaml")
	chartHACopy    = filepath.Join("deploy", "helm", "quackmux", "files", "config-ha.yaml")
	placeholderTag = "CHANGE-ME-INSECURE-PLACEHOLDER"
)

func readRepoFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, path))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// update makes TestConfigFilesMatchKustomize copy the Kustomize
// configurations into the chart instead of failing.
var update = flag.Bool("update", false, "copy the Kustomize configurations into the chart (files/)")

// TestConfigFilesMatchKustomize needs no tools: the chart's default
// configurations are copies of the Kustomize ones, and a change there must
// reach the chart (go test ./test/helm/ -run TestConfigFilesMatchKustomize -update).
func TestConfigFilesMatchKustomize(t *testing.T) {
	for _, f := range []struct{ copy, source string }{
		{chartBaseCopy, baseConfig},
		{chartHACopy, haConfig},
	} {
		src := readRepoFile(t, f.source)
		if bytes.Equal(readRepoFile(t, f.copy), src) {
			continue
		}
		if *update {
			if err := os.WriteFile(filepath.Join(repoRoot, f.copy), src, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("updated %s from %s", filepath.ToSlash(f.copy), filepath.ToSlash(f.source))
			continue
		}
		t.Errorf("%s differs from %s, of which it is a copy; update it with\n"+
			"  go test ./test/helm/ -run TestConfigFilesMatchKustomize -update",
			filepath.ToSlash(f.copy), filepath.ToSlash(f.source))
	}
}

// TestChartMatchesKustomize compares the chart with default values to
// deploy/kubernetes/base and with ha.enabled to overlays/ha, object by
// object, apart from the documented differences (manifest_test.go).
func TestChartMatchesKustomize(t *testing.T) {
	for _, tc := range []struct {
		name, dir string
		args      []string
	}{
		{"base", filepath.Join("deploy", "kubernetes", "base"), nil},
		{"ha", filepath.Join("deploy", "kubernetes", "overlays", "ha"), []string{"--set", "ha.enabled=true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeHelm(render(t, "quackmux", tc.args...))
			want := normalizeKustomize(kustomize(t, tc.dir))
			for _, d := range diffObjects(t, "kubectl kustomize "+filepath.ToSlash(tc.dir), want, "helm template", got) {
				t.Error(d)
			}
		})
	}
}

// mountedConfigMap returns the name of the ConfigMap that the gateway mounts
// (volume "config").
func mountedConfigMap(t *testing.T, objs []object) string {
	t.Helper()
	for _, v := range listAt(mustFind(t, objs, "Deployment", "quackmux"), "spec", "template", "spec", "volumes") {
		if str(v, "name") == "config" {
			return str(v, "configMap", "name")
		}
	}
	t.Fatal("the gateway has no volume config")
	return ""
}

// configText returns config.yaml of the ConfigMap that the gateway mounts.
func configText(t *testing.T, objs []object) string {
	t.Helper()
	text, ok := get(mustFind(t, objs, "ConfigMap", mountedConfigMap(t, objs)), "data", "config.yaml").(string)
	if !ok {
		t.Fatal("the ConfigMap has no config.yaml")
	}
	return text
}

// validateConfig runs the checks of "quackmux validate" on a rendered
// configuration (as far as they apply to Kubernetes).
func validateConfig(t *testing.T, text string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatalf("the rendered configuration is invalid: %v\n%s", err, text)
	}
	var errs []error
	if cfg.Provider.Type == "kubernetes" {
		errs = append(errs, kubernetes.Validate(cfg))
	}
	if cfg.HA.Enabled {
		errs = append(errs, ha.Validate(cfg.HA))
	}
	if err := errors.Join(errs...); err != nil {
		t.Fatalf("the gateway would refuse the rendered configuration: %v", err)
	}
	return cfg
}

// yamlMap decodes YAML into a generic map (comments and order do not count).
func yamlMap(t *testing.T, text []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal(text, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestChartConfigs renders the configurations the chart can produce and
// checks them with the gateway's own validation.
func TestChartConfigs(t *testing.T) {
	t.Run("default is the base configuration", func(t *testing.T) {
		text := configText(t, render(t, "quackmux"))
		if text != string(readRepoFile(t, baseConfig)) {
			t.Errorf("config.yaml is not %s", baseConfig)
		}
		if cfg := validateConfig(t, text); cfg.HA.Enabled {
			t.Error("ha.enabled is set")
		}
	})
	t.Run("ha.enabled is the HA configuration", func(t *testing.T) {
		text := configText(t, render(t, "quackmux", "--set", "ha.enabled=true"))
		if text != string(readRepoFile(t, haConfig)) {
			t.Errorf("config.yaml is not %s", haConfig)
		}
		if cfg := validateConfig(t, text); !cfg.HA.Enabled {
			t.Error("ha.enabled is not set")
		}
	})
	t.Run("engines follow the release namespace", func(t *testing.T) {
		for _, args := range [][]string{nil, {"--set", "ha.enabled=true"}} {
			text := configText(t, render(t, "analytics", args...))
			if got := validateConfig(t, text).Provider.Kubernetes.Namespace; got != "analytics" {
				t.Errorf("%v: provider.kubernetes.namespace is %q, want analytics", args, got)
			}
			// Apart from the namespace it is the shipped configuration.
			src := baseConfig
			if args != nil {
				src = haConfig
			}
			want := yamlMap(t, readRepoFile(t, src))
			mapAt(want, "provider", "kubernetes")["namespace"] = "analytics"
			if got := yamlMap(t, []byte(text)); !reflect.DeepEqual(got, want) {
				t.Errorf("%v: the configuration differs from %s beyond the namespace:\n%s", args, src, text)
			}
		}
	})
	t.Run("values are merged in", func(t *testing.T) {
		text := configText(t, render(t, "quackmux",
			"--set", "config.limits.maxEngines=80",
			"--set", "config.audit.enabled=false",
			"--set", "config.limits.maxSessionsPerUser=0",
			"--set", "config.profiles.large=null",
			"--set", "config.profiles.default.image=registry.example.com/duckdb-engine:custom"))
		cfg := validateConfig(t, text)
		if cfg.Limits.MaxEngines != 80 || cfg.Limits.MaxSessionsPerUser != 0 || cfg.Audit.Enabled {
			t.Errorf("limits %+v, audit %+v: the values were not merged in", cfg.Limits, cfg.Audit)
		}
		if _, ok := cfg.Profiles["large"]; ok {
			t.Error("profile large was not removed")
		}
		def := cfg.Profiles["default"]
		if def.Image != "registry.example.com/duckdb-engine:custom" || def.Settings["memory_limit"] != "3GB" {
			t.Errorf("profile default %+v: want the shipped profile with the new image", def)
		}
		if cfg.Limits.MaxSessions != 2000 {
			t.Errorf("limits.maxSessions is %d: values that were not set must keep the shipped value", cfg.Limits.MaxSessions)
		}
	})
	t.Run("configYaml replaces the configuration", func(t *testing.T) {
		text := configText(t, render(t, "quackmux", "--set-file", "configYaml="+filepath.Join(repoRoot, devConfig)))
		if text != string(readRepoFile(t, devConfig)) {
			t.Errorf("config.yaml is not %s", devConfig)
		}
		validateConfig(t, text)
	})
}

// TestChartRefuses checks the configurations the chart refuses to render.
func TestChartRefuses(t *testing.T) {
	base := filepath.Join(repoRoot, baseConfig)
	haFile := filepath.Join(repoRoot, haConfig)
	for _, tc := range []struct {
		name, ns string
		args     []string
		want     string
	}{
		{"several replicas without leader election", "quackmux", []string{"--set", "replicas=2"}, "without leader election"},
		{"one replica with leader election", "quackmux", []string{"--set", "ha.enabled=true", "--set", "replicas=1"}, "no standby"},
		{"configuration without ha for ha.enabled", "quackmux", []string{"--set", "ha.enabled=true", "--set-file", "configYaml=" + base}, "must match"},
		{"configuration with ha without ha.enabled", "quackmux", []string{"--set-file", "configYaml=" + haFile}, "must match"},
		{"ha switched off by values", "quackmux", []string{"--set", "ha.enabled=true", "--set", "config.ha.enabled=false"}, "must match"},
		{"own configuration with another engine namespace", "analytics", []string{"--set-file", "configYaml=" + base}, "release namespace"},
		{"engine namespace by values", "quackmux", []string{"--set", "config.provider.kubernetes.namespace=other"}, "release namespace"},
		{"configuration that is no map", "quackmux", []string{"--set", "configYaml=- a"}, "not a YAML map"},
		{"service account without a name", "quackmux", []string{"--set", "serviceAccount.create=false"}, "serviceAccount.name is required"},
		{"unknown value", "quackmux", []string{"--set", "ha.enable=true"}, "enable"},
		// The chart stops rendering a Secret it generates once it is named as
		// existing: Helm would delete the users Secret and leave the others
		// unmanaged.
		{"own session key Secret as existing", "quackmux", []string{"--set", "secrets.sessionKey.existingSecret=quackmux-session-key"}, "the Secret that the chart generates"},
		{"own admin token Secret as existing", "quackmux", []string{"--set", "secrets.adminToken.existingSecret=quackmux-admin-token"}, "the Secret that the chart generates"},
		{"own users Secret as existing", "analytics", []string{"--set", "fullnameOverride=qmx", "--set", "secrets.users.existingSecret=qmx-users"}, "the Secret that the chart generates"},
		// The API refuses externalTrafficPolicy for ClusterIP and the
		// loadBalancer* fields for every type but LoadBalancer.
		{"external Service of type ClusterIP", "quackmux", []string{"--set", "externalService.enabled=true", "--set", "externalService.type=ClusterIP"}, "/externalService/type"},
		{"source ranges of a NodePort", "quackmux", []string{"--set", "externalService.enabled=true", "--set", "externalService.type=NodePort", "--set", "externalService.loadBalancerSourceRanges[0]=203.0.113.0/24"}, "need type LoadBalancer"},
		// More cases, compared with the gateway's own check: TestChartUsersMatchGateway.
		{"unknown field of a user", "quackmux", []string{"--set", "users[0].name=alice", "--set", "users[0].disable=true"}, "'disable' not allowed"},
		{"sha256 that is not hex", "quackmux", []string{"--set", "users[0].name=alice", "--set", "users[0].tokens[0].sha256=zz"}, "/users/0/tokens/0/sha256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := helmTemplate(t, tc.ns, tc.args...)
			if err == nil {
				t.Fatalf("helm template %v succeeded", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error with %q, got: %v", tc.want, err)
			}
		})
	}
}

// secretValue decodes the value of key in Secret name.
func secretValue(t *testing.T, objs []object, name, key string) string {
	t.Helper()
	s, _ := get(mustFind(t, objs, "Secret", name), "data", key).(string)
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("Secret %s, key %s: %v", name, key, err)
	}
	return string(b)
}

// mountedSecrets maps the gateway's secret volumes to their Secrets.
func mountedSecrets(t *testing.T, objs []object) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, v := range listAt(mustFind(t, objs, "Deployment", "quackmux"), "spec", "template", "spec", "volumes") {
		if name := str(v, "secret", "secretName"); name != "" {
			m[str(v, "name")] = name
		}
	}
	return m
}

// TestChartSecrets checks the generated Secrets and existing ones.
func TestChartSecrets(t *testing.T) {
	t.Run("generated", func(t *testing.T) {
		objs := render(t, "quackmux")
		key := secretValue(t, objs, "quackmux-session-key", "key")
		token := secretValue(t, objs, "quackmux-admin-token", "token")
		for what, v := range map[string]string{"session key": key, "admin token": token} {
			if len(v) != 43 || strings.Contains(v, placeholderTag) {
				t.Errorf("%s %q: want 43 random characters", what, v)
			}
		}
		if key == token {
			t.Error("the session key equals the admin token")
		}
		// Without users nobody can log in, but the file is valid.
		users := secretValue(t, objs, "quackmux-users", "users.yaml")
		validateUsers(t, users)
		if strings.TrimSpace(users) != "users: []" {
			t.Errorf("users.yaml is %q, want users: []", users)
		}
		want := map[string]string{"users": "quackmux-users", "admin-token": "quackmux-admin-token", "session-key": "quackmux-session-key"}
		if got := mountedSecrets(t, objs); !reflect.DeepEqual(got, want) {
			t.Errorf("mounted Secrets %v, want %v", got, want)
		}
		// Helm keeps the Secrets when the chart stops rendering them
		// (existingSecret, helm uninstall): pods of earlier ReplicaSets still
		// mount them, and a new release of the same name finds the session
		// key and the admin token again.
		for _, name := range []string{"quackmux-session-key", "quackmux-admin-token", "quackmux-users"} {
			if policy := str(mustFind(t, objs, "Secret", name), "metadata", "annotations", "helm.sh/resource-policy"); policy != "keep" {
				t.Errorf("Secret %s: helm.sh/resource-policy %q, want keep", name, policy)
			}
		}
	})
	t.Run("existing", func(t *testing.T) {
		objs := render(t, "quackmux",
			"--set", "secrets.sessionKey.existingSecret=my-session-key",
			"--set", "secrets.adminToken.existingSecret=my-admin-token",
			"--set", "secrets.users.existingSecret=my-users")
		for _, o := range objs {
			if str(o, "kind") == "Secret" {
				t.Errorf("the chart renders Secret %s although existing ones are named", str(o, "metadata", "name"))
			}
		}
		want := map[string]string{"users": "my-users", "admin-token": "my-admin-token", "session-key": "my-session-key"}
		if got := mountedSecrets(t, objs); !reflect.DeepEqual(got, want) {
			t.Errorf("mounted Secrets %v, want %v", got, want)
		}
	})
}

func validateUsers(t *testing.T, users string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "users.yaml")
	if err := os.WriteFile(path, []byte(users), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := auth.ValidateUsersFile(path); err != nil {
		t.Fatalf("the gateway would refuse the users file: %v\n%s", err, users)
	}
}

// TestChartUsers renders the users of testdata/values-users.yaml.
func TestChartUsers(t *testing.T) {
	objs := render(t, "quackmux", "-f", filepath.Join("testdata", "values-users.yaml"))
	users := secretValue(t, objs, "quackmux-users", "users.yaml")
	validateUsers(t, users)
	for _, want := range []string{"name: alice", "analytics", "sha256: 5e884898da28047151d0e56f8dc6292773603d0d6aabbdd62a11ef721d1542d8", "expires: \"2027-06-30T00:00:00Z\""} {
		if !strings.Contains(users, want) {
			t.Errorf("users.yaml lacks %q:\n%s", want, users)
		}
	}
}

// TestChartOptions renders testdata/values-options.yaml (every optional
// object and field, CI checks it with kubeconform as well).
func TestChartOptions(t *testing.T) {
	objs := render(t, "quackmux", "-f", filepath.Join("testdata", "values-options.yaml"))

	ext := mustFind(t, objs, "Service", "quackmux-external")
	if got := mapAt(ext, "spec", "selector"); got["quackmux.io/leader"] != "true" {
		t.Errorf("the external Service selects %v: with ha.enabled it must select the leader", got)
	}
	if ports := listAt(ext, "spec", "ports"); len(ports) != 1 || str(ports[0], "name") != "quack" {
		t.Errorf("the external Service exposes %v: only the Quack port", ports)
	}
	if got := str(ext, "spec", "externalTrafficPolicy"); got != "Local" {
		t.Errorf("externalTrafficPolicy %q", got)
	}

	np := mustFind(t, objs, "NetworkPolicy", "quackmux-gateway")
	admin := listAt(np, "spec", "ingress")
	if len(admin) != 2 || len(listAt(admin[1], "from")) != 2 {
		t.Errorf("the admin port rule of the gateway policy has the peers %v: want the namespace's pods and adminFrom", admin)
	}
	if got := listAt(mustFind(t, objs, "NetworkPolicy", "quackmux-engines"), "spec", "egress"); len(got) != 1 {
		t.Errorf("engine egress %v: want networkPolicy.engineEgress", got)
	}

	dep := mustFind(t, objs, "Deployment", "quackmux")
	pod := mapAt(dep, "spec", "template", "spec")
	if got := str(listAt(pod, "containers")[0], "image"); got != "registry.example.com/quackmux:1.2.3" {
		t.Errorf("image %q", got)
	}
	var env []string
	for _, e := range listAt(listAt(pod, "containers")[0], "env") {
		env = append(env, str(e, "name"))
	}
	if want := []string{"POD_NAME", "POD_NAMESPACE", "GOMEMLIMIT", "QUACKMUX_EXTRA"}; !reflect.DeepEqual(env, want) {
		t.Errorf("env %v, want %v", env, want)
	}
	// A nodeAffinity keeps the anti-affinity of ha.enabled: a node failure
	// must leave a follower.
	if got := mapAt(pod, "affinity"); got["nodeAffinity"] == nil || got["podAntiAffinity"] == nil {
		t.Errorf("affinity %v: want the nodeAffinity of the values and the default podAntiAffinity", got)
	}
	if got := get(dep, "spec", "replicas"); got != float64(3) {
		t.Errorf("replicas %v, want 3", got)
	}
	if got := mapAt(dep, "spec", "strategy", "rollingUpdate"); got["maxSurge"] != float64(0) || got["maxUnavailable"] != float64(1) {
		t.Errorf("rollingUpdate %v", got)
	}
	if find(objs, "PodDisruptionBudget", "quackmux") == nil {
		t.Error("no PodDisruptionBudget with ha.enabled")
	}
	if sa := mustFind(t, objs, "ServiceAccount", "quackmux-gateway"); mapAt(sa, "metadata", "annotations")["iam.example.com/role"] != "quackmux" {
		t.Errorf("ServiceAccount annotations %v", mapAt(sa, "metadata", "annotations"))
	}

	t.Run("podAntiAffinity of the values", func(t *testing.T) {
		values := filepath.Join(t.TempDir(), "values.yaml")
		own := "affinity:\n  podAntiAffinity:\n    requiredDuringSchedulingIgnoredDuringExecution:\n" +
			"      - topologyKey: topology.kubernetes.io/zone\n        labelSelector: {matchLabels: {app.kubernetes.io/component: gateway}}\n"
		for _, tc := range []struct {
			name, values string
			want         []string // the keys of the rendered podAntiAffinity
		}{
			{"replaces the default", own, []string{"requiredDuringSchedulingIgnoredDuringExecution"}},
			{"empty turns it off", "affinity: {podAntiAffinity: {}}\n", []string{}},
		} {
			if err := os.WriteFile(values, []byte(tc.values), 0o600); err != nil {
				t.Fatal(err)
			}
			dep := mustFind(t, render(t, "quackmux", "--set", "ha.enabled=true", "-f", values), "Deployment", "quackmux")
			anti := mapAt(dep, "spec", "template", "spec", "affinity", "podAntiAffinity")
			if got := sortedKeys(anti); anti == nil || !slices.Equal(got, tc.want) {
				t.Errorf("%s: podAntiAffinity %v, want the keys %v", tc.name, anti, tc.want)
			}
		}
	})

	t.Run("NodePort", func(t *testing.T) {
		ext := mustFind(t, render(t, "quackmux", "--set", "externalService.enabled=true", "--set", "externalService.type=NodePort"),
			"Service", "quackmux-external")
		spec := mapAt(ext, "spec")
		if spec["type"] != "NodePort" || spec["externalTrafficPolicy"] != "Local" {
			t.Errorf("spec %v: want type NodePort with externalTrafficPolicy Local", spec)
		}
		for _, f := range []string{"loadBalancerSourceRanges", "loadBalancerClass"} {
			if _, ok := spec[f]; ok {
				t.Errorf("a NodePort Service with %s: the API refuses it", f)
			}
		}
	})
}
