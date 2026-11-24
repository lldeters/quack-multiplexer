// Package kubernetes is the engine provider that runs every engine as a
// Kubernetes pod (docs/design.md, section 7).
//
// An engine is a pod with a container named "engine" (image from the
// profile, see render.go) and a token secret "<pod>-token". Launch creates
// the secret first, then the pod, and finally makes the pod the owner of
// the secret so that Kubernetes garbage-collects the secret with the pod;
// secrets left behind by failures are deleted by a periodic collector. A pod
// informer, restricted to the labels of this gateway instance, translates
// pod changes into engine events. Labels and annotations carry everything
// needed to rebuild the registry after a gateway restart: key, user,
// profile, spec hash and the name of the token secret.
//
// Profiles with persistence get a PersistentVolumeClaim per engine key,
// created before the pod and kept when the engine ends (volume.go).
//
// Required RBAC in the engine namespace: pods get/list/watch/create/delete/
// patch, secrets get/list/create/delete/patch, events create,
// persistentvolumeclaims get/list/create/patch/delete.
package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/validate/content"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/informers"
	kclient "k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"quackmux/internal/config"
	"quackmux/internal/engine"
)

const (
	// eventBufferSize is the capacity of the Events channel.
	eventBufferSize = 4096
	// defaultSendTimeout bounds how long an informer handler waits for room
	// in a full Events channel before it drops the event.
	defaultSendTimeout = 5 * time.Second
	// defaultResync re-delivers every cached pod periodically, so that the
	// manager converges even if it missed an event.
	defaultResync = 5 * time.Minute
	// defaultReplayDelay: after List, the current state of all pods is sent
	// again once the caller had time to register the listed instances.
	defaultReplayDelay = 2 * time.Second
	// Orphaned token secrets are collected every gcInterval (first after
	// gcFirstDelay) once they are older than orphanMinAge.
	defaultGCInterval   = 5 * time.Minute
	defaultGCFirstDelay = time.Minute
	defaultOrphanMinAge = 5 * time.Minute

	// cleanupTimeout bounds clean-up calls made after the caller's context
	// may already be done.
	cleanupTimeout = 30 * time.Second
	// eventTimeout bounds recording a Kubernetes event (best effort).
	eventTimeout = 5 * time.Second
	// apiTimeout bounds single reads made from background work.
	apiTimeout = 10 * time.Second
	// maxIDAttempts bounds the search for an unused pod name.
	maxIDAttempts = 16
)

var (
	errNotSynced = errors.New("kubernetes provider: the engine pod cache is not synced yet")
	// errTokenMissing marks engines whose token secret is definitively gone
	// or unusable (as opposed to a failed read).
	errTokenMissing = errors.New("engine token secret missing")
)

// Provider implements engine.Provider with Kubernetes pods. It is safe for
// concurrent use.
type Provider struct {
	renderer
	client   kclient.Interface
	log      *slog.Logger
	prefix   string
	selector labels.Selector
	// identity names this gateway process in recorded events.
	identity string

	events  chan engine.Instance
	synced  chan struct{} // closed once the pod cache is synced
	stopped chan struct{} // closed when Run returns
	started atomic.Bool
	// listed is set by the first List. Before that, events of pods whose
	// token is unknown are not worth an API call: List reads all tokens and
	// then replays the current state of every pod.
	listed atomic.Bool
	// eventWarned is set once a failure to record an event was logged.
	eventWarned atomic.Bool

	mu     sync.RWMutex
	lister corelisters.PodLister
	tokens map[string]tokenEntry // pod name -> token
	warned map[string]bool       // pod name + message, to warn only once

	// emitMu orders replayed states before the Gone event of a pod that is
	// deleted concurrently (see replay).
	emitMu sync.Mutex

	// closeMu guards the events channel against senders outside the
	// informer (Terminate), which may run after Run has closed it.
	closeMu      sync.RWMutex
	eventsClosed bool

	replayMu  sync.Mutex
	replayIDs map[string]struct{}
	replayCh  chan struct{}

	// Tunables, replaced by tests.
	now          func() time.Time
	newSuffix    func() string
	sendTimeout  time.Duration
	resync       time.Duration
	replayDelay  time.Duration
	gcInterval   time.Duration
	gcFirstDelay time.Duration
	orphanMinAge time.Duration
}

type tokenEntry struct {
	token string
	since time.Time
}

var _ engine.Provider = (*Provider)(nil)

// New creates the provider. It validates the configuration but does not
// contact the API server. The namespace is provider.kubernetes.namespace,
// else $POD_NAMESPACE, else the namespace of the service account the
// gateway runs as, else "default".
func New(cfg *config.Config, client kclient.Interface, log *slog.Logger) (*Provider, error) {
	if cfg == nil {
		return nil, errors.New("kubernetes provider: configuration is required")
	}
	if client == nil {
		return nil, errors.New("kubernetes provider: a Kubernetes client is required")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ns := resolveNamespace(cfg.Provider.Kubernetes.Namespace, os.Getenv, os.ReadFile)
	r, err := newRenderer(cfg, ns)
	if err != nil {
		return nil, err
	}
	instance := r.instance
	p := &Provider{
		renderer:     r,
		client:       client,
		log:          log.With("component", "kubernetes", "namespace", ns, "instance", instance),
		prefix:       namePrefix(cfg.Provider.Kubernetes.NamePrefix),
		selector:     instanceSelector(instance),
		identity:     processIdentity(),
		events:       make(chan engine.Instance, eventBufferSize),
		synced:       make(chan struct{}),
		stopped:      make(chan struct{}),
		tokens:       map[string]tokenEntry{},
		warned:       map[string]bool{},
		replayIDs:    map[string]struct{}{},
		replayCh:     make(chan struct{}, 1),
		now:          time.Now,
		newSuffix:    randomSuffix,
		sendTimeout:  defaultSendTimeout,
		resync:       defaultResync,
		replayDelay:  defaultReplayDelay,
		gcInterval:   defaultGCInterval,
		gcFirstDelay: defaultGCFirstDelay,
		orphanMinAge: defaultOrphanMinAge,
	}
	return p, nil
}

// Validate runs the checks of New that need no cluster (for "quackmux
// validate"): the instance, the grace period and every profile rendered
// once. The namespace is checked only when the configuration names one.
func Validate(cfg *config.Config) error {
	ns := strings.TrimSpace(cfg.Provider.Kubernetes.Namespace)
	if ns == "" {
		ns = "default"
	}
	_, err := newRenderer(cfg, ns)
	return err
}

// newRenderer validates the provider settings and the profiles and returns
// the renderer for them.
func newRenderer(cfg *config.Config, ns string) (renderer, error) {
	kc := cfg.Provider.Kubernetes
	if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
		return renderer{}, fmt.Errorf("kubernetes provider: invalid namespace %q: %s", ns, strings.Join(errs, "; "))
	}
	instance := strings.TrimSpace(kc.Instance)
	if instance == "" {
		instance = "default"
	}
	if errs := content.IsLabelValue(instance); len(errs) > 0 {
		return renderer{}, fmt.Errorf("kubernetes provider: provider.kubernetes.instance %q is not a valid label value: %s",
			instance, strings.Join(errs, "; "))
	}
	grace := defaultGracePeriod
	if kc.DeleteGracePeriod.IsSet() {
		grace = kc.DeleteGracePeriod.Duration
	}
	if grace < time.Second {
		// A grace period of 0 force-deletes the pod while DuckDB still runs.
		return renderer{}, fmt.Errorf("kubernetes provider: provider.kubernetes.deleteGracePeriod must be at least 1s, got %s", grace)
	}
	r := renderer{cfg: cfg, namespace: ns, instance: instance, grace: grace}
	if err := r.validateProfiles(); err != nil {
		return renderer{}, err
	}
	return r, nil
}

// processIdentity names the gateway process in events: $POD_NAME, else the
// host name.
func processIdentity() string {
	id := strings.TrimSpace(os.Getenv("POD_NAME"))
	if id == "" {
		id, _ = os.Hostname()
	}
	if len(id) > 128 {
		id = id[:128]
	}
	return id
}

// Namespace returns the namespace engine pods run in.
func (p *Provider) Namespace() string { return p.namespace }

// Name implements engine.Provider.
func (p *Provider) Name() string { return "kubernetes" }

// CanLaunch implements engine.Provider.
func (p *Provider) CanLaunch() bool { return true }

// Events implements engine.Provider.
func (p *Provider) Events() <-chan engine.Instance { return p.events }

// SpecHash implements engine.Provider. It hashes the pod rendered for a
// placeholder engine of the profile, so it is stable across restarts and
// independent of per-engine values.
func (p *Provider) SpecHash(profileName string, profile config.Profile) string {
	return p.specHash(profileName, profile)
}

// NewID implements engine.Provider: a DNS-1123 label such as
// quack-user-alice-x7k2p (quack-connection-conn-x7k2p for CONNECTION,
// quack-warm-<profile>-x7k2p for warm-pool engines). Names of existing
// engines are never returned.
func (p *Provider) NewID(key engine.Key, warm bool) string {
	parts := append([]string{p.prefix}, nameParts(key, warm)...)
	name := buildName(p.newSuffix(), parts...)
	for i := 1; i < maxIDAttempts && p.nameTaken(name); i++ {
		name = buildName(p.newSuffix(), parts...)
	}
	return name
}

// nameTaken reports whether a pod of that name exists or was just launched.
func (p *Provider) nameTaken(name string) bool {
	if _, ok := p.cachedToken(name); ok {
		return true
	}
	return p.cachedPod(name) != nil
}

// Run implements engine.Provider: it runs the pod informer and the orphan
// collector until ctx is cancelled, then closes the Events channel. It must
// be called only once.
func (p *Provider) Run(ctx context.Context) error {
	if !p.started.CompareAndSwap(false, true) {
		return errors.New("kubernetes provider: Run must be called only once")
	}
	var (
		wg      sync.WaitGroup
		factory informers.SharedInformerFactory
	)
	defer func() {
		// Shutdown waits until no event handler runs any more, so nobody
		// sends on the channel once it is closed.
		if factory != nil {
			factory.Shutdown()
		}
		wg.Wait()
		close(p.stopped)
		p.closeMu.Lock()
		p.eventsClosed = true
		close(p.events)
		p.closeMu.Unlock()
	}()

	selector := p.selector.String()
	factory = informers.NewSharedInformerFactoryWithOptions(p.client, p.resync,
		informers.WithNamespace(p.namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = selector }),
		informers.WithTransform(stripManagedFields),
	)
	pods := factory.Core().V1().Pods()
	inf := pods.Informer()
	if err := inf.SetWatchErrorHandlerWithContext(p.watchError); err != nil {
		return fmt.Errorf("kubernetes provider: %w", err)
	}
	if _, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { p.onPod(ctx, obj) },
		UpdateFunc: func(_, obj any) { p.onPod(ctx, obj) },
		DeleteFunc: func(obj any) { p.onPodDeleted(ctx, obj) },
	}); err != nil {
		return fmt.Errorf("kubernetes provider: %w", err)
	}
	p.mu.Lock()
	p.lister = pods.Lister()
	p.mu.Unlock()

	// client-go logs through the context. Its informational messages are
	// chatty, so they go one level below Info (errors stay errors).
	ictx := klog.NewContext(ctx, logr.FromSlogHandler(p.log.Handler()).WithName("informer").V(1))
	p.log.Info("watching engine pods", "selector", selector)
	factory.StartWithContext(ictx)
	if res := factory.WaitForCacheSyncWithContext(ictx); res.Err != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fmt.Errorf("kubernetes provider: engine pod cache sync: %w", res.Err)
	}
	close(p.synced)
	p.log.Info("engine pod cache synced", "pods", len(inf.GetStore().ListKeys()))

	wg.Go(func() { p.gcLoop(ctx) })
	wg.Go(func() { p.replayLoop(ctx) })
	<-ctx.Done()
	return ctx.Err()
}

// WaitSynced implements engine.Provider.
func (p *Provider) WaitSynced(ctx context.Context) error {
	if isClosed(p.synced) {
		return nil
	}
	select {
	case <-p.synced:
		return nil
	case <-p.stopped:
		if isClosed(p.synced) {
			return nil
		}
		return errors.New("kubernetes provider stopped before the engine pod cache synced")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// List implements engine.Provider: all cached engine pods of this gateway
// instance, with the tokens read from their secrets. Pods whose token cannot
// be read right now are skipped (with a warning; the periodic resync retries
// them). Pods whose token secret is definitively gone are returned as
// PhaseFailed without a token, so that the manager deletes them after
// engine.failedRetention instead of leaving them running unseen.
func (p *Provider) List(ctx context.Context) ([]engine.Instance, error) {
	lister := p.podLister()
	if lister == nil || !isClosed(p.synced) {
		return nil, errNotSynced
	}
	pods, err := lister.Pods(p.namespace).List(p.selector)
	if err != nil {
		return nil, fmt.Errorf("kubernetes provider: list engine pods: %w", err)
	}
	sort.Slice(pods, func(i, j int) bool {
		a, b := pods[i].CreationTimestamp.Time, pods[j].CreationTimestamp.Time
		if !a.Equal(b) {
			return a.Before(b)
		}
		return pods[i].Name < pods[j].Name
	})
	var secrets map[string]*corev1.Secret
	out := make([]engine.Instance, 0, len(pods))
	ids := make([]string, 0, len(pods))
	for _, pod := range pods {
		if !p.owns(pod) {
			continue
		}
		if _, ok := p.cachedToken(pod.Name); !ok && secrets == nil {
			// One LIST instead of a GET per pod (a restart may find many).
			secrets = p.listTokenSecrets(ctx)
		}
		inst, ok := p.resolve(ctx, pod, secrets)
		if !ok {
			continue
		}
		out = append(out, inst)
		ids = append(ids, pod.Name)
	}
	p.listed.Store(true)
	p.requestReplay(ids)
	return out, nil
}

// Launch implements engine.Provider: it creates the token secret and the
// pod and returns the instance in PhaseStarting. For a profile with
// persistence it first makes sure that the data volume claim of the key
// exists and that no other pod of the key does (volume.go).
func (p *Provider) Launch(ctx context.Context, req engine.LaunchRequest) (engine.Instance, error) {
	if errs := validation.IsDNS1123Label(req.ID); len(errs) > 0 {
		return engine.Instance{}, fmt.Errorf("kubernetes provider: invalid engine id %q: %s", req.ID, strings.Join(errs, "; "))
	}
	if req.Token == "" {
		return engine.Instance{}, errors.New("kubernetes provider: launch request without token")
	}
	hash := req.SpecHash
	if hash == "" {
		hash = p.SpecHash(req.ProfileName, req.Profile)
	}
	pod := p.pod(podInput{
		Name: req.ID, Key: req.Key, Warm: req.Warm, User: req.User,
		ProfileName: req.ProfileName, Profile: req.Profile, SpecHash: hash,
	})
	secret := p.secret(req.ID, req.Token, req.Profile)
	secrets := p.client.CoreV1().Secrets(p.namespace)
	if req.Profile.Persistence != nil {
		// The claim before the pod, which would otherwise wait for it.
		if _, err := p.ensureDataClaim(ctx, req); err != nil {
			return engine.Instance{}, err
		}
		if err := p.checkSoleEngine(ctx, req); err != nil {
			return engine.Instance{}, err
		}
	}

	// Known before the pod exists, so that its first events carry the token.
	p.storeToken(req.ID, req.Token)
	if _, err := secrets.Create(ctx, secret, metav1.CreateOptions{FieldManager: eventComponent}); err != nil {
		p.forgetToken(req.ID)
		return engine.Instance{}, fmt.Errorf("create token secret %s: %w", secret.Name, err)
	}
	created, err := p.createPod(ctx, pod, req.Fence)
	if err != nil {
		p.forgetToken(req.ID)
		// The caller's context may be what failed; clean up regardless.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if derr := secrets.Delete(cctx, secret.Name, metav1.DeleteOptions{}); derr != nil && !apierrors.IsNotFound(derr) {
			p.log.Warn("deleting the token secret of a failed launch failed; the orphan collector will remove it",
				"engine", req.ID, "secret", secret.Name, "err", derr)
		}
		return engine.Instance{}, fmt.Errorf("create engine pod %s: %w", pod.Name, err)
	}
	if err := p.attachSecret(ctx, secret.Name, created); err != nil {
		// Not fatal: without the owner reference the orphan collector
		// deletes the secret once the pod is gone.
		p.log.Warn("making the engine pod the owner of its token secret failed", "engine", req.ID, "secret", secret.Name, "err", err)
	}
	p.log.Debug("engine pod created", "engine", created.Name, "key", req.Key.String(), "profile", req.ProfileName,
		"warm", req.Warm, "user", req.User)

	inst, err := instanceFromPod(created)
	if err != nil { // cannot happen: the annotations were rendered above
		inst = engine.Instance{ID: req.ID, Key: req.Key, Warm: req.Warm, ProfileName: req.ProfileName, SpecHash: hash, User: req.User}
	}
	inst.Token = req.Token
	inst.Phase, inst.Endpoint, inst.Message = engine.PhaseStarting, "", "pod created"
	if inst.CreatedAt.IsZero() {
		inst.CreatedAt = p.now()
	}
	return inst, nil
}

// createPod creates the engine pod. The fence of a launch (see
// engine.LaunchRequest.Fence) is checked as late as possible: the lease may
// have gone stale during the calls of Launch before, and while client-go's
// rate limiter holds the request back. The client of NewClient checks it
// again when the request is sent (fence.go).
func (p *Provider) createPod(ctx context.Context, pod *corev1.Pod, fence func() bool) (*corev1.Pod, error) {
	if fence != nil && !fence() {
		return nil, engine.ErrNotLeader
	}
	created, err := p.client.CoreV1().Pods(p.namespace).Create(withFence(ctx, fence), pod,
		metav1.CreateOptions{FieldManager: eventComponent})
	if errors.Is(err, engine.ErrNotLeader) {
		return nil, engine.ErrNotLeader // not sent: drop client-go's "Post <url>:" wrapping
	}
	return created, err
}

// attachSecret makes the pod the owner of its token secret, so that
// Kubernetes garbage-collects the secret together with the pod.
func (p *Provider) attachSecret(ctx context.Context, secretName string, pod *corev1.Pod) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"ownerReferences": []metav1.OwnerReference{{
		APIVersion:         "v1",
		Kind:               "Pod",
		Name:               pod.Name,
		UID:                pod.UID,
		BlockOwnerDeletion: ptrTo(false),
	}}}})
	if err != nil {
		return err
	}
	_, err = p.client.CoreV1().Secrets(p.namespace).Patch(ctx, secretName, types.MergePatchType, patch,
		metav1.PatchOptions{FieldManager: eventComponent})
	return err
}

// Claim implements engine.Provider: it re-keys a warm-pool pod for a
// session (labels key-hash, share-level, warm=false, the key and user
// annotations) and gives it its activeDeadlineSeconds backstop.
func (p *Provider) Claim(ctx context.Context, id string, key engine.Key, user string) error {
	patch := map[string]any{"metadata": map[string]any{
		"labels": map[string]string{
			LabelKeyHash:    key.Hash(),
			LabelShareLevel: strings.ToLower(string(key.ShareLevel)),
			LabelWarm:       "false",
		},
		"annotations": map[string]string{AnnotationKey: key.String(), AnnotationUser: user},
	}}
	if d := p.claimDeadline(id); d > 0 {
		patch["spec"] = map[string]any{"activeDeadlineSeconds": d}
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	if _, err := p.client.CoreV1().Pods(p.namespace).Patch(ctx, id, types.MergePatchType, data,
		metav1.PatchOptions{FieldManager: eventComponent}); err != nil {
		return fmt.Errorf("claim engine pod %s: %w", id, err)
	}
	p.log.Debug("claimed warm engine pod", "engine", id, "key", key.String())
	return nil
}

// claimDeadline returns the activeDeadlineSeconds a claimed warm-pool pod
// needs, or 0 when it has one already or its age is unknown.
func (p *Provider) claimDeadline(id string) int64 {
	pod := p.cachedPod(id)
	if pod == nil || pod.Spec.ActiveDeadlineSeconds != nil {
		return 0
	}
	name := pod.Annotations[AnnotationProfile]
	prof, ok := p.cfg.Profiles[name]
	if !ok {
		return 0
	}
	var age time.Duration
	if !pod.CreationTimestamp.IsZero() {
		age = max(p.now().Sub(pod.CreationTimestamp.Time), 0)
	}
	return p.activeDeadline(prof, age)
}

// Terminate implements engine.Provider: it records an event on the pod
// (best effort) and deletes the pod (with the configured grace period) and
// its token secret. Missing objects are not an error.
func (p *Provider) Terminate(ctx context.Context, id string, reason string) error {
	pods := p.client.CoreV1().Pods(p.namespace)
	pod := p.cachedPod(id)
	if pod == nil {
		// Typed clients return an empty object (not nil) on errors.
		gctx, cancel := context.WithTimeout(ctx, apiTimeout)
		if got, err := pods.Get(gctx, id, metav1.GetOptions{}); err == nil {
			pod = got
		}
		cancel()
	}
	if pod != nil && pod.DeletionTimestamp == nil {
		p.recordEvent(ctx, pod, reason)
	}
	grace := ceilSeconds(p.grace)
	err := pods.Delete(ctx, id, metav1.DeleteOptions{GracePeriodSeconds: &grace})
	gone := false
	switch {
	case err == nil:
		p.log.Debug("deleting engine pod", "engine", id, "reason", reason)
	case apierrors.IsNotFound(err):
		// No pod (any more), for example after a create that the API server
		// rejected. The informer reports nothing for a pod that never
		// existed, and the manager forgets an engine only on Gone.
		gone = true
	default:
		return fmt.Errorf("delete engine pod %s: %w", id, err)
	}
	// Always the canonical name: the annotation naming the secret could have
	// been edited to point at another engine's secret.
	secretName := tokenSecretName(id)
	if err := p.client.CoreV1().Secrets(p.namespace).Delete(ctx, secretName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		// The owner reference or the orphan collector removes it later.
		p.log.Warn("deleting the token secret of an engine failed", "engine", id, "secret", secretName, "err", err)
	}
	if gone {
		p.emitGone(ctx, id, "no engine pod")
	}
	return nil
}

// emitGone reports that an engine has no pod. A second Gone for the same pod
// (from the informer) is harmless.
func (p *Provider) emitGone(ctx context.Context, id, msg string) {
	p.closeMu.RLock()
	defer p.closeMu.RUnlock()
	if p.eventsClosed {
		return
	}
	p.emit(ctx, engine.Instance{ID: id, Phase: engine.PhaseGone, Message: msg})
}

// recordEvent records a Kubernetes event about the termination on the pod,
// so that "kubectl describe pod" tells why the gateway deleted it.
func (p *Provider) recordEvent(ctx context.Context, pod *corev1.Pod, reason string) {
	now := metav1.NewTime(p.now())
	typ := corev1.EventTypeNormal
	if l := strings.ToLower(reason); strings.Contains(l, "fail") || strings.Contains(l, "timeout") {
		typ = corev1.EventTypeWarning
	}
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s.%x", pod.Name, now.UnixNano()),
			Namespace: p.namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			APIVersion:      "v1",
			Kind:            "Pod",
			Namespace:       p.namespace,
			Name:            pod.Name,
			UID:             pod.UID,
			ResourceVersion: pod.ResourceVersion,
		},
		Reason:              "Terminating",
		Message:             truncate("quackmux terminates the engine: " + reason),
		Type:                typ,
		Source:              corev1.EventSource{Component: eventComponent},
		FirstTimestamp:      now,
		LastTimestamp:       now,
		Count:               1,
		ReportingController: eventComponent,
		ReportingInstance:   p.identity,
	}
	ectx, cancel := context.WithTimeout(ctx, eventTimeout)
	defer cancel()
	if _, err := p.client.CoreV1().Events(p.namespace).Create(ectx, ev, metav1.CreateOptions{}); err != nil {
		level := slog.LevelDebug
		if !p.eventWarned.Swap(true) {
			level = slog.LevelWarn // once: probably RBAC (events create)
		}
		p.log.Log(ctx, level, "recording a Kubernetes event failed", "engine", pod.Name, "err", err)
	}
}

// onPod handles informer adds and updates (including periodic resyncs).
func (p *Provider) onPod(ctx context.Context, obj any) {
	pod, ok := obj.(*corev1.Pod)
	if !ok || !p.owns(pod) {
		return
	}
	if _, ok := p.cachedToken(pod.Name); !ok && !p.listed.Load() {
		return
	}
	if inst, ok := p.resolve(ctx, pod, nil); ok {
		p.emit(ctx, inst)
	}
}

// onPodDeleted handles informer deletions.
func (p *Provider) onPodDeleted(ctx context.Context, obj any) {
	if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tomb.Obj
	}
	pod, ok := obj.(*corev1.Pod)
	if !ok || !p.owns(pod) {
		return
	}
	inst, err := instanceFromPod(pod)
	if err != nil {
		inst = engine.Instance{ID: pod.Name}
	}
	inst.Phase, inst.Endpoint, inst.Message, inst.Token = engine.PhaseGone, "", "pod deleted", ""
	p.forgetToken(pod.Name)
	p.clearWarned(pod.Name)
	p.emitMu.Lock()
	defer p.emitMu.Unlock()
	p.emit(ctx, inst)
}

// resolve builds the instance of a pod including its token. ok is false when
// the pod must be skipped (for now). secrets may hold prefetched token
// secrets by name.
func (p *Provider) resolve(ctx context.Context, pod *corev1.Pod, secrets map[string]*corev1.Secret) (engine.Instance, bool) {
	inst, err := instanceFromPod(pod)
	if err != nil {
		p.warnOnce(pod.Name, "ignoring an engine pod with an invalid key annotation", "engine", pod.Name, "err", err)
		return engine.Instance{}, false
	}
	if tok, ok := p.cachedToken(pod.Name); ok {
		inst.Token = tok
		return inst, true
	}
	tok, err := p.fetchToken(ctx, pod, secrets)
	switch {
	case err == nil:
		p.storeToken(pod.Name, tok)
		inst.Token = tok
	case errors.Is(err, errTokenMissing):
		// Unusable without its token: report the engine as failed, so that
		// the manager deletes it after engine.failedRetention.
		p.warnOnce(pod.Name, "engine pod has no usable token secret", "engine", pod.Name, "err", err)
		inst.Phase, inst.Endpoint, inst.Message = engine.PhaseFailed, "", truncate(err.Error())
	default:
		p.warnOnce(pod.Name, "cannot read the token of an engine pod, skipping it for now", "engine", pod.Name, "err", err)
		return engine.Instance{}, false
	}
	return inst, true
}

// fetchToken reads the token of a pod from its secret, taken from secrets
// if present there, else from the API server.
func (p *Provider) fetchToken(ctx context.Context, pod *corev1.Pod, secrets map[string]*corev1.Secret) (string, error) {
	name := secretNameOf(pod)
	s, ok := secrets[name]
	if !ok {
		gctx, cancel := context.WithTimeout(ctx, apiTimeout)
		defer cancel()
		var err error
		s, err = p.client.CoreV1().Secrets(p.namespace).Get(gctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("%w: secret %s not found", errTokenMissing, name)
		}
		if err != nil {
			return "", fmt.Errorf("read secret %s: %w", name, err)
		}
	}
	return tokenFromSecret(s, pod.Name, p.instance)
}

// tokenFromSecret extracts the token. The secret must be the one created
// for the pod: the annotation naming it is not trusted blindly.
func tokenFromSecret(s *corev1.Secret, podName, instance string) (string, error) {
	if s.Labels[LabelEngine] != podName || s.Labels[LabelInstance] != instance || s.Labels[LabelManagedBy] != managedByValue {
		return "", fmt.Errorf("%w: secret %s does not belong to engine %s", errTokenMissing, s.Name, podName)
	}
	tok := s.Data[tokenDataKey]
	if len(tok) == 0 {
		return "", fmt.Errorf("%w: secret %s has no %q entry", errTokenMissing, s.Name, tokenDataKey)
	}
	return string(tok), nil
}

// listTokenSecrets reads all token secrets of this instance. On failure it
// returns an empty map (callers fall back to single reads).
func (p *Provider) listTokenSecrets(ctx context.Context) map[string]*corev1.Secret {
	out := map[string]*corev1.Secret{}
	list, err := p.client.CoreV1().Secrets(p.namespace).List(ctx, metav1.ListOptions{LabelSelector: p.selector.String()})
	if err != nil {
		p.log.Warn("listing engine token secrets failed, reading them one by one", "err", err)
		return out
	}
	for i := range list.Items {
		out[list.Items[i].Name] = &list.Items[i]
	}
	return out
}

// secretNameOf returns the name of the token secret of a pod.
func secretNameOf(pod *corev1.Pod) string {
	if name := pod.Annotations[AnnotationTokenSecret]; name != "" {
		return name
	}
	return tokenSecretName(pod.Name)
}

// emit sends an event without blocking the informer for long: if the
// channel stays full for sendTimeout, the event is dropped.
func (p *Provider) emit(ctx context.Context, inst engine.Instance) {
	select {
	case p.events <- inst:
		return
	default:
	}
	t := time.NewTimer(p.sendTimeout)
	defer t.Stop()
	select {
	case p.events <- inst:
	case <-t.C:
		p.log.Error("engine event channel full, dropping event", "engine", inst.ID, "phase", inst.Phase, "waited", p.sendTimeout)
	case <-ctx.Done():
	}
}

// requestReplay schedules a replay for the instances that List returned.
func (p *Provider) requestReplay(ids []string) {
	p.replayMu.Lock()
	for _, id := range ids {
		p.replayIDs[id] = struct{}{}
	}
	p.replayMu.Unlock()
	select {
	case p.replayCh <- struct{}{}:
	default: // a replay is pending already and will cover these ids
	}
}

func (p *Provider) replayLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.replayCh:
		}
		t := time.NewTimer(p.replayDelay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		p.replayMu.Lock()
		ids := p.replayIDs
		p.replayIDs = map[string]struct{}{}
		p.replayMu.Unlock()
		p.replay(ctx, ids)
	}
}

// replay sends the current state of every cached engine pod again, and Gone
// for listed pods that disappeared since. The manager registers what List
// returned only after List returned and ignores events of engines it does
// not know before that; an event racing with the registration would
// otherwise be lost for good.
func (p *Provider) replay(ctx context.Context, listed map[string]struct{}) {
	lister := p.podLister()
	if lister == nil {
		return
	}
	pods, err := lister.Pods(p.namespace).List(p.selector)
	if err != nil {
		p.log.Warn("replaying engine pod states failed", "err", err)
		return
	}
	present := make(map[string]bool, len(pods))
	for _, pod := range pods {
		present[pod.Name] = true
		p.emitMu.Lock()
		// Re-read under emitMu: a pod deleted meanwhile must not be sent
		// after its Gone event (the manager would adopt a ghost).
		if cur := p.cachedPod(pod.Name); cur != nil && p.owns(cur) {
			if inst, ok := p.resolve(ctx, cur, nil); ok {
				p.emit(ctx, inst)
			}
		}
		p.emitMu.Unlock()
	}
	for id := range listed {
		if !present[id] {
			p.emit(ctx, engine.Instance{ID: id, Phase: engine.PhaseGone, Message: "pod deleted"})
		}
	}
}

// gcLoop runs the orphan collector periodically.
func (p *Provider) gcLoop(ctx context.Context) {
	t := time.NewTimer(p.gcFirstDelay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.collectOrphans(ctx)
			t.Reset(p.gcInterval)
		}
	}
}

// collectOrphans deletes token secrets of this instance whose engine pod no
// longer exists (normally the owner reference takes care of that; this
// catches launches that died half-way) and forgets tokens of such pods.
func (p *Provider) collectOrphans(ctx context.Context) {
	now := p.now()
	p.mu.Lock()
	for name, e := range p.tokens {
		if now.Sub(e.since) >= p.orphanMinAge && p.cachedPodLocked(name) == nil {
			delete(p.tokens, name)
		}
	}
	p.mu.Unlock()

	secrets := p.client.CoreV1().Secrets(p.namespace)
	list, err := secrets.List(ctx, metav1.ListOptions{LabelSelector: p.selector.String()})
	if err != nil {
		if ctx.Err() == nil {
			p.log.Warn("listing token secrets for orphan collection failed", "err", err)
		}
		return
	}
	for i := range list.Items {
		s := &list.Items[i]
		podName := s.Labels[LabelEngine]
		if podName == "" || s.DeletionTimestamp != nil || now.Sub(s.CreationTimestamp.Time) < p.orphanMinAge {
			continue
		}
		if p.nameTaken(podName) {
			continue
		}
		// The cache may lag behind: only the API server's word counts.
		gctx, cancel := context.WithTimeout(ctx, apiTimeout)
		_, err := p.client.CoreV1().Pods(p.namespace).Get(gctx, podName, metav1.GetOptions{})
		cancel()
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			if ctx.Err() == nil {
				p.log.Warn("checking the engine pod of a token secret failed", "secret", s.Name, "engine", podName, "err", err)
			}
			continue
		}
		uid := s.UID
		err = secrets.Delete(ctx, s.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if err != nil && !apierrors.IsNotFound(err) {
			p.log.Warn("deleting an orphaned token secret failed", "secret", s.Name, "err", err)
			continue
		}
		p.log.Info("deleted orphaned token secret", "secret", s.Name, "engine", podName)
	}
}

// watchError logs list/watch failures of the pod informer (it retries with
// backoff by itself).
func (p *Provider) watchError(_ context.Context, _ *cache.Reflector, err error) {
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), apierrors.IsResourceExpired(err), apierrors.IsGone(err):
		p.log.Debug("engine pod watch restarts", "err", err)
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		p.log.Error("not allowed to list/watch engine pods (RBAC: pods get, list, watch)", "err", err)
	default:
		p.log.Warn("engine pod list/watch failed, retrying", "err", err)
	}
}

// stripManagedFields drops managed fields from cached pods (memory).
func stripManagedFields(obj any) (any, error) {
	if pod, ok := obj.(*corev1.Pod); ok {
		pod.ManagedFields = nil
	}
	return obj, nil
}

// owns reports whether a pod is an engine of this gateway instance.
func (p *Provider) owns(pod *corev1.Pod) bool {
	return pod != nil && pod.Namespace == p.namespace && p.selector.Matches(labels.Set(pod.Labels))
}

func (p *Provider) podLister() corelisters.PodLister {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lister
}

// cachedPod returns a pod from the informer cache (nil if absent or before
// Run). The result must not be modified.
func (p *Provider) cachedPod(name string) *corev1.Pod {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cachedPodLocked(name)
}

func (p *Provider) cachedPodLocked(name string) *corev1.Pod {
	if p.lister == nil {
		return nil
	}
	pod, err := p.lister.Pods(p.namespace).Get(name)
	if err != nil {
		return nil
	}
	return pod
}

func (p *Provider) cachedToken(name string) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.tokens[name]
	return e.token, ok
}

func (p *Provider) storeToken(name, token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tokens[name] = tokenEntry{token: token, since: p.now()}
}

func (p *Provider) forgetToken(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.tokens, name)
}

// warnOnce logs a warning about a pod once (until the pod is deleted).
func (p *Provider) warnOnce(podName, msg string, args ...any) {
	k := podName + "\x00" + msg
	p.mu.Lock()
	seen := p.warned[k]
	p.warned[k] = true
	p.mu.Unlock()
	if !seen {
		p.log.Warn(msg, args...)
	}
}

func (p *Provider) clearWarned(podName string) {
	prefix := podName + "\x00"
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.warned {
		if strings.HasPrefix(k, prefix) {
			delete(p.warned, k)
		}
	}
}
