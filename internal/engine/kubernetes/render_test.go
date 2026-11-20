package kubernetes

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"

	"quackmux/internal/config"
	"quackmux/internal/engine"
)

const profileYAML = `profiles:
  default:
    image: example.com/engine:1
    imagePullPolicy: Always
    database: /data/lake.duckdb
    initSQL: "ATTACH 'lake.db' AS lake"
    settings: {threads: "4"}
    resources:
      requests: {cpu: "1", memory: 2Gi}
      limits: {memory: 4Gi}
    env:
      - {name: FOO, value: bar}
      - {name: HOME, value: /home/duck}
      - {name: QUACK_TOKEN, value: not-the-token}
    nodeSelector: {pool: engines}
    tolerations: [{key: dedicated, operator: Equal, value: engines, effect: NoSchedule}]
    serviceAccountName: engine-sa
    imagePullSecrets: [{name: regcred}]
    priorityClassName: high
    labels: {team: data, app.kubernetes.io/managed-by: someone-else}
    annotations: {note: hello}
`

func renderDefault(t *testing.T, cfg *config.Config, in podInput) *corev1.Pod {
	t.Helper()
	return newTestEnv(t, cfg).p.pod(in)
}

func TestRenderPod(t *testing.T) {
	cfg := testConfig(t, profileYAML)
	prof := cfg.Profiles["default"]
	pod := renderDefault(t, cfg, podInput{
		Name: "quack-user-alice-abcde", Key: userKey, User: "alice", ProfileName: "default", Profile: prof, SpecHash: "h1",
	})

	if pod.Name != "quack-user-alice-abcde" || pod.Namespace != testNamespace {
		t.Fatalf("name %s/%s", pod.Namespace, pod.Name)
	}
	wantLabels := map[string]string{
		LabelManagedBy: "quackmux", LabelName: "quack-engine", LabelComponent: "engine",
		LabelAppInstance: "default", LabelInstance: "default", LabelKeyHash: userKey.Hash(),
		LabelShareLevel: "user", LabelProfile: "default", LabelWarm: "false", "team": "data",
	}
	if !reflect.DeepEqual(pod.Labels, wantLabels) {
		t.Errorf("labels\n got %v\nwant %v", pod.Labels, wantLabels)
	}
	initSum := sha256.Sum256([]byte(engine.RenderInitSQL(prof)))
	wantAnn := map[string]string{
		AnnotationKey: userKey.String(), AnnotationUser: "alice", AnnotationProfile: "default",
		AnnotationSpecHash: "h1", AnnotationTokenSecret: "quack-user-alice-abcde-token", "note": "hello",
		annotationDefaultContainer: ContainerName, AnnotationInitSQLHash: hex.EncodeToString(initSum[:]),
	}
	if !reflect.DeepEqual(pod.Annotations, wantAnn) {
		t.Errorf("annotations\n got %v\nwant %v", pod.Annotations, wantAnn)
	}

	s := pod.Spec
	if s.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy %s", s.RestartPolicy)
	}
	if s.AutomountServiceAccountToken == nil || *s.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken must default to false")
	}
	if s.EnableServiceLinks == nil || *s.EnableServiceLinks {
		t.Error("enableServiceLinks must default to false")
	}
	if s.TerminationGracePeriodSeconds == nil || *s.TerminationGracePeriodSeconds != 30 {
		t.Errorf("terminationGracePeriodSeconds %v", s.TerminationGracePeriodSeconds)
	}
	if s.ActiveDeadlineSeconds != nil {
		t.Errorf("activeDeadlineSeconds %d without maxLifetime", *s.ActiveDeadlineSeconds)
	}
	if !reflect.DeepEqual(s.NodeSelector, map[string]string{"pool": "engines"}) || len(s.Tolerations) != 3 ||
		s.ServiceAccountName != "engine-sa" || s.PriorityClassName != "high" ||
		!reflect.DeepEqual(s.ImagePullSecrets, []corev1.LocalObjectReference{{Name: "regcred"}}) {
		t.Errorf("profile scheduling settings not applied: %+v", s)
	}
	psc := s.SecurityContext
	if psc == nil || psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot || *psc.RunAsUser != 1000 || *psc.RunAsGroup != 1000 ||
		*psc.FSGroup != 1000 || psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security context %+v", psc)
	}

	if len(s.Containers) != 1 {
		t.Fatalf("%d containers", len(s.Containers))
	}
	c := s.Containers[0]
	if c.Name != ContainerName || c.Image != "example.com/engine:1" || c.ImagePullPolicy != corev1.PullAlways {
		t.Errorf("container %s image %s policy %s", c.Name, c.Image, c.ImagePullPolicy)
	}
	if c.Command != nil || c.Args != nil {
		t.Errorf("command/args must be left to the image: %v %v", c.Command, c.Args)
	}
	if !reflect.DeepEqual(c.Resources, prof.Resources) {
		t.Errorf("resources %+v", c.Resources)
	}
	if !reflect.DeepEqual(c.Ports, []corev1.ContainerPort{{Name: "quack", ContainerPort: 9494, Protocol: corev1.ProtocolTCP}}) {
		t.Errorf("ports %+v", c.Ports)
	}
	checkProbe(t, "readiness", c.ReadinessProbe, 2, 2, 3, 0)
	checkProbe(t, "liveness", c.LivenessProbe, 20, 5, 6, 180)
	csc := c.SecurityContext
	if csc == nil || csc.AllowPrivilegeEscalation == nil || *csc.AllowPrivilegeEscalation ||
		csc.ReadOnlyRootFilesystem == nil || !*csc.ReadOnlyRootFilesystem ||
		csc.Capabilities == nil || !reflect.DeepEqual(csc.Capabilities.Drop, []corev1.Capability{"ALL"}) {
		t.Errorf("container security context %+v", csc)
	}

	wantEnv := map[string]string{
		engine.EnvDatabase:   "/data/lake.duckdb",
		engine.EnvPort:       "9494",
		engine.EnvEngineID:   "quack-user-alice-abcde",
		engine.EnvShareLevel: "USER",
		engine.EnvOwner:      "alice",
		engine.EnvSubdomain:  "default",
		engine.EnvProfile:    "default",
		"HOME":               "/tmp",
		"FOO":                "bar",
	}
	for name, want := range wantEnv {
		got, n := envVar(&c, name)
		if n != 1 || got.Value != want || got.ValueFrom != nil {
			t.Errorf("env %s = %q (%d times), want %q", name, got.Value, n, want)
		}
	}
	// The init SQL comes from the engine's secret, never as a literal.
	if got, n := envVar(&c, engine.EnvInitSQL); n != 1 || got.Value != "" || got.ValueFrom == nil || got.ValueFrom.SecretKeyRef == nil ||
		got.ValueFrom.SecretKeyRef.Name != "quack-user-alice-abcde-token" || got.ValueFrom.SecretKeyRef.Key != initSQLDataKey {
		t.Errorf("env %s = %+v (%d times), want a reference to the engine secret", engine.EnvInitSQL, got, n)
	}
	if sql := engine.RenderInitSQL(prof); !strings.Contains(sql, "SET threads = '4';") || !strings.Contains(sql, "ATTACH 'lake.db' AS lake;") {
		t.Errorf("init SQL %q", sql)
	}
	tok, n := envVar(&c, engine.EnvToken)
	if n != 1 || tok.Value != "" || tok.ValueFrom == nil || tok.ValueFrom.SecretKeyRef == nil ||
		tok.ValueFrom.SecretKeyRef.Name != "quack-user-alice-abcde-token" || tok.ValueFrom.SecretKeyRef.Key != "token" {
		t.Errorf("QUACK_TOKEN %+v (%d times)", tok, n)
	}
	if len(c.Env) != len(wantEnv)+2 || c.Env[0].Name != engine.EnvToken || c.Env[len(c.Env)-1].Name != "FOO" {
		t.Errorf("env order: contract variables first, then the profile's: %v", c.Env)
	}

	if len(s.Volumes) != 1 || s.Volumes[0].Name != "tmp" || s.Volumes[0].EmptyDir == nil {
		t.Errorf("volumes %+v", s.Volumes)
	}
	if !reflect.DeepEqual(c.VolumeMounts, []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}}) {
		t.Errorf("mounts %+v", c.VolumeMounts)
	}
}

func checkProbe(t *testing.T, what string, p *corev1.Probe, period, timeout, failures, delay int32) {
	t.Helper()
	if p == nil || p.HTTPGet == nil {
		t.Fatalf("%s probe %+v", what, p)
	}
	if p.HTTPGet.Path != "/" || p.HTTPGet.Port != intstr.FromString("quack") || p.HTTPGet.Scheme != corev1.URISchemeHTTP {
		t.Errorf("%s probe target %+v", what, p.HTTPGet)
	}
	if p.PeriodSeconds != period || p.TimeoutSeconds != timeout || p.FailureThreshold != failures ||
		p.SuccessThreshold != 1 || p.InitialDelaySeconds != delay {
		t.Errorf("%s probe timings %+v", what, p)
	}
}

const templateYAML = `profiles:
  default:
    image: example.com/engine:2
    env: [{name: FROM_PROFILE, value: p}, {name: SHARED, value: profile}]
    nodeSelector: {zone: b}
    tolerations: [{key: a, operator: Exists}]
    imagePullSecrets: [{name: regcred}, {name: other}]
    affinity:
      nodeAffinity:
        requiredDuringSchedulingIgnoredDuringExecution:
          nodeSelectorTerms: [{matchExpressions: [{key: disk, operator: In, values: [ssd]}]}]
    labels: {from-profile: "yes"}
    podTemplate:
      metadata:
        labels: {from-template: "yes", from-profile: "no", quackmux.io/warm: "maybe"}
        annotations: {kubectl.kubernetes.io/default-container: sidecar, quackmux.io/key: forged}
      spec:
        automountServiceAccountToken: true
        enableServiceLinks: true
        restartPolicy: Always
        terminationGracePeriodSeconds: 5
        nodeSelector: {zone: a, disk: ssd}
        tolerations: [{key: a, operator: Exists}, {key: t, operator: Exists}]
        imagePullSecrets: [{name: regcred}]
        affinity:
          podAntiAffinity:
            preferredDuringSchedulingIgnoredDuringExecution:
              - {weight: 1, podAffinityTerm: {topologyKey: kubernetes.io/hostname}}
        securityContext: {runAsUser: 2000, fsGroup: 3000}
        volumes:
          - {name: tmp, configMap: {name: cm}}
          - {name: data, persistentVolumeClaim: {claimName: pvc}}
        initContainers: [{name: init, image: busybox}]
        containers:
          - name: sidecar
            image: proxy:1
          - name: engine
            image: template/ignored:1
            command: [/custom-entrypoint]
            env:
              - {name: SHARED, value: template}
              - {name: FROM_TEMPLATE, value: t}
              - {name: QUACK_TOKEN, value: leaked}
            resources: {limits: {memory: 1Gi}}
            readinessProbe: {tcpSocket: {port: 9494}, periodSeconds: 7}
            ports: [{name: metrics, containerPort: 9100}, {name: http, containerPort: 9494}]
            volumeMounts: [{name: data, mountPath: /data}]
            securityContext: {readOnlyRootFilesystem: false, capabilities: {add: [NET_BIND_SERVICE]}}
`

func TestRenderPodTemplateMerge(t *testing.T) {
	cfg := testConfig(t, templateYAML)
	prof := cfg.Profiles["default"]
	pod := renderDefault(t, cfg, podInput{Name: "quack-user-alice-abcde", Key: userKey, User: "alice", ProfileName: "default", Profile: prof})

	if pod.Labels["from-template"] != "yes" || pod.Labels["from-profile"] != "yes" || pod.Labels[LabelWarm] != "false" {
		t.Errorf("labels: template < profile < gateway: %v", pod.Labels)
	}
	if pod.Annotations[annotationDefaultContainer] != "sidecar" || pod.Annotations[AnnotationKey] != userKey.String() {
		t.Errorf("annotations %v", pod.Annotations)
	}
	s := pod.Spec
	if !*s.AutomountServiceAccountToken || !*s.EnableServiceLinks {
		t.Error("explicit template settings for automount/service links must be kept")
	}
	if s.RestartPolicy != corev1.RestartPolicyNever || *s.TerminationGracePeriodSeconds != 30 {
		t.Errorf("restart policy %s, grace %d: the gateway's values win", s.RestartPolicy, *s.TerminationGracePeriodSeconds)
	}
	if !reflect.DeepEqual(s.NodeSelector, map[string]string{"zone": "b", "disk": "ssd"}) {
		t.Errorf("nodeSelector %v", s.NodeSelector)
	}
	if len(s.Tolerations) != 4 || s.Tolerations[0].Key != "a" || s.Tolerations[1].Key != "t" ||
		s.Tolerations[2].Key != "node.kubernetes.io/not-ready" || s.Tolerations[3].Key != "node.kubernetes.io/unreachable" {
		t.Errorf("tolerations %+v", s.Tolerations)
	}
	if !reflect.DeepEqual(s.ImagePullSecrets, []corev1.LocalObjectReference{{Name: "regcred"}, {Name: "other"}}) {
		t.Errorf("pull secrets %+v", s.ImagePullSecrets)
	}
	if s.Affinity == nil || s.Affinity.NodeAffinity == nil || s.Affinity.PodAntiAffinity == nil {
		t.Errorf("affinity %+v", s.Affinity)
	}
	if sc := s.SecurityContext; *sc.RunAsUser != 2000 || *sc.FSGroup != 3000 || *sc.RunAsGroup != 1000 || !*sc.RunAsNonRoot {
		t.Errorf("pod security context %+v", sc)
	}
	if len(s.InitContainers) != 1 || s.InitContainers[0].SecurityContext != nil {
		t.Errorf("init containers must be untouched: %+v", s.InitContainers)
	}
	if len(s.Containers) != 2 || s.Containers[0].Name != "sidecar" || s.Containers[0].SecurityContext != nil || s.Containers[0].Env != nil {
		t.Fatalf("sidecar must be untouched: %+v", s.Containers)
	}

	c := container(pod, ContainerName)
	if c.Image != "example.com/engine:2" || !reflect.DeepEqual(c.Command, []string{"/custom-entrypoint"}) {
		t.Errorf("image %s (profile wins), command %v (template kept)", c.Image, c.Command)
	}
	if !c.Resources.Limits.Memory().Equal(resource.MustParse("1Gi")) || c.Resources.Requests != nil {
		t.Errorf("template resources must be kept: %+v", c.Resources)
	}
	var names []string
	for _, e := range c.Env {
		names = append(names, e.Name+"="+e.Value)
	}
	got := strings.Join(names, ",")
	if !strings.HasSuffix(got, ",SHARED=profile,FROM_TEMPLATE=t,FROM_PROFILE=p") || strings.Contains(got, "leaked") {
		t.Errorf("env %s", got)
	}
	if tok, n := envVar(c, engine.EnvToken); n != 1 || tok.ValueFrom == nil {
		t.Errorf("QUACK_TOKEN %+v (%d)", tok, n)
	}
	if c.ReadinessProbe.TCPSocket == nil || c.ReadinessProbe.PeriodSeconds != 7 {
		t.Errorf("template readiness probe must be kept: %+v", c.ReadinessProbe)
	}
	checkProbe(t, "liveness", c.LivenessProbe, 20, 5, 6, 180)
	if len(c.Ports) != 2 || c.Ports[0].Name != "quack" || c.Ports[1].Name != "metrics" {
		t.Errorf("ports %+v", c.Ports)
	}
	if len(s.Volumes) != 3 || s.Volumes[0].ConfigMap == nil || s.Volumes[2].Name != "quackmux-tmp" || s.Volumes[2].EmptyDir == nil {
		t.Errorf("volumes %+v", s.Volumes)
	}
	if !reflect.DeepEqual(c.VolumeMounts, []corev1.VolumeMount{{Name: "data", MountPath: "/data"}, {Name: "quackmux-tmp", MountPath: "/tmp"}}) {
		t.Errorf("mounts %+v", c.VolumeMounts)
	}
	sc := c.SecurityContext
	if *sc.ReadOnlyRootFilesystem || !reflect.DeepEqual(sc.Capabilities.Add, []corev1.Capability{"NET_BIND_SERVICE"}) ||
		!reflect.DeepEqual(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) || *sc.AllowPrivilegeEscalation {
		t.Errorf("container security context %+v", sc)
	}

	// The configuration must not have been modified.
	tc := prof.PodTemplate.Spec.Containers[1]
	if tc.Image != "template/ignored:1" || len(tc.Env) != 3 || len(prof.PodTemplate.Spec.Volumes) != 2 ||
		tc.SecurityContext.Capabilities.Drop != nil || prof.PodTemplate.Spec.RestartPolicy != corev1.RestartPolicyAlways {
		t.Errorf("rendering modified the profile's pod template: %+v", prof.PodTemplate.Spec)
	}
}

func TestRenderPodTemplateMountsTmp(t *testing.T) {
	cfg := testConfig(t, `profiles:
  default:
    podTemplate:
      spec:
        volumes: [{name: scratch, emptyDir: {medium: Memory}}]
        containers:
          - name: engine
            volumeMounts: [{name: scratch, mountPath: /tmp/}]
            securityContext: {runAsUser: 0, privileged: true}
`)
	pod := renderDefault(t, cfg, podInput{Name: "quack-server-abcde", Key: userKey, ProfileName: "default", Profile: cfg.Profiles["default"]})
	if len(pod.Spec.Volumes) != 1 || len(pod.Spec.Containers[0].VolumeMounts) != 1 {
		t.Errorf("an existing /tmp mount must be kept as the only one: %+v", pod.Spec)
	}
	if pod.Spec.SecurityContext.RunAsNonRoot != nil {
		t.Error("runAsNonRoot must not be defaulted for a container that explicitly runs as root")
	}
	if pod.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation != nil {
		t.Error("allowPrivilegeEscalation=false is invalid for privileged containers")
	}
}

func TestRenderActiveDeadline(t *testing.T) {
	cfg := testConfig(t, `engine: {maxLifetime: 2h, drainTimeout: 1h}
profiles:
  default: {}
  short: {maxLifetime: 30m}
  forever: {maxLifetime: 0s}
  strict:
    podTemplate: {spec: {activeDeadlineSeconds: 100}}
`)
	e := newTestEnv(t, cfg)
	deadline := func(profile string, warm bool) *int64 {
		pod := e.p.pod(podInput{Name: "x", Key: userKey, Warm: warm, ProfileName: profile, Profile: cfg.Profiles[profile]})
		return pod.Spec.ActiveDeadlineSeconds
	}
	if d := deadline("default", false); d == nil || *d != int64((2*time.Hour+time.Hour+10*time.Minute)/time.Second) {
		t.Errorf("default: %v", d)
	}
	if d := deadline("short", false); d == nil || *d != int64((30*time.Minute+time.Hour+10*time.Minute)/time.Second) {
		t.Errorf("profile maxLifetime: %v", d)
	}
	if d := deadline("forever", false); d != nil {
		t.Errorf("maxLifetime 0 must not set a deadline: %d", *d)
	}
	if d := deadline("strict", false); d == nil || *d != 100 {
		t.Errorf("a stricter template deadline must be kept: %v", d)
	}
	if d := deadline("default", true); d != nil {
		t.Errorf("warm engines get their deadline when claimed: %d", *d)
	}
	// Claimed at an age beyond maxLifetime: the manager drains at once.
	if d := e.p.activeDeadline(cfg.Profiles["default"], 5*time.Hour); d != int64((5*time.Hour+time.Hour+10*time.Minute)/time.Second) {
		t.Errorf("deadline at age 5h: %d", d)
	}
	if d := e.p.activeDeadline(config.Profile{MaxLifetime: &config.Duration{Duration: 1000000 * time.Hour}}, 0); d != 1<<31-1 {
		t.Errorf("deadline must be clamped to int32: %d", d)
	}
}

func TestRenderSecret(t *testing.T) {
	e := newTestEnv(t, testConfig(t, ""))
	s := e.p.secret("quack-user-alice-abcde", "s3cret", "")
	if s.Name != "quack-user-alice-abcde-token" || s.Namespace != testNamespace || s.Type != corev1.SecretTypeOpaque ||
		string(s.Data["token"]) != "s3cret" || s.Immutable == nil || !*s.Immutable {
		t.Fatalf("secret %+v", s)
	}
	if s.Labels[LabelManagedBy] != "quackmux" || s.Labels[LabelInstance] != "default" || s.Labels[LabelEngine] != "quack-user-alice-abcde" {
		t.Errorf("secret labels %v", s.Labels)
	}
	if !e.p.selector.Matches(labels.Set(s.Labels)) {
		t.Error("the instance selector must match token secrets (orphan collection)")
	}
}

func TestSpecHash(t *testing.T) {
	cfg := testConfig(t, profileYAML)
	e := newTestEnv(t, cfg)
	base := cfg.Profiles["default"]
	h := e.p.SpecHash("default", base)
	if len(h) != 16 || h != e.p.SpecHash("default", base) {
		t.Fatalf("hash %q is not stable", h)
	}
	// Deterministic across gateway restarts (a fresh provider and config).
	cfg2 := testConfig(t, profileYAML)
	if h2 := newTestEnv(t, cfg2).p.SpecHash("default", cfg2.Profiles["default"]); h2 != h {
		t.Fatalf("hash differs across restarts: %s vs %s", h, h2)
	}
	if e.p.SpecHash("other", base) == h {
		t.Error("the profile name must be part of the hash")
	}

	d := func(v time.Duration) *config.Duration { x := config.D(v); return &x }
	changes := map[string]func(p *config.Profile){
		"image":            func(p *config.Profile) { p.Image = "example.com/engine:2" },
		"imagePullPolicy":  func(p *config.Profile) { p.ImagePullPolicy = corev1.PullNever },
		"database":         func(p *config.Profile) { p.Database = ":memory:" },
		"initSQL":          func(p *config.Profile) { p.InitSQL = "SELECT 1" },
		"settings":         func(p *config.Profile) { p.Settings = map[string]string{"threads": "8"} },
		"env":              func(p *config.Profile) { p.Env = append(p.Env, corev1.EnvVar{Name: "X", Value: "1"}) },
		"resources":        func(p *config.Profile) { p.Resources.Limits = corev1.ResourceList{"memory": resource.MustParse("8Gi")} },
		"nodeSelector":     func(p *config.Profile) { p.NodeSelector = map[string]string{"pool": "other"} },
		"tolerations":      func(p *config.Profile) { p.Tolerations = nil },
		"affinity":         func(p *config.Profile) { p.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{}} },
		"serviceAccount":   func(p *config.Profile) { p.ServiceAccountName = "other" },
		"imagePullSecrets": func(p *config.Profile) { p.ImagePullSecrets = nil },
		"priorityClass":    func(p *config.Profile) { p.PriorityClassName = "low" },
		"labels":           func(p *config.Profile) { p.Labels = map[string]string{"team": "other"} },
		"annotations":      func(p *config.Profile) { p.Annotations = nil },
		"maxLifetime":      func(p *config.Profile) { p.MaxLifetime = d(12 * time.Hour) },
		"startupTimeout":   func(p *config.Profile) { p.StartupTimeout = d(10 * time.Minute) },
		"podTemplate": func(p *config.Profile) {
			p.PodTemplate = &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "sidecar", Image: "x"}}}}
		},
	}
	seen := map[string]string{h: "base"}
	for name, change := range changes {
		p := deepCopyProfile(base)
		change(&p)
		got := e.p.SpecHash("default", p)
		if prev, dup := seen[got]; dup {
			t.Errorf("changing %s gives the same hash as %s", name, prev)
		}
		seen[got] = name
	}
	if !reflect.DeepEqual(base, cfg2.Profiles["default"]) {
		t.Fatal("SpecHash modified the profile")
	}

	// Fields that do not shape the pod never replace engines.
	for name, change := range map[string]func(p *config.Profile){
		"description":   func(p *config.Profile) { p.Description = "changed" },
		"pool":          func(p *config.Profile) { p.Pool.Size = 4 },
		"warmPool":      func(p *config.Profile) { p.WarmPool.Size = 2 },
		"allowedUsers":  func(p *config.Profile) { p.AllowedUsers = []string{"bob"} },
		"idleTimeout":   func(p *config.Profile) { p.IdleTimeout = d(time.Minute) },
		"allowedLevels": func(p *config.Profile) { p.AllowedShareLevels = []string{"USER"} },
	} {
		p := deepCopyProfile(base)
		change(&p)
		if got := e.p.SpecHash("default", p); got != h {
			t.Errorf("changing %s must not change the hash", name)
		}
	}
}

// deepCopyProfile copies the parts of a profile the tests mutate.
func deepCopyProfile(p config.Profile) config.Profile {
	out := p
	out.Env = append([]corev1.EnvVar(nil), p.Env...)
	out.Resources = *p.Resources.DeepCopy()
	out.Settings = mergeMaps(p.Settings)
	out.Labels = mergeMaps(p.Labels)
	out.Annotations = mergeMaps(p.Annotations)
	out.NodeSelector = mergeMaps(p.NodeSelector)
	return out
}

func TestNewRejectsInvalidProfiles(t *testing.T) {
	for name, yaml := range map[string]string{
		"label key":   "profiles: {default: {labels: {'bad key!': x}}}\n",
		"label value": "profiles: {default: {labels: {ok: 'not a valid value'}}}\n",
		"annotation":  "profiles: {default: {annotations: {'-bad-': x}}}\n",
		"template":    "profiles: {default: {podTemplate: {metadata: {labels: {'a/b/c': x}}}}}\n",
		"pull policy": "profiles: {default: {imagePullPolicy: Sometimes}}\n",
	} {
		cfg := testConfig(t, yaml)
		if _, err := New(cfg, fake.NewSimpleClientset(), nil); err == nil || !strings.Contains(err.Error(), "profiles") {
			t.Errorf("%s: New accepted an invalid profile (err %v)", name, err)
		}
	}
}
