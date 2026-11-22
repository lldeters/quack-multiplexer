package kubernetes

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"quackmux/internal/config"
	"quackmux/internal/engine"
)

// Probes of the engine container. GET / on the Quack port answers with a
// banner (HTTP 200, no authentication) as soon as quack_serve listens.
const (
	readinessPeriodSeconds    = 2
	readinessTimeoutSeconds   = 2
	readinessFailureThreshold = 3
	livenessPeriodSeconds     = 20
	livenessTimeoutSeconds    = 5
	livenessFailureThreshold  = 6
)

const (
	// deadlineSlack is added to engine.maxLifetime + engine.drainTimeout for
	// activeDeadlineSeconds, the hard backstop for engines that outlive
	// every gateway that could drain them.
	deadlineSlack = 10 * time.Minute

	// engineUID is the default user, group and fsGroup of engine pods.
	engineUID = 1000

	// defaultGracePeriod applies when provider.kubernetes.deleteGracePeriod
	// is not configured.
	defaultGracePeriod = 30 * time.Second

	homeEnv = "HOME"
)

// The spec hash is computed for a fixed placeholder engine, so that only the
// profile (and the gateway configuration that shapes pods) influences it,
// never per-instance values such as the pod name, key, user or token.
const (
	placeholderName = "quackmux-spec-placeholder"
	placeholderUser = "placeholder"
)

var placeholderKey = engine.Key{
	ShareLevel: engine.ShareUser, Owner: "placeholder", Subdomain: "placeholder", Profile: "placeholder",
}

// renderer turns launch requests into pod and secret objects. It has no
// mutable state and never modifies the configuration it reads.
type renderer struct {
	cfg       *config.Config
	namespace string
	instance  string
	// grace is the termination grace period of engine pods.
	grace time.Duration
}

// podInput is everything that differs between the pods of one profile.
type podInput struct {
	Name        string
	Key         engine.Key
	Warm        bool
	User        string
	ProfileName string
	Profile     config.Profile
	// SpecHash is recorded in an annotation (omitted when empty).
	SpecHash string
}

// pod renders an engine pod: the profile's pod template (if any) with the
// profile's settings and everything the gateway needs merged on top.
//
// Precedence: the gateway's labels, annotations, contract environment
// variables, restart policy and termination grace period always win; the
// profile wins over the template for the fields it sets (image, pull policy,
// node selector, affinity parts, service account, priority class); probes,
// resources, security settings and the /tmp volume are defaults that a
// template may replace. Command and args are left to the image (its
// entrypoint implements the engine container contract, see
// engine.EnvToken).
func (r *renderer) pod(in podInput) *corev1.Pod {
	prof := in.Profile
	pod := &corev1.Pod{}
	var tmplLabels, tmplAnnotations map[string]string
	if prof.PodTemplate != nil {
		t := prof.PodTemplate.DeepCopy()
		tmplLabels, tmplAnnotations = t.Labels, t.Annotations
		pod.Spec = t.Spec
	}
	pod.Name = in.Name
	pod.Namespace = r.namespace
	pod.Labels = mergeMaps(tmplLabels, prof.Labels, r.podLabels(in))
	annotations := mergeMaps(tmplAnnotations, prof.Annotations)
	if _, ok := annotations[annotationDefaultContainer]; !ok {
		annotations = mergeMaps(annotations, map[string]string{annotationDefaultContainer: ContainerName})
	}
	pod.Annotations = mergeMaps(annotations, r.podAnnotations(in))

	spec := &pod.Spec
	r.shapeSpec(spec, in)
	c := &spec.Containers[engineContainer(spec)]
	r.shapeContainer(c, in)
	ensureTmpVolume(spec, c)
	if ps := prof.Persistence; ps != nil {
		addDataVolume(spec, c, dataClaimName(r.instance, in.Key), ps)
	}
	applyPodSecurityDefaults(spec)
	if prof.Persistence != nil && spec.SecurityContext.FSGroupChangePolicy == nil {
		// Adjust the ownership of the data volume only while its root does
		// not match fsGroup yet, not on every mount.
		spec.SecurityContext.FSGroupChangePolicy = ptrTo(corev1.FSGroupChangeOnRootMismatch)
	}
	applyContainerSecurityDefaults(c)
	return pod
}

// podLabels are the labels the gateway selects and groups engine pods by.
func (r *renderer) podLabels(in podInput) map[string]string {
	return map[string]string{
		LabelManagedBy:   managedByValue,
		LabelName:        nameValue,
		LabelComponent:   componentValue,
		LabelAppInstance: r.instance,
		LabelInstance:    r.instance,
		LabelKeyHash:     in.Key.Hash(),
		LabelShareLevel:  strings.ToLower(string(in.Key.ShareLevel)),
		LabelProfile:     labelValue(in.ProfileName),
		LabelWarm:        strconv.FormatBool(in.Warm),
	}
}

// podAnnotations carry what engine discovery needs after a gateway restart.
func (r *renderer) podAnnotations(in podInput) map[string]string {
	a := map[string]string{
		AnnotationKey:         in.Key.String(),
		AnnotationUser:        in.User,
		AnnotationProfile:     in.ProfileName,
		AnnotationTokenSecret: tokenSecretName(in.Name),
	}
	if in.SpecHash != "" {
		a[AnnotationSpecHash] = in.SpecHash
	}
	if sql := engine.RenderInitSQL(in.Profile); sql != "" {
		sum := sha256.Sum256([]byte(sql))
		a[AnnotationInitSQLHash] = hex.EncodeToString(sum[:])
	}
	return a
}

// shapeSpec applies the pod-level settings.
func (r *renderer) shapeSpec(spec *corev1.PodSpec, in podInput) {
	prof := in.Profile
	// The in-memory state of a crashed engine is lost anyway; restarting it
	// in place would only pretend that its sessions survived.
	spec.RestartPolicy = corev1.RestartPolicyNever
	if spec.AutomountServiceAccountToken == nil {
		spec.AutomountServiceAccountToken = ptrTo(false)
	}
	// Service links would inject variables such as QUACKMUX_PORT for a
	// Service named "quackmux" (the gateway's own).
	if spec.EnableServiceLinks == nil {
		spec.EnableServiceLinks = ptrTo(false)
	}
	spec.TerminationGracePeriodSeconds = ptrTo(ceilSeconds(r.grace))
	spec.NodeSelector = mergeMaps(spec.NodeSelector, prof.NodeSelector)
	spec.Tolerations = addNodeLossTolerations(appendTolerations(spec.Tolerations, prof.Tolerations))
	spec.Affinity = mergeAffinity(spec.Affinity, prof.Affinity)
	if prof.ServiceAccountName != "" {
		spec.ServiceAccountName = prof.ServiceAccountName
		spec.DeprecatedServiceAccount = ""
	}
	spec.ImagePullSecrets = appendPullSecrets(spec.ImagePullSecrets, prof.ImagePullSecrets)
	if prof.PriorityClassName != "" {
		spec.PriorityClassName = prof.PriorityClassName
		// The priority admission plugin derives the value from the class
		// and rejects a pod that carries a different one.
		spec.Priority = nil
	}
	// Warm-pool engines get their deadline when a session claims them
	// (Provider.Claim): they may wait in the pool for a long time first.
	if !in.Warm {
		setActiveDeadline(spec, r.activeDeadline(prof, 0))
	}
}

// setActiveDeadline lowers spec.activeDeadlineSeconds to d (0 = none).
func setActiveDeadline(spec *corev1.PodSpec, d int64) {
	if d > 0 && (spec.ActiveDeadlineSeconds == nil || *spec.ActiveDeadlineSeconds > d) {
		spec.ActiveDeadlineSeconds = ptrTo(d)
	}
}

// maxLifetime resolves engine.maxLifetime for a profile (like
// config.EngineMaxLifetime, but for the profile at hand).
func (r *renderer) maxLifetime(prof config.Profile) time.Duration {
	if prof.MaxLifetime != nil {
		return prof.MaxLifetime.Duration
	}
	return r.cfg.Engine.MaxLifetime.Duration
}

// startupTimeout resolves engine.startupTimeout for a profile.
func (r *renderer) startupTimeout(prof config.Profile) time.Duration {
	if prof.StartupTimeout != nil {
		return prof.StartupTimeout.Duration
	}
	return r.cfg.Engine.StartupTimeout.Duration
}

// activeDeadline returns activeDeadlineSeconds for an engine of the profile
// that starts serving sessions at the given age (0 for regular engines, the
// age at claim time for warm-pool engines), or 0 without a max lifetime.
// The manager drains engines once they are maxLifetime old and closes their
// sessions drainTimeout later; the deadline only matters when no gateway is
// around to do that.
func (r *renderer) activeDeadline(prof config.Profile, age time.Duration) int64 {
	life := r.maxLifetime(prof)
	if life <= 0 {
		return 0
	}
	life = max(life, age)
	// Summed in seconds: absurdly long durations must not overflow.
	d := ceilSeconds(life) + ceilSeconds(r.cfg.Engine.DrainTimeout.Duration) + ceilSeconds(deadlineSlack)
	return min(max(d, 1), math.MaxInt32)
}

// engineContainer returns the index of the container named "engine",
// appending one if the template has none.
func engineContainer(spec *corev1.PodSpec) int {
	for i := range spec.Containers {
		if spec.Containers[i].Name == ContainerName {
			return i
		}
	}
	spec.Containers = append(spec.Containers, corev1.Container{Name: ContainerName})
	return len(spec.Containers) - 1
}

// shapeContainer fills the engine container.
func (r *renderer) shapeContainer(c *corev1.Container, in podInput) {
	prof := in.Profile
	switch {
	case prof.Image != "":
		c.Image = prof.Image
	case c.Image == "":
		c.Image = config.DefaultEngineImage
	}
	switch {
	case prof.ImagePullPolicy != "":
		c.ImagePullPolicy = prof.ImagePullPolicy
	case c.ImagePullPolicy == "":
		c.ImagePullPolicy = corev1.PullIfNotPresent
	}
	// The pod's restartPolicy Never must hold for the engine: a
	// container-level policy or restart rules would restart it in place, a
	// fresh DuckDB without the state its sessions rely on.
	c.RestartPolicy = nil
	c.RestartPolicyRules = nil
	if !hasResources(c.Resources) {
		c.Resources = *prof.Resources.DeepCopy()
	}
	c.Env = r.env(c.Env, in)
	c.Ports = withQuackPort(c.Ports)
	if c.ReadinessProbe == nil {
		c.ReadinessProbe = httpProbe(readinessPeriodSeconds, readinessTimeoutSeconds, readinessFailureThreshold)
	}
	if c.LivenessProbe == nil {
		c.LivenessProbe = httpProbe(livenessPeriodSeconds, livenessTimeoutSeconds, livenessFailureThreshold)
		// The init SQL may take a while (ATTACH, loading data). Liveness
		// checks start once the startup budget is used up; until then the
		// manager's startup timeout is in charge.
		c.LivenessProbe.InitialDelaySeconds = int32(min(ceilSeconds(r.startupTimeout(prof)), math.MaxInt32))
	}
}

func hasResources(rr corev1.ResourceRequirements) bool {
	return len(rr.Limits) > 0 || len(rr.Requests) > 0 || len(rr.Claims) > 0
}

// contractEnvNames are set by the gateway and cannot be overridden.
var contractEnvNames = map[string]bool{
	engine.EnvToken: true, engine.EnvDatabase: true, engine.EnvInitSQL: true, engine.EnvPort: true,
	engine.EnvEngineID: true, engine.EnvShareLevel: true, engine.EnvOwner: true, engine.EnvSubdomain: true,
	engine.EnvProfile: true, engine.EnvStopTimeout: true, homeEnv: true,
}

// stopGraceReserve is the part of the pod's grace period left to the
// engine's entrypoint after its stop timeout (to kill DuckDB and exit).
const stopGraceReserve = 5 * time.Second

// stopTimeoutSeconds returns QUACKMUX_STOP_TIMEOUT for a grace period: the
// period less stopGraceReserve, at least one second.
func stopTimeoutSeconds(grace time.Duration) int64 {
	return max(1, ceilSeconds(grace-stopGraceReserve))
}

// env returns the environment of the engine container: the engine
// container contract (see engine.EnvToken) followed by the template's and
// the profile's variables (the profile wins over the template). Contract
// variables come first, so that user variables can reference them as
// $(QUACKMUX_ENGINE_ID); user variables with a contract name are dropped.
func (r *renderer) env(base []corev1.EnvVar, in podInput) []corev1.EnvVar {
	database := in.Profile.Database
	if database == "" {
		database = ":memory:"
	}
	out := []corev1.EnvVar{
		{Name: engine.EnvToken, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: tokenSecretName(in.Name)},
			Key:                  tokenDataKey,
		}}},
		// Literal values are escaped: the kubelet expands $(VAR) and $$ in
		// them, and the owner is a user or group name (a user called
		// "$(QUACK_TOKEN)" would get the token into the engine's log).
		literalEnv(engine.EnvDatabase, database),
		literalEnv(engine.EnvPort, strconv.Itoa(engine.EnginePort)),
		literalEnv(engine.EnvEngineID, in.Name),
		literalEnv(engine.EnvShareLevel, string(in.Key.ShareLevel)),
		literalEnv(engine.EnvOwner, in.Key.Owner),
		literalEnv(engine.EnvSubdomain, in.Key.Subdomain),
		literalEnv(engine.EnvProfile, in.ProfileName),
		// The engine must stop DuckDB (checkpoint included) before the
		// kubelet kills the container at the end of the grace period.
		literalEnv(engine.EnvStopTimeout, strconv.FormatInt(stopTimeoutSeconds(r.grace), 10)),
		// The root file system is read-only; DuckDB keeps ~/.duckdb in HOME.
		literalEnv(homeEnv, tmpMountPath),
	}
	if engine.RenderInitSQL(in.Profile) != "" {
		// From the engine's secret, not as a literal: the init SQL may hold
		// credentials. (Values from secrets are not expanded either.)
		out = append(out, corev1.EnvVar{Name: engine.EnvInitSQL, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: tokenSecretName(in.Name)},
			Key:                  initSQLDataKey,
		}}})
	}
	for _, e := range mergeEnv(base, in.Profile.Env) {
		if !contractEnvNames[e.Name] {
			out = append(out, e)
		}
	}
	return out
}

// literalEnv is an environment variable with a literal value, escaped
// against the kubelet's $(VAR) expansion.
func literalEnv(name, value string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, Value: strings.ReplaceAll(value, "$", "$$")}
}

// mergeEnv returns base with overrides applied: an override replaces the
// variable of the same name in place, new names are appended. The result
// shares no memory with the inputs.
func mergeEnv(base, overrides []corev1.EnvVar) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(base)+len(overrides))
	index := make(map[string]int, len(base)+len(overrides))
	add := func(e corev1.EnvVar) {
		e = *e.DeepCopy()
		if i, ok := index[e.Name]; ok {
			out[i] = e
			return
		}
		index[e.Name] = len(out)
		out = append(out, e)
	}
	for _, e := range base {
		add(e)
	}
	for _, e := range overrides {
		add(e)
	}
	return out
}

// withQuackPort puts the Quack port first and drops template ports that
// would clash with it.
func withQuackPort(ports []corev1.ContainerPort) []corev1.ContainerPort {
	out := []corev1.ContainerPort{{Name: PortName, ContainerPort: engine.EnginePort, Protocol: corev1.ProtocolTCP}}
	for _, p := range ports {
		tcp := p.Protocol == "" || p.Protocol == corev1.ProtocolTCP
		if p.Name == PortName || (p.ContainerPort == engine.EnginePort && tcp) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// httpProbe probes GET / on the Quack port.
func httpProbe(period, timeout, failures int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path:   "/",
			Port:   intstr.FromString(PortName),
			Scheme: corev1.URISchemeHTTP,
		}},
		PeriodSeconds:    period,
		TimeoutSeconds:   timeout,
		FailureThreshold: failures,
		SuccessThreshold: 1,
	}
}

// ensureTmpVolume mounts an emptyDir at /tmp (the root file system is
// read-only) unless the engine container already mounts something there.
func ensureTmpVolume(spec *corev1.PodSpec, c *corev1.Container) {
	for _, m := range c.VolumeMounts {
		if path.Clean(m.MountPath) == tmpMountPath {
			return
		}
	}
	name := tmpVolumeName
	for i := 1; hasVolume(spec.Volumes, name); i++ {
		name = tmpVolumeAltName
		if i > 1 {
			name += "-" + strconv.Itoa(i)
		}
	}
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name:         name,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: name, MountPath: tmpMountPath})
}

func hasVolume(volumes []corev1.Volume, name string) bool {
	for _, v := range volumes {
		if v.Name == name {
			return true
		}
	}
	return false
}

// applyPodSecurityDefaults hardens the pod where the template leaves the
// security context unset. runAsNonRoot is not defaulted when a template
// explicitly runs a container as root (that combination never starts).
func applyPodSecurityDefaults(spec *corev1.PodSpec) {
	sc := spec.SecurityContext
	if sc == nil {
		sc = &corev1.PodSecurityContext{}
		spec.SecurityContext = sc
	}
	root := (sc.RunAsUser != nil && *sc.RunAsUser == 0) || anyContainerRunsAsRoot(spec)
	if sc.RunAsUser == nil {
		sc.RunAsUser = ptrTo(int64(engineUID))
	}
	if sc.RunAsGroup == nil {
		sc.RunAsGroup = ptrTo(int64(engineUID))
	}
	if sc.FSGroup == nil {
		sc.FSGroup = ptrTo(int64(engineUID))
	}
	if sc.RunAsNonRoot == nil && !root {
		sc.RunAsNonRoot = ptrTo(true)
	}
	if sc.SeccompProfile == nil {
		sc.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	}
}

func anyContainerRunsAsRoot(spec *corev1.PodSpec) bool {
	for _, list := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
		for _, c := range list {
			if c.SecurityContext != nil && c.SecurityContext.RunAsUser != nil && *c.SecurityContext.RunAsUser == 0 {
				return true
			}
		}
	}
	return false
}

// applyContainerSecurityDefaults hardens the engine container where the
// template leaves its security context unset.
func applyContainerSecurityDefaults(c *corev1.Container) {
	sc := c.SecurityContext
	if sc == nil {
		sc = &corev1.SecurityContext{}
		c.SecurityContext = sc
	}
	if sc.AllowPrivilegeEscalation == nil && !needsPrivilegeEscalation(sc) {
		sc.AllowPrivilegeEscalation = ptrTo(false)
	}
	if sc.ReadOnlyRootFilesystem == nil {
		sc.ReadOnlyRootFilesystem = ptrTo(true)
	}
	if sc.Capabilities == nil {
		sc.Capabilities = &corev1.Capabilities{}
	}
	if len(sc.Capabilities.Drop) == 0 {
		sc.Capabilities.Drop = []corev1.Capability{"ALL"}
	}
}

// needsPrivilegeEscalation reports settings that the API server rejects in
// combination with allowPrivilegeEscalation: false.
func needsPrivilegeEscalation(sc *corev1.SecurityContext) bool {
	if sc.Privileged != nil && *sc.Privileged {
		return true
	}
	if sc.Capabilities != nil {
		for _, c := range sc.Capabilities.Add {
			if c == "SYS_ADMIN" || c == "CAP_SYS_ADMIN" {
				return true
			}
		}
	}
	return false
}

// mergeMaps merges maps into a new one; later maps win. It returns nil when
// all maps are empty.
func mergeMaps(maps ...map[string]string) map[string]string {
	n := 0
	for _, m := range maps {
		n += len(m)
	}
	if n == 0 {
		return nil
	}
	out := make(map[string]string, n)
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// nodeLossTolerations replace the 300s that admission adds by default: an
// engine on a node that is not ready or unreachable is evicted after a
// minute, so that the gateway stops sending its sessions to a dead address
// (its in-memory state is lost with the node anyway). A template or profile
// toleration for the same key wins.
var nodeLossTolerations = []corev1.Toleration{
	{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptrTo(int64(60))},
	{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptrTo(int64(60))},
}

// addNodeLossTolerations adds nodeLossTolerations for the keys ts does not
// tolerate yet.
func addNodeLossTolerations(ts []corev1.Toleration) []corev1.Toleration {
	out := ts
next:
	for _, t := range nodeLossTolerations {
		for _, have := range ts {
			if have.Key == t.Key {
				continue next
			}
		}
		out = append(out, *t.DeepCopy())
	}
	return out
}

// appendTolerations adds the tolerations of extra that base lacks.
func appendTolerations(base, extra []corev1.Toleration) []corev1.Toleration {
	out := base
next:
	for _, t := range extra {
		for _, have := range out {
			if reflect.DeepEqual(have, t) {
				continue next
			}
		}
		out = append(out, *t.DeepCopy())
	}
	return out
}

// appendPullSecrets adds the pull secrets of extra that base lacks.
func appendPullSecrets(base, extra []corev1.LocalObjectReference) []corev1.LocalObjectReference {
	out := base
next:
	for _, s := range extra {
		for _, have := range out {
			if have.Name == s.Name {
				continue next
			}
		}
		out = append(out, s)
	}
	return out
}

// mergeAffinity replaces the node affinity, pod affinity and pod
// anti-affinity of the template with the profile's where the profile sets
// them.
func mergeAffinity(base, prof *corev1.Affinity) *corev1.Affinity {
	if prof == nil {
		return base
	}
	p := prof.DeepCopy()
	out := base
	if out == nil {
		out = &corev1.Affinity{}
	}
	if p.NodeAffinity != nil {
		out.NodeAffinity = p.NodeAffinity
	}
	if p.PodAffinity != nil {
		out.PodAffinity = p.PodAffinity
	}
	if p.PodAntiAffinity != nil {
		out.PodAntiAffinity = p.PodAntiAffinity
	}
	return out
}

// secret renders the token secret of an engine pod. It is immutable: the
// token of an engine never changes.
func (r *renderer) secret(podName, token, initSQL string) *corev1.Secret {
	data := map[string][]byte{tokenDataKey: []byte(token)}
	if initSQL != "" {
		data[initSQLDataKey] = []byte(initSQL)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tokenSecretName(podName),
			Namespace: r.namespace,
			Labels: map[string]string{
				LabelManagedBy:   managedByValue,
				LabelName:        nameValue,
				LabelComponent:   tokenComponentValue,
				LabelAppInstance: r.instance,
				LabelInstance:    r.instance,
				LabelEngine:      podName,
			},
		},
		Type:      corev1.SecretTypeOpaque,
		Immutable: ptrTo(true),
		Data:      data,
	}
}

// specHash implements Provider.SpecHash: a digest of the pod rendered for a
// placeholder engine of the profile (labels, annotations and spec) and of
// the profile name. Everything that shapes the pod changes it: every
// pod-related profile field, the startup timeout (liveness delay), the max
// lifetime (activeDeadlineSeconds) and the gateway settings
// provider.kubernetes.deleteGracePeriod and engine.drainTimeout. Profile
// fields that do not shape the pod (description, pool and warm pool sizes,
// access control, idle timeout) leave it alone, so that changing them never
// replaces running engines.
func (r *renderer) specHash(profileName string, prof config.Profile) string {
	pod := r.pod(podInput{
		Name: placeholderName, Key: placeholderKey, User: placeholderUser,
		ProfileName: profileName, Profile: prof,
	})
	return engine.HashJSON(struct {
		Profile     string            `json:"profile"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		Spec        corev1.PodSpec    `json:"spec"`
	}{profileName, pod.Labels, pod.Annotations, pod.Spec})
}

// validateProfiles renders every profile once and checks what the API
// server would reject anyway, so that a broken profile fails at start-up
// instead of at every launch.
func (r *renderer) validateProfiles() error {
	names := make([]string, 0, len(r.cfg.Profiles))
	for name := range r.cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	var errs []error
	for _, name := range names {
		prof := r.cfg.Profiles[name]
		pod := r.pod(podInput{
			Name: placeholderName, Key: placeholderKey, User: placeholderUser,
			ProfileName: name, Profile: prof,
		})
		base := field.NewPath("profiles").Key(name)
		if el := metav1validation.ValidateLabels(pod.Labels, base.Child("labels")); len(el) > 0 {
			errs = append(errs, el.ToAggregate())
		}
		if el := apivalidation.ValidateAnnotations(pod.Annotations, base.Child("annotations")); len(el) > 0 {
			errs = append(errs, el.ToAggregate())
		}
		switch policy := pod.Spec.Containers[engineContainer(&pod.Spec)].ImagePullPolicy; policy {
		case corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever:
		default:
			errs = append(errs, fmt.Errorf("profiles.%s.imagePullPolicy: %q is not Always, IfNotPresent or Never", name, policy))
		}
		if prof.Persistence != nil {
			if err := r.validatePersistence(name, prof, pod); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("kubernetes provider: invalid profile: %w", err)
	}
	return nil
}

// ceilSeconds converts d to whole seconds, rounding up; negative durations
// give 0.
func ceilSeconds(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	s := int64(d / time.Second)
	if d%time.Second != 0 {
		s++
	}
	return s
}

func ptrTo[T any](v T) *T { return &v }
