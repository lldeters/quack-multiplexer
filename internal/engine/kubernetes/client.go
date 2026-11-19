package kubernetes

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	kclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"quackmux/internal/config"
)

// serviceAccountNamespaceFile holds the namespace of the pod the gateway runs in.
const serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// userAgent identifies the gateway at the API server.
const userAgent = "quackmux"

// NewClient creates a Kubernetes client. Without cfg.Kubeconfig the
// in-cluster configuration (service account) is used, otherwise the given
// kubeconfig file. QPS and Burst are taken from the configuration. The
// returned rest.Config can be used for further clients (leader election).
func NewClient(cfg config.KubernetesProviderConfig) (kclient.Interface, *rest.Config, error) {
	var (
		rc  *rest.Config
		err error
	)
	if cfg.Kubeconfig == "" {
		rc, err = rest.InClusterConfig()
		if err != nil {
			if errors.Is(err, rest.ErrNotInCluster) {
				return nil, nil, fmt.Errorf("kubernetes: not running in a cluster; set provider.kubernetes.kubeconfig: %w", err)
			}
			return nil, nil, fmt.Errorf("kubernetes: in-cluster configuration: %w", err)
		}
	} else {
		path, perr := expandHome(cfg.Kubeconfig)
		if perr != nil {
			return nil, nil, fmt.Errorf("kubernetes: kubeconfig %q: %w", cfg.Kubeconfig, perr)
		}
		rc, err = clientcmd.BuildConfigFromFlags("", path)
		if err != nil {
			return nil, nil, fmt.Errorf("kubernetes: load kubeconfig %q: %w", path, err)
		}
	}
	if cfg.QPS > 0 {
		rc.QPS = cfg.QPS
	}
	if cfg.Burst > 0 {
		rc.Burst = cfg.Burst
	}
	if rc.UserAgent == "" {
		rc.UserAgent = userAgent
	}
	client, err := kclient.NewForConfig(rc)
	if err != nil {
		return nil, nil, fmt.Errorf("kubernetes: create client: %w", err)
	}
	return client, rc, nil
}

// expandHome resolves a leading "~/" (kubeconfig paths are often written so).
func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, `~\`) {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, path[1:]), nil
}

// resolveNamespace picks the namespace for engine pods: the configured one,
// else $POD_NAMESPACE, else the namespace of the gateway's service account,
// else "default".
func resolveNamespace(configured string, getenv func(string) string, readFile func(string) ([]byte, error)) string {
	if ns := strings.TrimSpace(configured); ns != "" {
		return ns
	}
	if ns := strings.TrimSpace(getenv("POD_NAMESPACE")); ns != "" {
		return ns
	}
	if b, err := readFile(serviceAccountNamespaceFile); err == nil {
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	return "default"
}
