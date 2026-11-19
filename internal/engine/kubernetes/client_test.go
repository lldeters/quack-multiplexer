package kubernetes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"quackmux/internal/config"
)

const testKubeconfig = `apiVersion: v1
kind: Config
clusters:
  - name: test
    cluster: {server: "https://127.0.0.1:6443"}
users:
  - name: test
    user: {token: secret-token}
contexts:
  - name: test
    context: {cluster: test, user: test}
current-context: test
`

func TestNewClientFromKubeconfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(path, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	client, rc, err := NewClient(config.KubernetesProviderConfig{Kubeconfig: path, QPS: 7, Burst: 9})
	if err != nil {
		t.Fatal(err)
	}
	if client == nil || rc.Host != "https://127.0.0.1:6443" || rc.QPS != 7 || rc.Burst != 9 || rc.UserAgent != "quackmux" ||
		rc.BearerToken != "secret-token" {
		t.Errorf("rest config %+v", rc)
	}
	if _, _, err := NewClient(config.KubernetesProviderConfig{Kubeconfig: filepath.Join(dir, "missing")}); err == nil {
		t.Error("a missing kubeconfig must fail")
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") == "" {
		if _, _, err := NewClient(config.KubernetesProviderConfig{}); err == nil || !strings.Contains(err.Error(), "not running in a cluster") {
			t.Errorf("in-cluster configuration outside a cluster: %v", err)
		}
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	for in, want := range map[string]string{
		"~":                        home,
		"~/.kube/config":           filepath.Join(home, ".kube", "config"),
		`~\.kube\config`:           filepath.Join(home, `\.kube\config`),
		"/etc/quackmux/kubeconfig": "/etc/quackmux/kubeconfig",
		"relative/~/path":          "relative/~/path",
	} {
		got, err := expandHome(in)
		if err != nil || got != want {
			t.Errorf("expandHome(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
