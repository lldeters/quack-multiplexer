package ha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"

	"quackmux/internal/metrics"
)

// LeaderLabel marks the gateway pod that serves Quack traffic. With leader
// election every healthy replica is ready, and the Service of the HA overlay
// also selects this label, so that it routes to the leader alone.
const LeaderLabel = "quackmux.io/leader"

// leaderLabelValue is the value of LeaderLabel on the leader's pod.
const leaderLabelValue = "true"

// DeletionCostAnnotation is the pod deletion cost of the ReplicaSet
// controller. The leader sets it on its pod, so that scaling down (the old
// ReplicaSet in a rollout, kubectl scale) removes followers first and a
// rollout hands over only once. Followers carry none (cost 0); a value from
// the pod template is overwritten.
const DeletionCostAnnotation = "controller.kubernetes.io/pod-deletion-cost"

// leaderDeletionCost is the deletion cost of the leader's pod.
const leaderDeletionCost = "100"

// PodNamespaceEnv names the namespace of the gateway's own pod (set it from
// the downward API: fieldRef metadata.namespace).
const PodNamespaceEnv = "POD_NAMESPACE"

// fieldManager names the gateway in the managed fields of the pods it patches.
const fieldManager = "quackmux"

// Default timings of a Labeler (fields of the instance, so that tests can
// shorten them).
const (
	// labelPoll is how often Run evaluates Want without a Kick.
	labelPoll = time.Second
	// labelResync is how often Run writes the desired state although nothing
	// changed: it repairs a label that somebody else changed.
	labelResync = 30 * time.Second
	// labelRequestTimeout bounds one patch.
	labelRequestTimeout = 5 * time.Second
	// labelMinBackoff and labelMaxBackoff bound the retries after a failure.
	labelMinBackoff = 250 * time.Millisecond
	labelMaxBackoff = 5 * time.Second
)

// OwnPod returns the namespace and the name of the pod the gateway runs in,
// from $POD_NAMESPACE and $POD_NAME. Leader election needs both: the leader
// labels its own pod, and the pod name is its identity in the lease.
func OwnPod() (namespace, name string, err error) {
	namespace = strings.TrimSpace(os.Getenv(PodNamespaceEnv))
	name = strings.TrimSpace(os.Getenv(PodNameEnv))
	var missing []string
	if name == "" {
		missing = append(missing, PodNameEnv)
	}
	if namespace == "" {
		missing = append(missing, PodNamespaceEnv)
	}
	if len(missing) > 0 {
		verb := "is"
		if len(missing) > 1 {
			verb = "are"
		}
		return "", "", fmt.Errorf("ha: leader election needs the gateway to run as a Kubernetes pod that knows its name and namespace, "+
			"but %s %s not set: set %s and %s from the downward API (fieldRef metadata.name and metadata.namespace, "+
			"see deploy/kubernetes/base/deployment.yaml)", strings.Join(missing, " and "), verb, PodNameEnv, PodNamespaceEnv)
	}
	if errs := validation.IsDNS1123Label(namespace); len(errs) > 0 {
		return "", "", fmt.Errorf("ha: invalid %s %q: %s", PodNamespaceEnv, namespace, strings.Join(errs, "; "))
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return "", "", fmt.Errorf("ha: invalid %s %q: %s", PodNameEnv, name, strings.Join(errs, "; "))
	}
	return namespace, name, nil
}

// labelState is what this process knows about the label on its own pod.
type labelState int

const (
	// labelUnknown: the label may be on the pod (at start-up, while it is
	// being set, after a failed attempt to set it).
	labelUnknown labelState = iota
	// labelAbsent: a removal succeeded, and the label was not set since.
	labelAbsent
	// labelPresent: setting the label succeeded.
	labelPresent
)

// LabelerOptions configure a Labeler.
type LabelerOptions struct {
	// Client patches pods; it needs "patch" on pods in Namespace.
	Client kubernetes.Interface
	// Namespace and Pod name the gateway's own pod (OwnPod). Required.
	Namespace string
	Pod       string
	// Want reports whether this process serves Quack traffic, that is,
	// whether its pod should carry the label. It is evaluated on Kick, every
	// second and right before the label is set, so it must be cheap and must
	// include the freshness of the lease (a leader that resumed after a
	// freeze must not label its pod). Required.
	Want func() bool
	// Log receives label changes and API errors; nil discards them.
	Log *slog.Logger
	// Metrics, if set, gets LeaderLabel and LeaderLabelUpdates registered
	// and updated.
	Metrics *metrics.Metrics
}

// Labeler keeps LeaderLabel (and DeletionCostAnnotation) on the gateway's own
// pod in line with Want: set while this process serves Quack traffic, removed
// otherwise. It starts by removing a label that an earlier container of the
// same pod may have left behind. RemoveStale removes the label from the pod
// of a leader whose expired lease this replica took over.
//
// The label steers the Service, but readiness is what keeps a replica that
// does not serve from receiving traffic: such a replica reports ready only
// once the label is confirmed absent (Absent, StandbyReady), and the Service
// routes only to ready pods. Methods are safe for concurrent use.
type Labeler struct {
	client kubernetes.Interface
	ns     string
	pod    string
	want   func() bool
	log    *slog.Logger
	met    *metrics.Metrics

	poll, resync, requestTimeout time.Duration
	minBackoff, maxBackoff       time.Duration

	kick chan struct{}

	// opMu serialises the patches of the own pod, so that the removal in
	// Close cannot be overtaken by a set that is already under way.
	opMu sync.Mutex

	mu      sync.Mutex
	closed  bool
	state   labelState
	written time.Time // last successful patch of the own pod
	err     error     // last error for the own pod; nil after a success
}

// NewLabeler validates the options and creates a Labeler. It does not contact
// the Kubernetes API.
func NewLabeler(o LabelerOptions) (*Labeler, error) {
	if o.Client == nil {
		return nil, errors.New("ha: the leader label needs a Kubernetes client")
	}
	if o.Want == nil {
		return nil, errors.New("ha: the leader label needs a Want function")
	}
	ns, pod := strings.TrimSpace(o.Namespace), strings.TrimSpace(o.Pod)
	if ns == "" || pod == "" {
		return nil, errors.New("ha: the leader label needs the namespace and the name of the gateway's pod")
	}
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	l := &Labeler{
		client: o.Client, ns: ns, pod: pod, want: o.Want, met: o.Metrics,
		log:  log.With("component", "ha", "pod", ns+"/"+pod, "label", LeaderLabel),
		poll: labelPoll, resync: labelResync, requestTimeout: labelRequestTimeout,
		minBackoff: labelMinBackoff, maxBackoff: labelMaxBackoff,
		kick: make(chan struct{}, 1),
	}
	if l.met != nil {
		l.met.RegisterLeaderLabel()
	}
	l.setGauge(false)
	return l, nil
}

// Pod returns "namespace/name" of the gateway's own pod.
func (l *Labeler) Pod() string { return l.ns + "/" + l.pod }

// Init removes the label from the own pod, which Run would do first: a
// container that restarts in the same pod finds the label of its
// predecessor. It fails when retrying cannot help, so that a gateway that
// can never label its pod does not start: the pod does not exist (POD_NAME
// or POD_NAMESPACE do not name it) or the API server refuses the patch (the
// Role lacks "patch" on pods, an admission webhook objects). Other errors,
// such as an API server that is not reachable yet, are only logged: Run
// retries, and the pod stays unready until the removal succeeded.
func (l *Labeler) Init(ctx context.Context) error {
	err := l.apply(ctx, false)
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err):
		return fmt.Errorf("ha: the gateway pod %s does not exist: %s and %s must name the pod the gateway runs in "+
			"(downward API, see deploy/kubernetes/base/deployment.yaml)", l.Pod(), PodNameEnv, PodNamespaceEnv)
	case apierrors.IsForbidden(err):
		return fmt.Errorf("ha: the gateway may not label its pod %s, which leader election needs (its Role must allow "+
			"\"patch\" on pods in namespace %s, see deploy/kubernetes/base/rbac.yaml): %w", l.Pod(), l.ns, err)
	}
	return nil
}

// Kick makes Run evaluate Want at once (call it when serving starts or ends).
func (l *Labeler) Kick() {
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// Absent reports whether the label is known to be absent from the own pod: a
// removal succeeded, and the label was not set since. A replica that does not
// serve reports ready only then.
func (l *Labeler) Absent() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state == labelAbsent
}

// Status describes the label on the own pod for readiness messages.
func (l *Labeler) Status() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var s string
	switch l.state {
	case labelAbsent:
		s = "absent"
	case labelPresent:
		s = "present"
	default:
		s = "not yet removed"
	}
	if l.err != nil {
		s += ": " + l.err.Error()
	}
	return s
}

// StandbyReady is the readiness of a replica with leader election that does
// not serve (a follower, or a leader that is starting up or stepping down):
// ready once the label is confirmed absent from its pod, so that the Service
// cannot route to it, and once it campaigns, so that it can take over at
// once. A replica that serves is ready without asking.
func StandbyReady(e *Elector, l *Labeler) (bool, string) {
	if !l.Absent() {
		return false, "label " + LeaderLabel + " of pod " + l.Pod() + " " + l.Status()
	}
	if !e.Campaigning() {
		return false, "waiting to campaign for leadership"
	}
	return true, ""
}

// Run keeps the label in line with Want until ctx ends. It writes the desired
// state at once unless Init just did (at start-up that removes a stale
// label), on every change and every resync period; failures are retried with
// backoff.
func (l *Labeler) Run(ctx context.Context) {
	var backoff time.Duration
	for {
		want := l.desired()
		l.mu.Lock()
		state, written := l.state, l.written
		l.mu.Unlock()
		wait := l.poll
		mismatch := (want && state != labelPresent) || (!want && state != labelAbsent)
		if mismatch || time.Since(written) >= l.resync {
			if err := l.apply(ctx, want); err != nil {
				if ctx.Err() != nil {
					return
				}
				backoff = max(l.minBackoff, min(2*backoff, l.maxBackoff))
				wait = backoff
			} else {
				backoff = 0
			}
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-l.kick:
			t.Stop()
		case <-t.C:
		}
	}
}

// Close stops setting the label and removes it from the own pod. It returns
// once the removal succeeded, or with the last error when ctx ends. The
// gateway calls it when it shuts down, before it releases the lease.
func (l *Labeler) Close(ctx context.Context) error {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	backoff := l.minBackoff
	for {
		err := l.apply(ctx, false)
		if err == nil {
			return nil
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
		backoff = min(2*backoff, l.maxBackoff)
	}
}

// RemoveStale removes the label from another pod: the pod of the leader whose
// expired lease this replica took over (Elector.Displaced). That pod may sit
// on a lost node and count as ready until the node controller notices. It
// retries until the label is gone (or the pod is), ctx ends, the Labeler is
// closed or fresh reports false (this replica no longer leads).
func (l *Labeler) RemoveStale(ctx context.Context, pod string, fresh func() bool) error {
	pod = strings.TrimSpace(pod)
	if pod == "" || pod == l.pod {
		return nil
	}
	backoff := l.minBackoff
	for {
		l.mu.Lock()
		closed := l.closed
		l.mu.Unlock()
		if closed || (fresh != nil && !fresh()) {
			return errors.New("ha: not removing the leader label of " + pod + ": no longer leading")
		}
		err := l.patch(ctx, pod, false)
		if apierrors.IsNotFound(err) {
			err = nil // the pod is gone, and its label with it
		}
		l.count("remove_stale", err)
		if err == nil {
			l.log.Info("removed the leader label from the pod of the previous leader", "previousLeader", pod)
			return nil
		}
		l.log.Warn("removing the leader label from the pod of the previous leader failed; retrying", "previousLeader", pod, "err", err)
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
		backoff = min(2*backoff, l.maxBackoff)
	}
}

// desired reports whether the own pod should carry the label now.
func (l *Labeler) desired() bool {
	l.mu.Lock()
	closed := l.closed
	l.mu.Unlock()
	return !closed && l.want()
}

// apply sets the label on the own pod or removes it.
func (l *Labeler) apply(ctx context.Context, set bool) error {
	l.opMu.Lock()
	defer l.opMu.Unlock()
	l.mu.Lock()
	before := l.state
	l.mu.Unlock()
	if set {
		// Fence, as late as possible: a leader that resumed after a freeze
		// (or that is shutting down) must not label its pod.
		if !l.desired() {
			return nil
		}
		l.mu.Lock()
		l.state = labelUnknown // from now on the label may be on the pod
		l.mu.Unlock()
	}
	err := l.patch(ctx, l.pod, set)
	action := "remove"
	if set {
		action = "set"
	}
	l.count(action, err)

	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		if l.err == nil || l.err.Error() != err.Error() {
			l.log.Warn("updating the leader label of this pod failed; retrying", "set", set, "err", err)
		}
		l.err = err
		return err // a failed removal leaves the state as it was
	}
	if l.err != nil {
		l.log.Info("updating the leader label of this pod works again")
	}
	l.err = nil
	l.written = time.Now()
	if set {
		l.state = labelPresent
	} else {
		l.state = labelAbsent
	}
	if before != l.state { // not for a resync
		if set {
			l.log.Info("labelled this pod as the leader")
		} else {
			l.log.Info("the leader label is absent from this pod")
		}
	}
	l.setGauge(set)
	return nil
}

// patch sets or removes the label and the deletion cost of a pod with a JSON
// merge patch. Removing an absent label changes nothing (the API server does
// not write the pod); a missing pod is an error.
func (l *Labeler) patch(ctx context.Context, pod string, set bool) error {
	var label, cost any // nil removes the key
	if set {
		label, cost = leaderLabelValue, leaderDeletionCost
	}
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"labels":      map[string]any{LeaderLabel: label},
		"annotations": map[string]any{DeletionCostAnnotation: cost},
	}})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, l.requestTimeout)
	defer cancel()
	_, err = l.client.CoreV1().Pods(l.ns).Patch(ctx, pod, types.MergePatchType, body, metav1.PatchOptions{FieldManager: fieldManager})
	return err
}

func (l *Labeler) count(action string, err error) {
	if l.met == nil {
		return
	}
	result := "ok"
	if err != nil {
		result = "error"
	}
	l.met.LeaderLabelUpdates.WithLabelValues(action, result).Inc()
}

func (l *Labeler) setGauge(present bool) {
	if l.met == nil {
		return
	}
	if present {
		l.met.LeaderLabel.Set(1)
	} else {
		l.met.LeaderLabel.Set(0)
	}
}
