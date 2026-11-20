package kubernetes

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"quackmux/internal/config"
	"quackmux/internal/engine"
)

// The init SQL may hold credentials: it lives in the engine's secret, not in
// the pod spec that everybody allowed to read pods can see.
func TestInitSQLLivesInTheEngineSecret(t *testing.T) {
	cfg := testConfig(t, `
profiles:
  default:
    initSQL: "CREATE SECRET s3 (TYPE s3, KEY_ID 'AKIA', SECRET 'very-secret');"
`)
	e := newTestEnv(t, cfg)
	key := engine.Key{ShareLevel: engine.ShareUser, Owner: "alice", Subdomain: "default", Profile: "default"}
	pod, secret := e.enginePod("quack-user-alice-abcde", key, false, "tok")
	for _, c := range pod.Spec.Containers {
		for _, v := range c.Env {
			if strings.Contains(v.Value, "very-secret") {
				t.Fatalf("credential in the pod spec: %s=%q", v.Name, v.Value)
			}
		}
	}
	if got := string(secret.Data[initSQLDataKey]); !strings.Contains(got, "very-secret") {
		t.Fatalf("init SQL missing from the engine secret: %q", got)
	}
	if pod.Annotations[AnnotationInitSQLHash] == "" {
		t.Fatal("no init SQL hash on the pod")
	}

	// A changed init SQL changes the spec hash (rolling replacement).
	prof := cfg.Profiles["default"]
	h1 := e.p.SpecHash("default", prof)
	prof.InitSQL = "SELECT 1;"
	if h2 := e.p.SpecHash("default", prof); h1 == h2 {
		t.Fatal("the spec hash ignores the init SQL")
	}
}

// Literal values are escaped against the kubelet's $(VAR) expansion: a user
// named "$(QUACK_TOKEN)" must not get the token into the engine's log.
func TestLiteralEnvValuesAreEscaped(t *testing.T) {
	e := newTestEnv(t, testConfig(t, ""))
	owner := "$" + "(QUACK_TOKEN)"
	key := engine.Key{ShareLevel: engine.ShareUser, Owner: owner, Subdomain: "default", Profile: "default"}
	pod, _ := e.enginePod("quack-user-x-abcde", key, false, "tok")
	got, _ := envVar(container(pod, ContainerName), engine.EnvOwner)
	if want := "$" + owner; got.Value != want {
		t.Fatalf("%s = %q, want %q", engine.EnvOwner, got.Value, want)
	}
}

// Engines tolerate a lost node for a minute (not the default five), unless
// the profile says otherwise.
func TestNodeLossTolerations(t *testing.T) {
	e := newTestEnv(t, testConfig(t, `
profiles:
  default: {}
  patient:
    tolerations:
      - key: node.kubernetes.io/unreachable
        operator: Exists
        effect: NoExecute
        tolerationSeconds: 600
`))
	seconds := func(ts []corev1.Toleration, key string) int64 {
		n := int64(-1)
		for _, t := range ts {
			if t.Key == key {
				if n != -1 {
					return -2 // twice
				}
				n = *t.TolerationSeconds
			}
		}
		return n
	}
	key := engine.Key{ShareLevel: engine.ShareUser, Owner: "alice", Subdomain: "default", Profile: "default"}
	pod, _ := e.enginePod("quack-user-alice-abcde", key, false, "tok")
	notReady, unreachable := seconds(pod.Spec.Tolerations, "node.kubernetes.io/not-ready"), seconds(pod.Spec.Tolerations, "node.kubernetes.io/unreachable")
	if notReady != 60 || unreachable != 60 {
		t.Fatalf("not-ready %d s, unreachable %d s, want 60", notReady, unreachable)
	}
	key.Profile = "patient"
	pod, _ = e.enginePod("quack-user-alice-fghij", key, false, "tok")
	if s := seconds(pod.Spec.Tolerations, "node.kubernetes.io/unreachable"); s != 600 {
		t.Fatalf("the profile's toleration was replaced: %d", s)
	}
}

// The podTemplate's engine image and pull policy apply unless the profile
// sets its own; without both, the defaults do.
func TestTemplateImageAndPullPolicy(t *testing.T) {
	cfg := testConfig(t, `
profiles:
  default: {}
  templated:
    podTemplate:
      spec:
        containers:
          - name: engine
            image: registry.example/quack/engine:2.0.0-lake
            imagePullPolicy: Always
  profiled:
    image: registry.example/quack/engine:2.0.0-other
    imagePullPolicy: Never
    podTemplate:
      spec:
        containers:
          - name: engine
            image: registry.example/quack/engine:2.0.0-lake
            imagePullPolicy: Always
`)
	e := newTestEnv(t, cfg)
	for profile, want := range map[string][2]string{
		"default":   {config.DefaultEngineImage, string(corev1.PullIfNotPresent)},
		"templated": {"registry.example/quack/engine:2.0.0-lake", string(corev1.PullAlways)},
		"profiled":  {"registry.example/quack/engine:2.0.0-other", string(corev1.PullNever)},
	} {
		key := engine.Key{ShareLevel: engine.ShareUser, Owner: "alice", Subdomain: "default", Profile: profile}
		pod, _ := e.enginePod("quack-user-alice-abcde", key, false, "tok")
		c := container(pod, ContainerName)
		if c.Image != want[0] || string(c.ImagePullPolicy) != want[1] {
			t.Errorf("%s: image %q, pull policy %q, want %v", profile, c.Image, c.ImagePullPolicy, want)
		}
	}
}

// The engine never restarts in place: a container-level restart policy or
// restart rules from the template are removed, and an engine container that
// restarted all the same is reported failed.
func TestEngineContainerNeverRestartsInPlace(t *testing.T) {
	cfg := testConfig(t, `
profiles:
  default:
    podTemplate:
      spec:
        containers:
          - name: engine
            restartPolicy: Always
            restartPolicyRules:
              - action: Restart
                exitCodes: {operator: In, values: [137]}
`)
	e := newTestEnv(t, cfg)
	pod, _ := e.enginePod("quack-user-alice-abcde", userKey, false, "tok")
	if c := container(pod, ContainerName); c.RestartPolicy != nil || c.RestartPolicyRules != nil {
		t.Fatalf("engine container restart policy %v, rules %v", c.RestartPolicy, c.RestartPolicyRules)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("pod restart policy %s", pod.Spec.RestartPolicy)
	}

	pod.Status = readyStatus("10.0.0.3")
	pod.Status.ContainerStatuses[0].RestartCount = 1
	pod.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137},
	}
	phase, endpoint, msg := podStatus(pod)
	if phase != engine.PhaseFailed || endpoint != "" || !strings.Contains(msg, "restarted") || !strings.Contains(msg, "OOMKilled") {
		t.Fatalf("restarted engine: phase %s, endpoint %q, message %q", phase, endpoint, msg)
	}
}

// The engine stops DuckDB within the pod's grace period: the gateway sets
// QUACKMUX_STOP_TIMEOUT from provider.kubernetes.deleteGracePeriod, and the
// profile cannot override it.
func TestStopTimeoutFollowsGracePeriod(t *testing.T) {
	for grace, want := range map[string]string{"": "25", "90s": "85", "5s": "1", "2500ms": "1", "1s": "1"} {
		extra := "profiles: {default: {env: [{name: QUACKMUX_STOP_TIMEOUT, value: '600'}]}}\n"
		if grace != "" {
			extra += "provider: {kubernetes: {deleteGracePeriod: " + grace + "}}\n"
		}
		e := newTestEnv(t, testConfig(t, extra))
		pod, _ := e.enginePod("quack-user-alice-abcde", userKey, false, "tok")
		got, n := envVar(container(pod, ContainerName), engine.EnvStopTimeout)
		if n != 1 || got.Value != want {
			t.Errorf("grace %q: %s = %q (%d times), want %q", grace, engine.EnvStopTimeout, got.Value, n, want)
		}
		if g := *pod.Spec.TerminationGracePeriodSeconds; grace == "" && g != 30 {
			t.Errorf("default termination grace period %d", g)
		}
	}
	// A grace period below a second would force-delete running engines.
	for _, grace := range []string{"0s", "500ms", "-5s"} {
		cfg := testConfig(t, "provider: {kubernetes: {deleteGracePeriod: "+grace+"}}\n")
		if _, err := New(cfg, fake.NewSimpleClientset(), nil); err == nil || !strings.Contains(err.Error(), "deleteGracePeriod") {
			t.Errorf("grace %s accepted (err %v)", grace, err)
		}
	}
}

// Validate ("quackmux validate") finds what New would reject, without a
// cluster.
func TestValidateWithoutCluster(t *testing.T) {
	for name, yaml := range map[string]string{
		"instance":           "provider: {kubernetes: {instance: 'team a'}}\n",
		"namespace":          "provider: {kubernetes: {namespace: Engines}}\n",
		"grace":              "provider: {kubernetes: {deleteGracePeriod: 0s}}\n",
		"label":              "profiles: {default: {labels: {'bad key': x}}}\n",
		"pull policy":        "profiles: {default: {imagePullPolicy: Sometimes}}\n",
		"template pull poli": "profiles: {default: {podTemplate: {spec: {containers: [{name: engine, imagePullPolicy: Sometimes}]}}}}\n",
	} {
		cfg, err := config.Parse([]byte("auth: {mode: none}\n" + yaml))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := Validate(cfg); err == nil {
			t.Errorf("%s: Validate accepted the configuration", name)
		}
	}
	cfg, err := config.Parse([]byte("auth: {mode: none}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("default configuration: %v", err)
	}
}
