package kubernetes

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"quackmux/internal/engine"
)

// The post-start SQL travels like the init SQL: in the engine's secret
// (key post-start.sql), referenced by QUACKMUX_POST_START_SQL, never as a
// literal in the pod spec; its hash is an annotation, so that a change
// replaces the engines of the profile.
func TestPostStartSQLLivesInTheEngineSecret(t *testing.T) {
	cfg := testConfig(t, `
profiles:
  default:
    initSQL: ".read /usr/local/share/quackmux/authorize.sql"
    postStartSQL: |
      SET allowed_directories = ['https://user:very-secret@example.com/'];
      SET enable_external_access = false;
      SET lock_configuration = true
    env:
      - {name: QUACKMUX_POST_START_SQL, value: "SET enable_external_access = true;"}
`)
	e := newTestEnv(t, cfg)
	pod, secret := e.enginePod("quack-user-alice-abcde", userKey, false, "tok")
	for _, c := range pod.Spec.Containers {
		for _, v := range c.Env {
			if strings.Contains(v.Value, "very-secret") || strings.Contains(v.Value, "enable_external_access") {
				t.Fatalf("post-start SQL in the pod spec: %s=%q", v.Name, v.Value)
			}
		}
	}
	c := container(pod, ContainerName)
	got, n := envVar(c, engine.EnvPostStartSQL)
	if n != 1 || got.Value != "" || got.ValueFrom == nil || got.ValueFrom.SecretKeyRef == nil ||
		got.ValueFrom.SecretKeyRef.Name != "quack-user-alice-abcde-token" || got.ValueFrom.SecretKeyRef.Key != postStartSQLDataKey {
		t.Fatalf("env %s = %+v (%d times), want one reference to the engine secret", engine.EnvPostStartSQL, got, n)
	}

	prof := cfg.Profiles["default"]
	want := "SET allowed_directories = ['https://user:very-secret@example.com/'];\n" +
		"SET enable_external_access = false;\nSET lock_configuration = true;\n"
	if sql := string(secret.Data[postStartSQLDataKey]); sql != want || engine.RenderPostStartSQL(prof) != want {
		t.Fatalf("post-start SQL in the secret %q, want %q", sql, want)
	}
	// A trailing dot command gets its ";" on a line of its own (the CLI would
	// read the file "authorize.sql;").
	if sql := string(secret.Data[initSQLDataKey]); sql != ".read /usr/local/share/quackmux/authorize.sql\n;\n" {
		t.Fatalf("init SQL in the secret %q", sql)
	}
	sum := sha256.Sum256([]byte(want))
	if a := pod.Annotations[AnnotationPostStartSQLHash]; a != hex.EncodeToString(sum[:]) {
		t.Fatalf("annotation %s = %q", AnnotationPostStartSQLHash, a)
	}

	// A changed post-start SQL changes the spec hash (rolling replacement).
	h1 := e.p.SpecHash("default", prof)
	prof.PostStartSQL = "SET lock_configuration = true;"
	if h2 := e.p.SpecHash("default", prof); h1 == h2 {
		t.Fatal("the spec hash ignores the post-start SQL")
	}

	// Without post-start SQL there is neither the variable nor the key.
	prof.PostStartSQL = " \n"
	pod = e.p.pod(podInput{Name: "quack-user-alice-fghij", Key: userKey, User: "alice", ProfileName: "default", Profile: prof})
	if _, n := envVar(container(pod, ContainerName), engine.EnvPostStartSQL); n != 0 {
		t.Fatalf("%s set %d times without post-start SQL", engine.EnvPostStartSQL, n)
	}
	if _, ok := pod.Annotations[AnnotationPostStartSQLHash]; ok {
		t.Fatal("post-start SQL hash without post-start SQL")
	}
	if _, ok := e.p.secret("quack-user-alice-fghij", "tok", prof).Data[postStartSQLDataKey]; ok {
		t.Fatal("post-start SQL key in the secret without post-start SQL")
	}
}
