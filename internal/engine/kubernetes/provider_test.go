package kubernetes

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"quackmux/internal/config"
	"quackmux/internal/engine"
)

func launchRequest(p *Provider, key engine.Key, warm bool, user, token string) engine.LaunchRequest {
	prof := p.cfg.Profiles[key.Profile]
	return engine.LaunchRequest{
		ID: p.NewID(key, warm), Key: key, Warm: warm, User: user, ProfileName: key.Profile, Profile: prof,
		Token: token, SpecHash: p.SpecHash(key.Profile, prof),
	}
}

func keyFor(owner string) engine.Key {
	return engine.Key{ShareLevel: engine.ShareUser, Owner: owner, Subdomain: "default", Profile: "default"}
}

func TestNewNamespace(t *testing.T) {
	cfg := testConfig(t, "")
	e := newTestEnv(t, cfg)
	if e.p.Namespace() != testNamespace || e.p.Name() != "kubernetes" || !e.p.CanLaunch() {
		t.Fatalf("namespace %q name %q", e.p.Namespace(), e.p.Name())
	}
	cfg.Provider.Kubernetes.Namespace = ""
	t.Setenv("POD_NAMESPACE", "from-env")
	p, err := New(cfg, fake.NewSimpleClientset(), nil)
	if err != nil || p.Namespace() != "from-env" {
		t.Fatalf("namespace from $POD_NAMESPACE: %v %v", p, err)
	}
}

func TestResolveNamespace(t *testing.T) {
	noEnv := func(string) string { return "" }
	env := func(k string) string {
		if k == "POD_NAMESPACE" {
			return "env-ns"
		}
		return ""
	}
	file := func(path string) ([]byte, error) {
		if path != serviceAccountNamespaceFile {
			t.Errorf("read %s", path)
		}
		return []byte(" sa-ns\n"), nil
	}
	noFile := func(string) ([]byte, error) { return nil, os.ErrNotExist }
	for _, c := range []struct {
		configured string
		getenv     func(string) string
		readFile   func(string) ([]byte, error)
		want       string
	}{
		{"configured", env, file, "configured"},
		{"", env, file, "env-ns"},
		{"", noEnv, file, "sa-ns"},
		{" ", noEnv, noFile, "default"},
	} {
		if got := resolveNamespace(c.configured, c.getenv, c.readFile); got != c.want {
			t.Errorf("resolveNamespace(%q) = %q, want %q", c.configured, got, c.want)
		}
	}
}

func TestNewRejectsInvalidSettings(t *testing.T) {
	for name, mutate := range map[string]func(*config.Config){
		"instance":  func(c *config.Config) { c.Provider.Kubernetes.Instance = "not valid!" },
		"namespace": func(c *config.Config) { c.Provider.Kubernetes.Namespace = "Not_A_Namespace" },
		"grace":     func(c *config.Config) { c.Provider.Kubernetes.DeleteGracePeriod = config.D(-time.Second) },
	} {
		cfg := testConfig(t, "")
		mutate(cfg)
		if _, err := New(cfg, fake.NewSimpleClientset(), nil); err == nil {
			t.Errorf("%s: invalid setting accepted", name)
		}
	}
	if _, err := New(nil, fake.NewSimpleClientset(), nil); err == nil {
		t.Error("nil config accepted")
	}
	if _, err := New(testConfig(t, ""), nil, nil); err == nil {
		t.Error("nil client accepted")
	}
}

func TestLaunch(t *testing.T) {
	e := newTestEnv(t, testConfig(t, ""))
	e.client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		a.(k8stesting.CreateAction).GetObject().(*corev1.Pod).UID = "pod-uid-1" // the fake assigns none
		return false, nil, nil
	})
	ctx := context.Background()
	req := launchRequest(e.p, userKey, false, "alice", "tok-123")
	inst, err := e.p.Launch(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if inst.ID != req.ID || inst.Phase != engine.PhaseStarting || inst.Token != "tok-123" || inst.Key != userKey ||
		inst.User != "alice" || inst.ProfileName != "default" || inst.SpecHash != req.SpecHash || inst.Endpoint != "" ||
		inst.Warm || inst.CreatedAt.IsZero() {
		t.Fatalf("instance %+v", inst)
	}

	s, err := e.client.CoreV1().Secrets(testNamespace).Get(ctx, req.ID+"-token", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(s.Data["token"]) != "tok-123" || s.Labels[LabelManagedBy] != "quackmux" || s.Labels[LabelInstance] != "default" ||
		s.Labels[LabelEngine] != req.ID {
		t.Errorf("secret %+v", s)
	}
	refs := s.OwnerReferences
	if len(refs) != 1 || refs[0].APIVersion != "v1" || refs[0].Kind != "Pod" || refs[0].Name != req.ID || refs[0].UID != "pod-uid-1" ||
		refs[0].BlockOwnerDeletion == nil || *refs[0].BlockOwnerDeletion || (refs[0].Controller != nil && *refs[0].Controller) {
		t.Errorf("owner references %+v", refs)
	}

	pod, err := e.client.CoreV1().Pods(testNamespace).Get(ctx, req.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pod.Annotations[AnnotationSpecHash] != req.SpecHash || pod.Annotations[AnnotationTokenSecret] != s.Name ||
		pod.Annotations[AnnotationUser] != "alice" || pod.Labels[LabelKeyHash] != userKey.Hash() {
		t.Errorf("pod metadata %v %v", pod.Labels, pod.Annotations)
	}
	if tok, _ := envVar(container(pod, ContainerName), engine.EnvToken); tok.ValueFrom.SecretKeyRef.Name != s.Name {
		t.Errorf("QUACK_TOKEN refers to %q", tok.ValueFrom.SecretKeyRef.Name)
	}

	var order []string
	for _, a := range e.client.Actions() {
		if v := a.GetVerb(); v == "create" || v == "patch" || v == "delete" {
			order = append(order, v+" "+a.GetResource().Resource)
		}
	}
	if strings.Join(order, ", ") != "create secrets, create pods, patch secrets" {
		t.Errorf("API calls: %v (the secret must exist before the pod, the owner reference comes after it)", order)
	}
	if !e.p.nameTaken(req.ID) {
		t.Error("NewID must not hand out the name of a launched engine")
	}
}

func TestLaunchFailures(t *testing.T) {
	ctx := context.Background()

	e := newTestEnv(t, testConfig(t, ""))
	e.client.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "x", errors.New("exceeded quota"))
	})
	req := launchRequest(e.p, userKey, false, "alice", "tok")
	if _, err := e.p.Launch(ctx, req); err == nil || !strings.Contains(err.Error(), "exceeded quota") {
		t.Fatalf("Launch: %v", err)
	}
	if _, err := e.client.CoreV1().Secrets(testNamespace).Get(ctx, req.ID+"-token", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the secret of a failed launch must be deleted: %v", err)
	}
	if e.p.nameTaken(req.ID) {
		t.Error("the token of a failed launch must be forgotten")
	}

	e = newTestEnv(t, testConfig(t, ""))
	e.client.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("etcd unavailable")
	})
	if _, err := e.p.Launch(ctx, launchRequest(e.p, userKey, false, "alice", "tok")); err == nil || !strings.Contains(err.Error(), "etcd unavailable") {
		t.Fatalf("Launch: %v", err)
	}
	if pods, _ := e.client.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{}); len(pods.Items) != 0 {
		t.Error("no pod may be created without its token secret")
	}

	bad := launchRequest(e.p, userKey, false, "alice", "tok")
	bad.ID = "Not_A_Pod_Name"
	if _, err := e.p.Launch(ctx, bad); err == nil {
		t.Error("invalid id accepted")
	}
	bad = launchRequest(e.p, userKey, false, "alice", "")
	if _, err := e.p.Launch(ctx, bad); err == nil {
		t.Error("launch without token accepted")
	}

	// A failing owner reference patch is not fatal (the orphan collector
	// takes care of the secret).
	e = newTestEnv(t, testConfig(t, ""))
	e.client.PrependReactor("patch", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("patch refused")
	})
	if _, err := e.p.Launch(ctx, launchRequest(e.p, userKey, false, "alice", "tok")); err != nil {
		t.Fatalf("Launch must survive a failed owner reference patch: %v", err)
	}
	if !strings.Contains(e.logs.String(), "owner of its token secret failed") {
		t.Error("the failed patch must be logged")
	}
}

func TestTerminate(t *testing.T) {
	e := newTestEnv(t, testConfig(t, ""))
	ctx := e.start(t)
	req := launchRequest(e.p, userKey, false, "alice", "tok")
	if _, err := e.p.Launch(ctx, req); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "pod in cache", func() bool { return e.p.cachedPod(req.ID) != nil })

	if err := e.p.Terminate(ctx, req.ID, "idle for 30m0s"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.client.CoreV1().Pods(testNamespace).Get(ctx, req.ID, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("pod still exists: %v", err)
	}
	if _, err := e.client.CoreV1().Secrets(testNamespace).Get(ctx, req.ID+"-token", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("secret still exists: %v", err)
	}
	var grace *int64
	for _, a := range e.client.Actions() {
		if d, ok := a.(k8stesting.DeleteActionImpl); ok && d.GetResource().Resource == "pods" {
			grace = d.DeleteOptions.GracePeriodSeconds
		}
	}
	if grace == nil || *grace != 30 {
		t.Errorf("delete grace period %v, want 30", grace)
	}
	events, err := e.client.CoreV1().Events(testNamespace).List(ctx, metav1.ListOptions{})
	if err != nil || len(events.Items) != 1 {
		t.Fatalf("events %v %v", events, err)
	}
	ev := events.Items[0]
	if ev.Reason != "Terminating" || ev.Type != corev1.EventTypeNormal || ev.InvolvedObject.Kind != "Pod" ||
		ev.InvolvedObject.Name != req.ID || ev.Source.Component != "quackmux" || !strings.Contains(ev.Message, "idle for 30m0s") {
		t.Errorf("event %+v", ev)
	}
	waitEvent(t, e.p, "Gone", func(i engine.Instance) bool { return i.ID == req.ID && i.Phase == engine.PhaseGone })

	if err := e.p.Terminate(ctx, req.ID, "again"); err != nil {
		t.Errorf("terminating a deleted engine: %v", err)
	}
	if err := e.p.Terminate(ctx, "quack-user-nobody-zzzzz", "unknown"); err != nil {
		t.Errorf("terminating an unknown engine: %v", err)
	}
}

func TestTerminateWithoutCache(t *testing.T) {
	// Terminate works before Run (or for pods the cache has not seen yet).
	e := newTestEnv(t, testConfig(t, ""))
	ctx := context.Background()
	req := launchRequest(e.p, userKey, false, "alice", "tok")
	if _, err := e.p.Launch(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Terminate(ctx, req.ID, "failed: startup timeout after 3m0s"); err != nil {
		t.Fatal(err)
	}
	events, _ := e.client.CoreV1().Events(testNamespace).List(ctx, metav1.ListOptions{})
	if len(events.Items) != 1 || events.Items[0].Type != corev1.EventTypeWarning {
		t.Errorf("a failure must be recorded as a warning event: %+v", events.Items)
	}
	if pods, _ := e.client.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{}); len(pods.Items) != 0 {
		t.Error("pod not deleted")
	}

	e.client.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("api down")
	})
	if err := e.p.Terminate(ctx, "quack-user-x-aaaaa", "x"); err == nil {
		t.Error("a failed pod deletion must be reported (the manager retries)")
	}
}

func TestClaim(t *testing.T) {
	cfg := testConfig(t, "engine: {maxLifetime: 2h, drainTimeout: 1h}\n")
	e := newTestEnv(t, cfg)
	now := time.Now()
	e.p.now = func() time.Time { return now }
	warmKey := engine.Key{ShareLevel: engine.ShareConnection, Profile: "default"}
	old, oldSecret := e.enginePod("quack-warm-default-ooooo", warmKey, true, "old-token")
	old.CreationTimestamp = metav1.NewTime(now.Add(-5 * time.Hour))
	e.add(t, old, oldSecret)
	ctx := e.start(t)

	req := launchRequest(e.p, warmKey, true, "", "warm-token")
	if !strings.HasPrefix(req.ID, "quack-warm-default-") {
		t.Fatalf("warm id %s", req.ID)
	}
	if _, err := e.p.Launch(ctx, req); err != nil {
		t.Fatal(err)
	}
	pods := e.client.CoreV1().Pods(testNamespace)
	pod, _ := pods.Get(ctx, req.ID, metav1.GetOptions{})
	if pod.Labels[LabelWarm] != "true" || pod.Spec.ActiveDeadlineSeconds != nil {
		t.Fatalf("warm pod: labels %v, deadline %v", pod.Labels, pod.Spec.ActiveDeadlineSeconds)
	}
	waitFor(t, "pod in cache", func() bool { return e.p.cachedPod(req.ID) != nil })

	claimKey := engine.Key{ShareLevel: engine.ShareConnection, Owner: "sess-42", Subdomain: "default", Profile: "default"}
	if err := e.p.Claim(ctx, req.ID, claimKey, "alice"); err != nil {
		t.Fatal(err)
	}
	pod, _ = pods.Get(ctx, req.ID, metav1.GetOptions{})
	if pod.Labels[LabelKeyHash] != claimKey.Hash() || pod.Labels[LabelShareLevel] != "connection" || pod.Labels[LabelWarm] != "false" ||
		pod.Annotations[AnnotationKey] != claimKey.String() || pod.Annotations[AnnotationUser] != "alice" {
		t.Errorf("claimed pod metadata %v %v", pod.Labels, pod.Annotations)
	}
	if d := pod.Spec.ActiveDeadlineSeconds; d == nil || *d != int64((2*time.Hour+time.Hour+10*time.Minute)/time.Second) {
		t.Errorf("deadline after claim: %v", d)
	}
	if pod.Labels[LabelManagedBy] != "quackmux" || pod.Annotations[AnnotationProfile] != "default" {
		t.Error("a claim must merge, not replace, the metadata")
	}
	claimed := waitEvent(t, e.p, "claimed instance", func(i engine.Instance) bool { return i.ID == req.ID && !i.Warm })
	if claimed.Key != claimKey || claimed.Token != "warm-token" || claimed.User != "alice" {
		t.Errorf("claimed instance %+v", claimed)
	}

	// A pod that waited in the pool for 5h gets 5h + drainTimeout + 10m.
	if err := e.p.Claim(ctx, old.Name, claimKey, "alice"); err != nil {
		t.Fatal(err)
	}
	pod, _ = pods.Get(ctx, old.Name, metav1.GetOptions{})
	if d := pod.Spec.ActiveDeadlineSeconds; d == nil || *d != int64((5*time.Hour+time.Hour+10*time.Minute)/time.Second) {
		t.Errorf("deadline of an old warm pod: %v", d)
	}

	if err := e.p.Claim(ctx, "quack-warm-default-missing", claimKey, "alice"); err == nil {
		t.Error("claiming a missing pod must fail")
	}
}

func TestInformerEventsAndList(t *testing.T) {
	e := newTestEnv(t, testConfig(t, ""))
	// Found at start-up: an engine of a previous gateway process, an engine
	// of another gateway instance and a pod with a broken key annotation.
	bobPod, bobSecret := e.enginePod("quack-user-bob-aaaaa", keyFor("bob"), false, "bob-token")
	bobPod.Status = readyStatus("10.0.0.5")
	foreign := bobPod.DeepCopy()
	foreign.Name = "quack-user-bob-other"
	foreign.Labels[LabelInstance] = "other"
	broken := bobPod.DeepCopy()
	broken.Name = "quack-user-bob-broken"
	broken.Annotations[AnnotationKey] = "garbage"
	e.add(t, bobPod, bobSecret, foreign, broken)
	ctx := e.start(t)

	// Before List nothing is worth reporting: the manager registers what
	// List returns.
	noEvent(t, e.p, 100*time.Millisecond, func(engine.Instance) bool { return true })

	list, err := e.p.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("List = %+v, want only bob's engine", list)
	}
	bob := list[0]
	if bob.ID != bobPod.Name || bob.Phase != engine.PhaseReady || bob.Endpoint != "http://10.0.0.5:9494" ||
		bob.Token != "bob-token" || bob.Key != keyFor("bob") || bob.User != "bob" || bob.SpecHash == "" {
		t.Errorf("listed instance %+v", bob)
	}
	if !strings.Contains(e.logs.String(), "invalid key annotation") {
		t.Error("the broken pod must be reported")
	}

	// A launched engine reports its progress.
	req := launchRequest(e.p, userKey, false, "alice", "alice-token")
	if _, err := e.p.Launch(ctx, req); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, e.p, "starting engine", func(i engine.Instance) bool {
		return i.ID == req.ID && i.Phase == engine.PhaseStarting && i.Token == "alice-token"
	})
	pods := e.client.CoreV1().Pods(testNamespace)
	pod, _ := pods.Get(ctx, req.ID, metav1.GetOptions{})
	pod.Status = readyStatus("10.0.0.6")
	if _, err := pods.UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	ready := waitEvent(t, e.p, "ready engine", func(i engine.Instance) bool { return i.ID == req.ID && i.Phase == engine.PhaseReady })
	if ready.Endpoint != "http://10.0.0.6:9494" || ready.Token != "alice-token" || ready.Key != userKey {
		t.Errorf("ready instance %+v", ready)
	}

	// An engine that appears later (created by another gateway process) is
	// reported with its token, read from its secret. Pods of other gateway
	// instances are ignored (the fake's watch does not filter by label, so
	// this exercises the handler's own check).
	foreign2 := foreign.DeepCopy()
	foreign2.Name = "quack-user-bob-other2"
	if _, err := pods.Create(ctx, foreign2, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	carolPod, carolSecret := e.enginePod("quack-user-carol-ccccc", keyFor("carol"), false, "carol-token")
	if _, err := e.client.CoreV1().Secrets(testNamespace).Create(ctx, carolSecret, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pods.Create(ctx, carolPod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, i := range collectUntil(t, e.p, "adopted engine", func(i engine.Instance) bool {
		return i.ID == carolPod.Name && i.Token == "carol-token"
	}) {
		if i.ID == foreign2.Name {
			t.Errorf("event for a pod of another gateway instance: %+v", i)
		}
	}

	// Deleted pods are reported as gone.
	if err := pods.Delete(ctx, req.ID, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	gone := waitEvent(t, e.p, "gone engine", func(i engine.Instance) bool { return i.ID == req.ID && i.Phase == engine.PhaseGone })
	if gone.Key != userKey || gone.Token != "" {
		t.Errorf("gone instance %+v", gone)
	}

	list, err = e.p.List(ctx)
	if err != nil || len(list) != 2 || list[0].ID != bobPod.Name || list[1].ID != carolPod.Name || list[1].Token != "carol-token" {
		t.Errorf("List = %+v %v", list, err)
	}
}

func TestListTokenProblems(t *testing.T) {
	e := newTestEnv(t, testConfig(t, ""))
	missing, _ := e.enginePod("quack-user-a-aaaaa", keyFor("a"), false, "x")
	wrong, _ := e.enginePod("quack-user-b-bbbbb", keyFor("b"), false, "x")
	_, zSecret := e.enginePod("quack-user-z-zzzzz", keyFor("z"), false, "z-token")
	wrong.Annotations[AnnotationTokenSecret] = zSecret.Name // another engine's secret
	empty, emptySecret := e.enginePod("quack-user-c-ccccc", keyFor("c"), false, "")
	unreadable, unreadableSecret := e.enginePod("quack-user-d-ddddd", keyFor("d"), false, "d-token")
	e.add(t, missing, wrong, zSecret, empty, emptySecret, unreadable, unreadableSecret)
	e.client.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("list refused")
	})
	e.client.PrependReactor("get", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.GetAction).GetName() == unreadableSecret.Name {
			return true, nil, errors.New("i/o timeout")
		}
		return false, nil, nil
	})
	ctx := e.start(t)
	list, err := e.p.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		missing.Name: "not found",
		wrong.Name:   "does not belong",
		empty.Name:   "has no",
	}
	if len(list) != len(want) {
		t.Fatalf("List = %+v", list)
	}
	for _, inst := range list {
		if inst.Phase != engine.PhaseFailed || inst.Token != "" || inst.Endpoint != "" || !strings.Contains(inst.Message, want[inst.ID]) {
			t.Errorf("%s: %+v, want PhaseFailed with %q", inst.ID, inst, want[inst.ID])
		}
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "listing engine token secrets failed") || !strings.Contains(logs, "skipping it for now") {
		t.Errorf("warnings missing:\n%s", logs)
	}
}

func TestReplayAfterList(t *testing.T) {
	e := newTestEnv(t, testConfig(t, ""))
	pod, secret := e.enginePod("quack-user-bob-aaaaa", keyFor("bob"), false, "bob-token")
	e.add(t, pod, secret)
	e.p.replayDelay = 20 * time.Millisecond
	ctx := e.start(t)
	if list, err := e.p.List(ctx); err != nil || len(list) != 1 {
		t.Fatalf("List = %+v %v", list, err)
	}
	// The state is sent again once the caller had time to register it.
	waitEvent(t, e.p, "replayed state", func(i engine.Instance) bool { return i.ID == pod.Name && i.Token == "bob-token" })

	// Listed engines that vanished in between are reported as gone.
	e.p.replay(ctx, map[string]struct{}{"quack-user-ghost-zzzzz": {}, pod.Name: {}})
	waitEvent(t, e.p, "ghost gone", func(i engine.Instance) bool {
		return i.ID == "quack-user-ghost-zzzzz" && i.Phase == engine.PhaseGone
	})
}

func TestCollectOrphans(t *testing.T) {
	e := newTestEnv(t, testConfig(t, ""))
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	mk := func(name, engineName, instance string, age time.Duration) *corev1.Secret {
		s := e.p.secret(engineName, "t", config.Profile{})
		s.Name = name
		s.Labels[LabelInstance] = instance
		s.CreationTimestamp = metav1.NewTime(now.Add(-age))
		return s
	}
	livePod, _ := e.enginePod("quack-user-live-aaaaa", userKey, false, "t")
	unlabeled := mk("unlabeled-token", "x", "default", time.Hour)
	delete(unlabeled.Labels, LabelEngine)
	e.add(t, livePod,
		mk("orphan-token", "gone-1", "default", 10*time.Minute),
		mk("young-token", "gone-2", "default", time.Minute),
		mk("live-token", livePod.Name, "default", time.Hour),
		mk("foreign-token", "gone-3", "other", time.Hour),
		mk("launching-token", "launching", "default", time.Hour),
		unlabeled,
	)
	e.p.now = func() time.Time { return now.Add(-time.Hour) }
	e.p.storeToken("stale", "t") // a launch whose pod vanished unseen
	e.p.now = func() time.Time { return now }
	e.p.storeToken("launching", "t") // launched just now, not in the cache yet

	e.p.collectOrphans(context.Background())

	list, err := e.client.CoreV1().Secrets(testNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range list.Items {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "foreign-token,launching-token,live-token,unlabeled-token,young-token" {
		t.Errorf("remaining secrets %s (only orphan-token is an old orphan of this instance)", got)
	}
	if _, ok := e.p.cachedToken("stale"); ok {
		t.Error("stale token entries must be pruned")
	}
	if _, ok := e.p.cachedToken("launching"); !ok {
		t.Error("fresh token entries must be kept")
	}
}

func TestEmitDropsWhenFull(t *testing.T) {
	e := newTestEnv(t, testConfig(t, ""))
	e.p.sendTimeout = 20 * time.Millisecond
	for len(e.p.events) < cap(e.p.events) {
		e.p.events <- engine.Instance{ID: "filler"}
	}
	start := time.Now()
	e.p.emit(context.Background(), engine.Instance{ID: "dropped"})
	if time.Since(start) < e.p.sendTimeout {
		t.Error("emit must wait for room before dropping an event")
	}
	if logs := e.logs.String(); !strings.Contains(logs, "dropping event") || !strings.Contains(logs, "dropped") {
		t.Errorf("dropped event not logged:\n%s", logs)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.p.sendTimeout = time.Hour
	done := make(chan struct{})
	go func() {
		e.p.emit(ctx, engine.Instance{ID: "cancelled"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("emit must give up when its context ends")
	}

	<-e.p.events
	e.p.emit(context.Background(), engine.Instance{ID: "kept"})
	var last engine.Instance
	for len(e.p.events) > 0 {
		last = <-e.p.events
	}
	if last.ID != "kept" {
		t.Errorf("last event %q", last.ID)
	}
}

func TestRunLifecycle(t *testing.T) {
	e := newTestEnv(t, testConfig(t, ""))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.p.Run(ctx) }()
	wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer wcancel()
	if err := e.p.WaitSynced(wctx); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Run(ctx); err == nil {
		t.Error("a second Run must fail")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	for range e.p.Events() {
		// Drain; the loop ends because Run closed the channel.
	}
	if err := e.p.WaitSynced(context.Background()); err != nil {
		t.Errorf("WaitSynced after a synced run: %v", err)
	}

	// The cache never syncs: WaitSynced honours its context, and reports a
	// Run that ended before the sync.
	e = newTestEnv(t, testConfig(t, ""))
	e.client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("no RBAC"))
	})
	tctx, tcancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer tcancel()
	if err := e.p.WaitSynced(tctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WaitSynced without Run: %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() { done <- e.p.Run(ctx) }()
	waitFor(t, "RBAC error log", func() bool { return strings.Contains(e.logs.String(), "not allowed to list/watch engine pods") })
	cancel()
	<-done
	if err := e.p.WaitSynced(context.Background()); err == nil || !strings.Contains(err.Error(), "before the engine pod cache synced") {
		t.Errorf("WaitSynced after an unsynced run: %v", err)
	}
	if _, err := e.p.List(context.Background()); !errors.Is(err, errNotSynced) {
		t.Errorf("List before sync: %v", err)
	}
}

func TestStripManagedFields(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}}}}
	out, err := stripManagedFields(pod)
	if err != nil || out.(*corev1.Pod).ManagedFields != nil || out.(*corev1.Pod).Name != "p" {
		t.Fatalf("stripManagedFields: %+v %v", out, err)
	}
	if out, err := stripManagedFields("not a pod"); err != nil || out != "not a pod" {
		t.Fatalf("other objects must pass unchanged: %v %v", out, err)
	}
}
