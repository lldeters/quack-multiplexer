package kubernetes

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

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
