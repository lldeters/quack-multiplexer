package kubernetes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	k8stesting "k8s.io/client-go/testing"

	"quackmux/internal/engine"
)

const persistentYAML = `
profiles:
  default: {}
  personal:
    allowedShareLevels: [USER, SERVER]
    allowedSubdomains: [default, etl]
    persistence:
      size: 5Gi
      storageClassName: fast
      labels: {team: data}
      annotations: {backup.example.com/enabled: "true"}
`

var personalKey = engine.Key{ShareLevel: engine.ShareUser, Owner: "alice", Subdomain: "default", Profile: "personal"}

func TestDataClaimName(t *testing.T) {
	long := personalKey
	long.Owner = strings.Repeat("Very.Long-User@Example.COM", 5)
	// The name must never change for a key: a new name is a new, empty
	// volume, and the data of every key would be orphaned. These values are
	// frozen (sha256 of "quackmux/data-claim\x00<instance>\x00<Key.String()>").
	for _, c := range []struct {
		instance string
		key      engine.Key
		want     string
	}{
		{"default", personalKey, "quackmux-data-user-alice-default-personal-3800bb77b5cb0374"},
		{"staging", personalKey, "quackmux-data-user-alice-default-personal-5a6ba2570d07b4af"},
		{"default", engine.Key{ShareLevel: engine.ShareServer, Subdomain: "default", Profile: "personal"},
			"quackmux-data-server-default-personal-b9740ee93a5c893e"},
		{"default", engine.Key{ShareLevel: engine.ShareGroup, Owner: "Data Science", Subdomain: "etl", Profile: "personal"},
			"quackmux-data-group-data-science-etl-personal-467a889d0ee0cb59"},
		{"default", long, "quackmux-data-user-very-long-user-example-comv-d46c5fa5f077a7f2"},
	} {
		if got := dataClaimName(c.instance, c.key); got != c.want {
			t.Errorf("dataClaimName(%q, %s) = %q, want %q", c.instance, c.key, got, c.want)
		}
	}
	server := engine.Key{ShareLevel: engine.ShareServer, Subdomain: "default", Profile: "personal"}
	keys := map[string]engine.Key{
		"alice": personalKey, "Alice": {ShareLevel: engine.ShareUser, Owner: "Alice", Subdomain: "default", Profile: "personal"},
		"subdomain": {ShareLevel: engine.ShareUser, Owner: "alice", Subdomain: "etl", Profile: "personal"},
		"profile":   {ShareLevel: engine.ShareUser, Owner: "alice", Subdomain: "default", Profile: "Personal"},
		"group":     {ShareLevel: engine.ShareGroup, Owner: "alice", Subdomain: "default", Profile: "personal"},
		"long":      long, "long2": {ShareLevel: engine.ShareUser, Owner: long.Owner + "x", Subdomain: "default", Profile: "personal"},
		"server": server, "unicode": {ShareLevel: engine.ShareUser, Owner: "jürgen/ü", Subdomain: "default", Profile: "personal"},
	}
	seen := map[string]string{}
	for what, k := range keys {
		for _, instance := range []string{"default", "staging"} {
			n := dataClaimName(instance, k)
			if errs := validation.IsDNS1123Label(n); len(errs) > 0 {
				t.Errorf("%s/%s: %q is not a DNS-1123 label: %v", what, instance, n, errs)
			}
			if n != dataClaimName(instance, k) {
				t.Errorf("%s: name is not deterministic", what)
			}
			if other, dup := seen[n]; dup {
				t.Errorf("%s/%s and %s share the claim name %q", what, instance, other, n)
			}
			seen[n] = what + "/" + instance
		}
	}
}

func TestPersistentPodRender(t *testing.T) {
	e := newTestEnv(t, testConfig(t, persistentYAML))
	prof := e.p.cfg.Profiles["personal"]
	pod := e.p.pod(podInput{Name: "quack-user-alice-abcde", Key: personalKey, User: "alice", ProfileName: "personal", Profile: prof})
	claim := dataClaimName("default", personalKey)
	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == dataVolumeName {
			vol = &pod.Spec.Volumes[i]
		}
	}
	if vol == nil || vol.PersistentVolumeClaim == nil || vol.PersistentVolumeClaim.ClaimName != claim || vol.PersistentVolumeClaim.ReadOnly {
		t.Fatalf("data volume %+v, want claim %s", vol, claim)
	}
	c := container(pod, ContainerName)
	mounted := false
	for _, m := range c.VolumeMounts {
		if m.Name == dataVolumeName && m.MountPath == "/data" && !m.ReadOnly {
			mounted = true
		}
	}
	if !mounted {
		t.Fatalf("engine container does not mount the data volume at /data: %+v", c.VolumeMounts)
	}
	if db, _ := envVar(c, engine.EnvDatabase); db.Value != "/data/engine.duckdb" {
		t.Errorf("%s = %q", engine.EnvDatabase, db.Value)
	}
	if p := pod.Spec.SecurityContext.FSGroupChangePolicy; p == nil || *p != corev1.FSGroupChangeOnRootMismatch {
		t.Errorf("fsGroupChangePolicy %v", p)
	}
	inst, err := instanceFromPod(pod)
	if err != nil || inst.Volume != claim {
		t.Errorf("instanceFromPod volume %q, %v", inst.Volume, err)
	}
	// Profiles without persistence are unchanged.
	plain := e.p.pod(podInput{Name: "quack-user-alice-fghij", Key: userKey, User: "alice", ProfileName: "default", Profile: e.p.cfg.Profiles["default"]})
	for _, v := range plain.Spec.Volumes {
		if v.Name == dataVolumeName {
			t.Error("a profile without persistence got a data volume")
		}
	}
	if plain.Spec.SecurityContext.FSGroupChangePolicy != nil {
		t.Error("a profile without persistence got an fsGroupChangePolicy")
	}
}

func TestPersistentSpecHash(t *testing.T) {
	base := testConfig(t, persistentYAML)
	e := newTestEnv(t, base)
	h := e.p.SpecHash("personal", base.Profiles["personal"])
	for what, change := range map[string]func(*testing.T) string{
		"size": func(t *testing.T) string {
			c := testConfig(t, strings.Replace(persistentYAML, "size: 5Gi", "size: 50Gi", 1))
			return newTestEnv(t, c).p.SpecHash("personal", c.Profiles["personal"])
		},
		"storageClassName": func(t *testing.T) string {
			c := testConfig(t, strings.Replace(persistentYAML, "storageClassName: fast", "storageClassName: slow", 1))
			return newTestEnv(t, c).p.SpecHash("personal", c.Profiles["personal"])
		},
	} {
		// Claim settings apply to new claims only; they never replace engines.
		if got := change(t); got != h {
			t.Errorf("changing %s changed the spec hash", what)
		}
	}
	c := testConfig(t, strings.Replace(persistentYAML, "size: 5Gi", "size: 5Gi\n      mountPath: /var/lib/duckdb", 1))
	if newTestEnv(t, c).p.SpecHash("personal", c.Profiles["personal"]) == h {
		t.Error("changing the mount path kept the spec hash")
	}
}

func TestValidatePersistentProfile(t *testing.T) {
	const head = `
profiles:
  default: {}
  p:
    allowedShareLevels: [USER]
    allowedSubdomains: [default]
`
	for what, tail := range map[string]string{
		"volume name": `    persistence: {size: 1Gi}
    podTemplate:
      spec:
        volumes: [{name: quackmux-data, emptyDir: {}}]
`,
		"mount path": `    persistence: {size: 1Gi}
    podTemplate:
      spec:
        containers: [{name: engine, volumeMounts: [{name: other, mountPath: /data}]}]
        volumes: [{name: other, emptyDir: {}}]
`,
		"data volume mounted by the template": `    persistence: {size: 1Gi}
    podTemplate:
      spec:
        containers: [{name: engine, volumeMounts: [{name: quackmux-data, mountPath: /data}]}]
`,
		"node name": `    persistence: {size: 1Gi}
    podTemplate:
      spec:
        nodeName: node-1
`,
		"storage class": `    persistence: {size: 1Gi, storageClassName: Not_Valid}
`,
		"claim label": `    persistence: {size: 1Gi, labels: {"bad label!": x}}
`,
	} {
		if err := Validate(testConfig(t, head+tail)); err == nil {
			t.Errorf("%s: accepted", what)
		} else {
			t.Logf("%s: %v", what, err)
		}
	}
	if err := Validate(testConfig(t, persistentYAML)); err != nil {
		t.Fatalf("valid profile rejected: %v", err)
	}
	// Sidecars may mount the data volume by name, e.g. for backups.
	sidecar := `    persistence: {size: 1Gi}
    podTemplate:
      spec:
        containers:
          - name: backup
            image: backup:1
            volumeMounts: [{name: quackmux-data, mountPath: /data, readOnly: true}]
`
	if err := Validate(testConfig(t, head+sidecar)); err != nil {
		t.Fatalf("sidecar with the data volume rejected: %v", err)
	}
}

// actionsOf lists "verb resource" of the recorded client actions.
func actionsOf(e *testEnv) []string {
	var out []string
	for _, a := range e.client.Actions() {
		out = append(out, a.GetVerb()+" "+a.GetResource().Resource)
	}
	return out
}

func TestLaunchCreatesClaimBeforePod(t *testing.T) {
	e := newTestEnv(t, testConfig(t, persistentYAML))
	ctx := context.Background()
	req := launchRequest(e.p, personalKey, false, "alice", "tok-123")
	if _, err := e.p.Launch(ctx, req); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(actionsOf(e), ", ")
	if !strings.HasPrefix(got, "get persistentvolumeclaims, create persistentvolumeclaims, list pods, create secrets, create pods") {
		t.Fatalf("actions: %s", got)
	}
	pvc, err := e.client.CoreV1().PersistentVolumeClaims(testNamespace).Get(ctx, dataClaimName("default", personalKey), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pvc.Labels[LabelComponent] != dataComponentValue || pvc.Labels[LabelInstance] != "default" ||
		pvc.Labels[LabelKeyHash] != personalKey.Hash() || pvc.Labels[LabelShareLevel] != "user" || pvc.Labels["team"] != "data" ||
		pvc.Annotations[AnnotationKey] != personalKey.String() || pvc.Annotations[AnnotationUser] != "alice" ||
		pvc.Annotations["backup.example.com/enabled"] != "true" || pvc.Annotations[AnnotationLastLaunch] == "" {
		t.Errorf("claim metadata %v %v", pvc.Labels, pvc.Annotations)
	}
	if len(pvc.OwnerReferences) != 0 {
		t.Error("the claim must outlive its engine (no owner reference)")
	}
	if m := pvc.Spec.AccessModes; len(m) != 1 || m[0] != corev1.ReadWriteOncePod || *pvc.Spec.StorageClassName != "fast" ||
		*pvc.Spec.VolumeMode != corev1.PersistentVolumeFilesystem {
		t.Errorf("claim spec %+v", pvc.Spec)
	}
	if q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; q.Cmp(resource.MustParse("5Gi")) != 0 {
		t.Errorf("storage request %s", q.String())
	}
	// Terminate deletes pod and secret, never the claim.
	if err := e.p.Terminate(ctx, req.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.client.CoreV1().PersistentVolumeClaims(testNamespace).Get(ctx, pvc.Name, metav1.GetOptions{}); err != nil {
		t.Errorf("claim gone after Terminate: %v", err)
	}
}

func TestLaunchReusesClaimOfTheKey(t *testing.T) {
	e := newTestEnv(t, testConfig(t, persistentYAML))
	ps := e.p.cfg.Profiles["personal"].Persistence
	old := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	existing := e.p.dataClaim(dataClaimName("default", personalKey), personalKey, "personal", "bob", ps, old)
	existing.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce} // created under an older config
	e.add(t, existing)
	ctx := context.Background()
	if _, err := e.p.Launch(ctx, launchRequest(e.p, personalKey, false, "alice", "tok-123")); err != nil {
		t.Fatal(err)
	}
	for _, a := range actionsOf(e) {
		if a == "create persistentvolumeclaims" {
			t.Fatal("an existing claim was created again")
		}
	}
	pvc, _ := e.client.CoreV1().PersistentVolumeClaims(testNamespace).Get(ctx, existing.Name, metav1.GetOptions{})
	if pvc.Annotations[AnnotationUser] != "bob" || pvc.Annotations[AnnotationLastLaunch] == old.Format(time.RFC3339) {
		t.Errorf("annotations after reuse: %v", pvc.Annotations)
	}
	if !strings.Contains(e.logs.String(), "keeps the access modes") {
		t.Error("no warning about the access mode of the existing claim")
	}
}

func TestLaunchRefusesUnusableClaims(t *testing.T) {
	for what, mutate := range map[string]func(*corev1.PersistentVolumeClaim){
		"other key": func(c *corev1.PersistentVolumeClaim) {
			c.Annotations[AnnotationKey] = "USER/mallory/default/personal#0"
		},
		"other instance": func(c *corev1.PersistentVolumeClaim) { c.Labels[LabelInstance] = "staging" },
		"not ours":       func(c *corev1.PersistentVolumeClaim) { delete(c.Labels, LabelManagedBy) },
		"deleting": func(c *corev1.PersistentVolumeClaim) {
			c.DeletionTimestamp = &metav1.Time{Time: time.Now()}
			c.Finalizers = []string{"kubernetes.io/pvc-protection"}
		},
		"lost": func(c *corev1.PersistentVolumeClaim) { c.Status.Phase = corev1.ClaimLost },
	} {
		t.Run(what, func(t *testing.T) {
			e := newTestEnv(t, testConfig(t, persistentYAML))
			c := e.p.dataClaim(dataClaimName("default", personalKey), personalKey, "personal", "alice", e.p.cfg.Profiles["personal"].Persistence, time.Now())
			mutate(c)
			e.add(t, c)
			if _, err := e.p.Launch(context.Background(), launchRequest(e.p, personalKey, false, "alice", "tok-123")); err == nil {
				t.Fatal("launch with an unusable claim succeeded")
			}
			for _, a := range actionsOf(e) {
				if a == "create pods" || a == "create secrets" {
					t.Fatalf("%s after a refused claim", a)
				}
			}
		})
	}
}

// A pod of the key that the registry does not know (one the initial List
// skipped because its token could not be read, or one another gateway
// process created) keeps a second pod of the key from being created, and it
// is replayed to the manager.
func TestLaunchRefusesSecondPodOfKey(t *testing.T) {
	e := newTestEnv(t, testConfig(t, persistentYAML))
	other, _ := e.enginePod("quack-user-alice-older", personalKey, false, "tok-old")
	e.add(t, other)
	ctx := context.Background()
	_, err := e.p.Launch(ctx, launchRequest(e.p, personalKey, false, "alice", "tok-123"))
	if !errors.Is(err, engine.ErrHandover) || !strings.Contains(err.Error(), other.Name) {
		t.Fatalf("launch next to another pod of the key: %v", err)
	}
	for _, a := range actionsOf(e) {
		if a == "create pods" || a == "create secrets" {
			t.Fatalf("%s next to another pod of the key", a)
		}
	}
	if len(e.p.replayCh) != 1 {
		t.Error("the pod of the key was not replayed to the manager")
	}
	// Pods of other keys do not matter.
	bob := personalKey
	bob.Owner = "bob"
	if _, err := e.p.Launch(ctx, launchRequest(e.p, bob, false, "bob", "tok-456")); err != nil {
		t.Fatalf("launch for another key: %v", err)
	}
}

func TestLaunchClaimErrors(t *testing.T) {
	e := newTestEnv(t, testConfig(t, persistentYAML))
	e.client.PrependReactor("create", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumeclaims"}, "x",
			errors.New("exceeded quota: storage, requested: requests.storage=5Gi"))
	})
	_, err := e.p.Launch(context.Background(), launchRequest(e.p, personalKey, false, "alice", "tok-123"))
	if err == nil || !strings.Contains(err.Error(), "exceeded quota") {
		t.Fatalf("launch error %v", err)
	}
	for _, a := range actionsOf(e) {
		if a == "create pods" || a == "create secrets" {
			t.Fatalf("%s after a failed claim", a)
		}
	}

	// A concurrent create (another gateway process) is fine if the claim is ours.
	e = newTestEnv(t, testConfig(t, persistentYAML))
	theirs := e.p.dataClaim(dataClaimName("default", personalKey), personalKey, "personal", "alice", e.p.cfg.Profiles["personal"].Persistence, time.Now())
	e.client.PrependReactor("create", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		if err := e.client.Tracker().Add(theirs); err != nil {
			t.Error(err)
		}
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "persistentvolumeclaims"}, theirs.Name)
	})
	if _, err := e.p.Launch(context.Background(), launchRequest(e.p, personalKey, false, "alice", "tok-123")); err != nil {
		t.Fatalf("launch after a concurrent create: %v", err)
	}
}

func TestVolumesAndDeleteVolume(t *testing.T) {
	e := newTestEnv(t, testConfig(t, persistentYAML))
	ps := e.p.cfg.Profiles["personal"].Persistence
	launched := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	ours := e.p.dataClaim(dataClaimName("default", personalKey), personalKey, "personal", "alice", ps, launched)
	ours.UID = "uid-1"
	ours.Status.Phase = corev1.ClaimBound
	ours.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("8Gi")}
	other := e.p.dataClaim("quackmux-data-other", personalKey, "personal", "alice", ps, launched)
	other.Labels[LabelInstance] = "staging"
	plain := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: testNamespace}}
	e.add(t, ours, other, plain)
	ctx := context.Background()
	vols, err := e.p.Volumes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(vols) != 1 {
		t.Fatalf("volumes %+v", vols)
	}
	v := vols[0]
	if v.Name != ours.Name || v.EngineKey != personalKey || v.Owner != "alice" || v.ShareLevel != "USER" || v.Subdomain != "default" ||
		v.Profile != "personal" || v.User != "alice" || v.Size != "8Gi" || v.StorageClass != "fast" || v.Phase != "Bound" ||
		!v.LastLaunch.Equal(launched) || len(v.AccessModes) != 1 || v.AccessModes[0] != "ReadWriteOncePod" {
		t.Errorf("volume %+v", v)
	}
	if err := e.p.DeleteVolume(ctx, other.Name); !errors.Is(err, engine.ErrVolumeNotFound) {
		t.Errorf("deleting another instance's claim: %v", err)
	}
	if err := e.p.DeleteVolume(ctx, "unrelated"); !errors.Is(err, engine.ErrVolumeNotFound) {
		t.Errorf("deleting an unrelated claim: %v", err)
	}
	if err := e.p.DeleteVolume(ctx, "missing"); !errors.Is(err, engine.ErrVolumeNotFound) {
		t.Errorf("deleting a missing claim: %v", err)
	}
	if err := e.p.DeleteVolume(ctx, ours.Name); err != nil {
		t.Fatal(err)
	}
	var del k8stesting.DeleteAction
	for _, a := range e.client.Actions() {
		if d, ok := a.(k8stesting.DeleteAction); ok && a.GetResource().Resource == "persistentvolumeclaims" {
			del = d
		}
	}
	if del == nil || del.GetName() != ours.Name || del.GetDeleteOptions().Preconditions == nil ||
		*del.GetDeleteOptions().Preconditions.UID != "uid-1" {
		t.Errorf("delete action %+v", del)
	}
}
