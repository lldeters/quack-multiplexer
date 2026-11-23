package kubernetes

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"quackmux/internal/config"
	"quackmux/internal/engine"
)

const testNamespace = "engines"

var userKey = engine.Key{ShareLevel: engine.ShareUser, Owner: "alice", Subdomain: "default", Profile: "default"}

// syncBuffer collects the log output of background goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// testConfig parses a configuration (with defaults) for the test namespace.
func testConfig(t *testing.T, yaml string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte("auth: {mode: none}\n" + yaml))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	cfg.Provider.Kubernetes.Namespace = testNamespace
	return cfg
}

type testEnv struct {
	p      *Provider
	client *fake.Clientset
	logs   *syncBuffer
}

func newTestEnv(t *testing.T, cfg *config.Config, objects ...runtime.Object) *testEnv {
	t.Helper()
	client := fake.NewSimpleClientset(objects...)
	logs := &syncBuffer{}
	p, err := New(cfg, client, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	// Tests that exercise the replay after List shorten this themselves.
	p.replayDelay = time.Hour
	return &testEnv{p: p, client: client, logs: logs}
}

// start runs the provider until the test ends and waits for the pod cache.
func (e *testEnv) start(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.p.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Run did not return after cancellation")
		}
	})
	wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
	defer wcancel()
	if err := e.p.WaitSynced(wctx); err != nil {
		t.Fatalf("WaitSynced: %v", err)
	}
	return ctx
}

// enginePod renders a pod and its token secret the way Launch creates them
// (for engines that "already exist" in the cluster).
func (e *testEnv) enginePod(name string, key engine.Key, warm bool, token string) (*corev1.Pod, *corev1.Secret) {
	prof := e.p.cfg.Profiles[key.Profile]
	pod := e.p.pod(podInput{
		Name: name, Key: key, Warm: warm, User: key.Owner, ProfileName: key.Profile, Profile: prof,
		SpecHash: e.p.SpecHash(key.Profile, prof),
	})
	return pod, e.p.secret(name, token, prof)
}

// add stores objects in the fake cluster.
func (e *testEnv) add(t *testing.T, objects ...runtime.Object) {
	t.Helper()
	for _, o := range objects {
		if err := e.client.Tracker().Add(o); err != nil {
			t.Fatal(err)
		}
	}
}

func readyStatus(ip string) corev1.PodStatus {
	return corev1.PodStatus{
		Phase: corev1.PodRunning,
		PodIP: ip,
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: ContainerName, Ready: true,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}},
	}
}

// waitEvent reads events until one matches.
func waitEvent(t *testing.T, p *Provider, what string, match func(engine.Instance) bool) engine.Instance {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case inst, ok := <-p.Events():
			if !ok {
				t.Fatalf("events channel closed while waiting for %s", what)
			}
			if match(inst) {
				return inst
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// collectUntil reads events up to and including the first that matches.
func collectUntil(t *testing.T, p *Provider, what string, match func(engine.Instance) bool) []engine.Instance {
	t.Helper()
	var seen []engine.Instance
	waitEvent(t, p, what, func(i engine.Instance) bool {
		seen = append(seen, i)
		return match(i)
	})
	return seen
}

// noEvent asserts that no event matches within d.
func noEvent(t *testing.T, p *Provider, d time.Duration, match func(engine.Instance) bool) {
	t.Helper()
	timeout := time.After(d)
	for {
		select {
		case inst, ok := <-p.Events():
			if !ok {
				return
			}
			if match(inst) {
				t.Fatalf("unexpected event %+v", inst)
			}
		case <-timeout:
			return
		}
	}
}

// waitFor polls cond until it holds. The deadline is generous: under the
// race detector and a loaded machine a few goroutine hops can take seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func envVar(c *corev1.Container, name string) (corev1.EnvVar, int) {
	n := 0
	var found corev1.EnvVar
	for _, e := range c.Env {
		if e.Name == name {
			found = e
			n++
		}
	}
	return found, n
}

func container(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	return nil
}
