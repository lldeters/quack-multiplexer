package ha

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"quackmux/internal/metrics"
)

const (
	testPod  = "quackmux-abc-1"
	otherPod = "quackmux-abc-2"
)

// gatewayPod returns a gateway pod; leader adds the label and the deletion
// cost a leader leaves behind.
func gatewayPod(name string, leader bool) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: testNamespace,
		Labels:      map[string]string{"app.kubernetes.io/name": "quackmux", "app.kubernetes.io/component": "gateway"},
		Annotations: map[string]string{"prometheus.io/scrape": "true"},
	}}
	if leader {
		p.Labels[LeaderLabel] = leaderLabelValue
		p.Annotations[DeletionCostAnnotation] = leaderDeletionCost
	}
	return p
}

func getPod(t *testing.T, client kubernetes.Interface, name string) *corev1.Pod {
	t.Helper()
	p, err := client.CoreV1().Pods(testNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod %s: %v", name, err)
	}
	return p
}

// labelled reports whether the pod carries the leader label (and checks that
// the deletion cost goes with it and nothing else changed).
func labelled(t *testing.T, client kubernetes.Interface, name string) bool {
	t.Helper()
	p := getPod(t, client, name)
	_, l := p.Labels[LeaderLabel]
	_, c := p.Annotations[DeletionCostAnnotation]
	if l != c {
		t.Fatalf("pod %s: label %v but deletion cost %v", name, l, c)
	}
	if p.Labels["app.kubernetes.io/component"] != "gateway" || p.Annotations["prometheus.io/scrape"] != "true" {
		t.Fatalf("pod %s: other labels or annotations changed: %v %v", name, p.Labels, p.Annotations)
	}
	return l
}

// newLabeler returns a Labeler of testPod with short timings whose Want
// reports want.
func newLabeler(t *testing.T, client kubernetes.Interface, want *atomic.Bool, met *metrics.Metrics) *Labeler {
	t.Helper()
	l, err := NewLabeler(LabelerOptions{Client: client, Namespace: testNamespace, Pod: testPod,
		Want: want.Load, Log: testLogger(t), Metrics: met})
	if err != nil {
		t.Fatalf("NewLabeler: %v", err)
	}
	l.poll, l.resync, l.requestTimeout = 10*time.Millisecond, time.Hour, time.Second
	l.minBackoff, l.maxBackoff = 5*time.Millisecond, 20*time.Millisecond
	return l
}

// runLabeler runs l.Run until the test ends.
func runLabeler(t *testing.T, l *Labeler) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(waitTimeout):
			t.Error("Labeler.Run did not return after cancel")
		}
	})
}

// failPatches makes patches of pods fail with the error the returned
// pointer holds (nil: patches work).
func failPatches(client *fake.Clientset) *atomic.Pointer[error] {
	var failure atomic.Pointer[error]
	client.PrependReactor("patch", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if err := failure.Load(); err != nil {
			return true, nil, *err
		}
		return false, nil, nil
	})
	return &failure
}

var errUnavailable = errors.New("injected: API server unavailable")

func metricValue(t *testing.T, m *metrics.Metrics, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	mfs, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	next:
		for _, s := range mf.GetMetric() {
			for _, lp := range s.GetLabel() {
				if want, ok := labels[lp.GetName()]; ok && want != lp.GetValue() {
					continue next
				}
			}
			if s.GetGauge() != nil {
				return s.GetGauge().GetValue(), true
			}
			return s.GetCounter().GetValue(), true
		}
	}
	return 0, false
}

func TestOwnPod(t *testing.T) {
	for _, tc := range []struct {
		name, ns, pod, wantErr string
	}{
		{"both set", testNamespace, testPod, ""},
		{"trimmed", " " + testNamespace + "\n", " " + testPod, ""},
		{"no name", testNamespace, "", "POD_NAME is not set"},
		{"no namespace", "", testPod, "POD_NAMESPACE is not set"},
		{"neither", "", " ", "POD_NAME and POD_NAMESPACE are not set"},
		{"invalid namespace", "Not_Valid", testPod, "invalid POD_NAMESPACE"},
		{"invalid name", testNamespace, "pod/../x", "invalid POD_NAME"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(PodNamespaceEnv, tc.ns)
			t.Setenv(PodNameEnv, tc.pod)
			ns, pod, err := OwnPod()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("OwnPod() = %q, %q, %v; want an error with %q", ns, pod, err, tc.wantErr)
				}
				if !strings.Contains(err.Error(), "downward API") && !strings.Contains(err.Error(), "invalid") {
					t.Errorf("the error does not say how to fix it: %v", err)
				}
				return
			}
			if err != nil || ns != testNamespace || pod != testPod {
				t.Fatalf("OwnPod() = %q, %q, %v", ns, pod, err)
			}
		})
	}
}

func TestNewLabeler(t *testing.T) {
	want := func() bool { return false }
	client := fake.NewSimpleClientset()
	for name, o := range map[string]LabelerOptions{
		"no client":    {Want: want, Namespace: "ns", Pod: "p"},
		"no Want":      {Client: client, Namespace: "ns", Pod: "p"},
		"no namespace": {Client: client, Want: want, Pod: "p"},
		"no pod":       {Client: client, Want: want, Namespace: "ns", Pod: " "},
	} {
		if _, err := NewLabeler(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	l, err := NewLabeler(LabelerOptions{Client: client, Want: want, Namespace: " ns ", Pod: "p\n"})
	if err != nil || l.Pod() != "ns/p" {
		t.Fatalf("NewLabeler: %v, pod %q", err, l.Pod())
	}
}

// Only gateways with leader election export the label metrics; several
// labelers may share one Metrics.
func TestLabelerRegistersItsMetrics(t *testing.T) {
	met := metrics.New()
	if _, ok := metricValue(t, met, "quackmux_leader_label", nil); ok {
		t.Fatal("quackmux_leader_label is exported without a labeler")
	}
	var want atomic.Bool
	newLabeler(t, fake.NewSimpleClientset(), &want, met)
	newLabeler(t, fake.NewSimpleClientset(), &want, met)
	if v, ok := metricValue(t, met, "quackmux_leader_label", nil); !ok || v != 0 {
		t.Fatalf("quackmux_leader_label = %v (exported %v), want 0", v, ok)
	}
}

// Init removes the label a predecessor left behind, and refuses to go on when
// the pod can never be labelled.
func TestLabelerInit(t *testing.T) {
	t.Run("removes a stale label", func(t *testing.T) {
		client := fake.NewSimpleClientset(gatewayPod(testPod, true))
		met := metrics.New()
		var want atomic.Bool
		l := newLabeler(t, client, &want, met)
		if l.Absent() {
			t.Fatal("absent before anything was removed")
		}
		if err := l.Init(context.Background()); err != nil {
			t.Fatal(err)
		}
		if labelled(t, client, testPod) || !l.Absent() {
			t.Fatal("the stale label is still there")
		}
		if v, _ := metricValue(t, met, "quackmux_leader_label_updates_total", map[string]string{"action": "remove", "result": "ok"}); v != 1 {
			t.Errorf("remove/ok = %v, want 1", v)
		}
	})
	t.Run("own pod not found", func(t *testing.T) {
		var want atomic.Bool
		l := newLabeler(t, fake.NewSimpleClientset(), &want, nil) // no pod named testPod
		err := l.Init(context.Background())
		if err == nil || !strings.Contains(err.Error(), "does not exist") || !strings.Contains(err.Error(), PodNameEnv) {
			t.Fatalf("Init = %v", err)
		}
	})
	t.Run("forbidden", func(t *testing.T) {
		client := fake.NewSimpleClientset(gatewayPod(testPod, false))
		forbidden := error(apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, testPod, errors.New("RBAC: no patch")))
		failPatches(client).Store(&forbidden)
		var want atomic.Bool
		l := newLabeler(t, client, &want, nil)
		err := l.Init(context.Background())
		if err == nil || !strings.Contains(err.Error(), "rbac.yaml") || !strings.Contains(err.Error(), "no patch") {
			t.Fatalf("Init = %v", err)
		}
	})
	t.Run("transient errors are retried by Run", func(t *testing.T) {
		client := fake.NewSimpleClientset(gatewayPod(testPod, true))
		failure := failPatches(client)
		failure.Store(&errUnavailable)
		var want atomic.Bool
		l := newLabeler(t, client, &want, nil)
		if err := l.Init(context.Background()); err != nil {
			t.Fatalf("Init failed on a transient error: %v", err)
		}
		if l.Absent() || !strings.Contains(l.Status(), "injected") {
			t.Fatalf("Absent %v, status %q", l.Absent(), l.Status())
		}
		runLabeler(t, l)
		time.Sleep(50 * time.Millisecond)
		if l.Absent() {
			t.Fatal("absent while the API fails")
		}
		failure.Store(nil)
		waitFor(t, "the removal after the API recovered", l.Absent)
		if labelled(t, client, testPod) {
			t.Fatal("Absent, but the pod still carries the label")
		}
	})
}

// A container that restarts in the same pod finds the label of its
// predecessor: Run removes it before the replica may report ready.
func TestLabelerRemovesStaleLabelAtStart(t *testing.T) {
	client := fake.NewSimpleClientset(gatewayPod(testPod, true))
	met := metrics.New()
	var want atomic.Bool
	l := newLabeler(t, client, &want, met)
	runLabeler(t, l)
	waitFor(t, "the stale label to be removed", l.Absent)
	if labelled(t, client, testPod) {
		t.Fatal("Absent, but the pod still carries the label")
	}
	if v, _ := metricValue(t, met, "quackmux_leader_label", nil); v != 0 {
		t.Errorf("gauge = %v", v)
	}
}

// Init's removal counts as the first write of Run.
func TestLabelerRunAfterInitDoesNotPatchAgain(t *testing.T) {
	client := fake.NewSimpleClientset(gatewayPod(testPod, false))
	var patches atomic.Int32
	client.PrependReactor("patch", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		patches.Add(1)
		return false, nil, nil
	})
	var want atomic.Bool
	l := newLabeler(t, client, &want, nil)
	if err := l.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	runLabeler(t, l)
	time.Sleep(50 * time.Millisecond)
	if n := patches.Load(); n != 1 {
		t.Fatalf("%d patches, want 1 (Init)", n)
	}
}

func TestLabelerFollowsWant(t *testing.T) {
	client := fake.NewSimpleClientset(gatewayPod(testPod, false))
	met := metrics.New()
	var want atomic.Bool
	l := newLabeler(t, client, &want, met)
	runLabeler(t, l)
	waitFor(t, "the label to be confirmed absent", l.Absent)

	want.Store(true)
	l.Kick()
	waitFor(t, "the label to be set", func() bool { return labelled(t, client, testPod) })
	p := getPod(t, client, testPod)
	if p.Labels[LeaderLabel] != "true" || p.Annotations[DeletionCostAnnotation] != "100" {
		t.Fatalf("labels %v, annotations %v", p.Labels, p.Annotations)
	}
	if l.Absent() {
		t.Fatal("Absent while the label is set")
	}
	waitFor(t, "the gauge", func() bool { v, _ := metricValue(t, met, "quackmux_leader_label", nil); return v == 1 })

	want.Store(false) // no Kick: the poll notices it
	waitFor(t, "the label to be removed", func() bool { return !labelled(t, client, testPod) && l.Absent() })
	if v, _ := metricValue(t, met, "quackmux_leader_label", nil); v != 0 {
		t.Error("gauge still 1")
	}
	if v, _ := metricValue(t, met, "quackmux_leader_label_updates_total", map[string]string{"action": "set", "result": "ok"}); v != 1 {
		t.Errorf("set/ok = %v, want 1", v)
	}
}

// Absent is false until a removal is confirmed.
func TestLabelerAbsentNeedsConfirmation(t *testing.T) {
	client := fake.NewSimpleClientset(gatewayPod(testPod, true))
	failure := failPatches(client)
	failure.Store(&errUnavailable)
	met := metrics.New()
	var want atomic.Bool
	l := newLabeler(t, client, &want, met)
	runLabeler(t, l)
	time.Sleep(100 * time.Millisecond)
	if l.Absent() || !strings.Contains(l.Status(), "injected") {
		t.Fatalf("Absent %v, status %q", l.Absent(), l.Status())
	}
	e := newElector(t, fake.NewSimpleClientset(), testPod, shortTimings(), nil)
	e.campaigning.Store(true)
	ok, why := StandbyReady(e, l)
	if ok || !strings.Contains(why, LeaderLabel) || !strings.Contains(why, "not yet removed") || !strings.Contains(why, "injected") {
		t.Fatalf("StandbyReady = %v, %q", ok, why)
	}
	if v, _ := metricValue(t, met, "quackmux_leader_label_updates_total", map[string]string{"action": "remove", "result": "error"}); v < 2 {
		t.Errorf("remove/error = %v, want retries", v)
	}
	failure.Store(nil)
	waitFor(t, "the removal after the API recovered", l.Absent)
}

// A follower stays ready through API hiccups: a failed resync of an absent
// label does not make it "possibly present".
func TestLabelerFailedResyncKeepsAbsent(t *testing.T) {
	client := fake.NewSimpleClientset(gatewayPod(testPod, false))
	failure := failPatches(client)
	var want atomic.Bool
	l := newLabeler(t, client, &want, nil)
	l.resync = 10 * time.Millisecond
	runLabeler(t, l)
	waitFor(t, "absent", l.Absent)
	failure.Store(&errUnavailable)
	time.Sleep(100 * time.Millisecond)
	if !l.Absent() {
		t.Fatal("a failed resync made the label possibly present")
	}
}

// The resync repairs a label that somebody else removed from the leader.
func TestLabelerResyncRepairsTheLabel(t *testing.T) {
	client := fake.NewSimpleClientset(gatewayPod(testPod, false))
	var want atomic.Bool
	want.Store(true)
	l := newLabeler(t, client, &want, nil)
	l.resync = 20 * time.Millisecond
	runLabeler(t, l)
	waitFor(t, "the label", func() bool { return labelled(t, client, testPod) })
	p := getPod(t, client, testPod)
	delete(p.Labels, LeaderLabel)
	delete(p.Annotations, DeletionCostAnnotation)
	if _, err := client.CoreV1().Pods(testNamespace).Update(context.Background(), p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the label to come back", func() bool { return labelled(t, client, testPod) })
}

// Want is checked right before every set: a leader that resumed after a
// freeze (lease no longer fresh) never labels its pod, even if Run saw Want
// true a moment before.
func TestLabelerFence(t *testing.T) {
	client := fake.NewSimpleClientset(gatewayPod(testPod, false))
	var calls atomic.Int32
	var want atomic.Bool
	l := newLabeler(t, client, &want, nil)
	// Want is true for the first evaluation only (in Run), false at the fence.
	l.want = func() bool { return calls.Add(1) == 1 }
	if err := l.apply(context.Background(), l.desired()); err != nil {
		t.Fatal(err)
	}
	if labelled(t, client, testPod) {
		t.Fatal("labelled although Want was false right before the patch")
	}
	if calls.Load() != 2 {
		t.Fatalf("Want evaluated %d times, want 2 (Run, fence)", calls.Load())
	}
}

// Close wins over a set that is under way, and nothing sets the label after
// Close.
func TestLabelerCloseRemovesForGood(t *testing.T) {
	client := fake.NewSimpleClientset(gatewayPod(testPod, false))
	release := make(chan struct{})
	var sets atomic.Int32
	client.PrependReactor("patch", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if strings.Contains(string(a.(k8stesting.PatchAction).GetPatch()), `"true"`) && sets.Add(1) == 1 {
			<-release // the first set hangs until the test releases it
		}
		return false, nil, nil
	})
	var want atomic.Bool
	want.Store(true)
	l := newLabeler(t, client, &want, nil)
	runLabeler(t, l)
	waitFor(t, "the set to be under way", func() bool { return sets.Load() == 1 })

	closed := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
		defer cancel()
		closed <- l.Close(ctx)
	}()
	time.Sleep(20 * time.Millisecond)
	close(release) // the set lands first, then Close removes the label
	if err := <-closed; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if labelled(t, client, testPod) || !l.Absent() {
		t.Fatal("the label survived Close")
	}
	l.Kick()
	time.Sleep(50 * time.Millisecond)
	if labelled(t, client, testPod) {
		t.Fatal("the label was set again after Close")
	}
}

// Close keeps to its deadline while a set is under way (a resync on a slow
// API server); Run removes the label once that set has landed.
func TestLabelerCloseGivesUpOnASetUnderWay(t *testing.T) {
	client := fake.NewSimpleClientset(gatewayPod(testPod, false))
	release := make(chan struct{})
	var sets atomic.Int32
	client.PrependReactor("patch", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if strings.Contains(string(a.(k8stesting.PatchAction).GetPatch()), `"true"`) && sets.Add(1) == 1 {
			<-release
		}
		return false, nil, nil
	})
	var want atomic.Bool
	want.Store(true)
	l := newLabeler(t, client, &want, nil)
	runLabeler(t, l)
	waitFor(t, "the set to be under way", func() bool { return sets.Load() == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := l.Close(ctx); err == nil || !strings.Contains(err.Error(), "under way") {
		t.Fatalf("Close = %v, want an error about the patch under way", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Close took %s", d)
	}
	close(release) // the set lands after Close gave up
	waitFor(t, "Run to remove the label after the set", func() bool { return !labelled(t, client, testPod) && l.Absent() })
	if sets.Load() != 1 {
		t.Fatalf("%d sets, want 1", sets.Load())
	}
}

func TestLabelerCloseGivesUpWithTheContext(t *testing.T) {
	client := fake.NewSimpleClientset(gatewayPod(testPod, true))
	failPatches(client).Store(&errUnavailable)
	var want atomic.Bool
	l := newLabeler(t, client, &want, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := l.Close(ctx); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("Close = %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Close took %s", d)
	}
}

func TestRemoveStale(t *testing.T) {
	fresh := func() bool { return true }
	t.Run("removes the label of the previous leader", func(t *testing.T) {
		client := fake.NewSimpleClientset(gatewayPod(testPod, true), gatewayPod(otherPod, true))
		met := metrics.New()
		var want atomic.Bool
		l := newLabeler(t, client, &want, met)
		if err := l.RemoveStale(context.Background(), otherPod, fresh); err != nil {
			t.Fatal(err)
		}
		if labelled(t, client, otherPod) {
			t.Fatal("the previous leader is still labelled")
		}
		if !labelled(t, client, testPod) {
			t.Fatal("RemoveStale touched the own pod")
		}
		if v, _ := metricValue(t, met, "quackmux_leader_label_updates_total", map[string]string{"action": "remove_stale", "result": "ok"}); v != 1 {
			t.Errorf("remove_stale/ok = %v, want 1", v)
		}
	})
	t.Run("a deleted pod is fine", func(t *testing.T) {
		l := newLabeler(t, fake.NewSimpleClientset(), new(atomic.Bool), nil)
		if err := l.RemoveStale(context.Background(), otherPod, fresh); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("own pod and empty name are ignored", func(t *testing.T) {
		client := fake.NewSimpleClientset(gatewayPod(testPod, true))
		l := newLabeler(t, client, new(atomic.Bool), nil)
		for _, p := range []string{"", testPod} {
			if err := l.RemoveStale(context.Background(), p, fresh); err != nil {
				t.Fatal(err)
			}
		}
		if !labelled(t, client, testPod) {
			t.Fatal("the own label was removed")
		}
	})
	t.Run("retries until it works", func(t *testing.T) {
		client := fake.NewSimpleClientset(gatewayPod(otherPod, true))
		failure := failPatches(client)
		failure.Store(&errUnavailable)
		l := newLabeler(t, client, new(atomic.Bool), nil)
		time.AfterFunc(50*time.Millisecond, func() { failure.Store(nil) })
		if err := l.RemoveStale(context.Background(), otherPod, fresh); err != nil {
			t.Fatal(err)
		}
		if labelled(t, client, otherPod) {
			t.Fatal("still labelled")
		}
	})
	t.Run("stops when no longer leading", func(t *testing.T) {
		client := fake.NewSimpleClientset(gatewayPod(otherPod, true))
		l := newLabeler(t, client, new(atomic.Bool), nil)
		if err := l.RemoveStale(context.Background(), otherPod, func() bool { return false }); err == nil {
			t.Fatal("removed a label without a fresh lease")
		}
		if !labelled(t, client, otherPod) {
			t.Fatal("removed a label without a fresh lease")
		}
	})
	t.Run("stops when closed", func(t *testing.T) {
		client := fake.NewSimpleClientset(gatewayPod(testPod, false), gatewayPod(otherPod, true))
		l := newLabeler(t, client, new(atomic.Bool), nil)
		if err := l.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := l.RemoveStale(context.Background(), otherPod, fresh); err == nil || !labelled(t, client, otherPod) {
			t.Fatalf("RemoveStale after Close = %v", err)
		}
	})
}

func TestStandbyReady(t *testing.T) {
	e := newElector(t, fake.NewSimpleClientset(), "gw-a", shortTimings(), nil)
	var want atomic.Bool
	l := newLabeler(t, fake.NewSimpleClientset(gatewayPod(testPod, true)), &want, nil)
	if ok, why := StandbyReady(e, l); ok || !strings.Contains(why, "not yet removed") {
		t.Fatalf("label not removed: %v %q", ok, why)
	}
	if err := l.apply(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if ok, why := StandbyReady(e, l); ok || !strings.Contains(why, "campaign") {
		t.Fatalf("not campaigning: %v %q", ok, why)
	}
	e.campaigning.Store(true)
	if ok, why := StandbyReady(e, l); !ok || why != "" {
		t.Fatalf("ready follower: %v %q", ok, why)
	}
	// A leader whose lease went stale (it resumed after a freeze) steps
	// down: never ready, although its label is absent and it campaigns.
	tr := newRenewTracker(&resourcelock.LeaseLock{LockConfig: resourcelock.ResourceLockConfig{Identity: "gw-a"}})
	tr.led.Store(true)
	tr.observe(time.Since(tr.base)-2*e.renewDeadline, resourcelock.LeaderElectionRecord{HolderIdentity: "gw-a"}, nil)
	e.renew.Store(tr)
	e.setLeader(true)
	if ok, why := StandbyReady(e, l); ok || !strings.Contains(why, "stepping down") {
		t.Fatalf("stale leader: %v %q", ok, why)
	}
}

// The handover as the gateway does it: the new leader takes over the expired
// lease of a frozen leader, labels its own pod and removes the label of the
// frozen one; at shutdown it removes its label before it releases the lease.
func TestLabelHandover(t *testing.T) {
	client := fake.NewSimpleClientset(gatewayPod(testPod, false), gatewayPod(otherPod, true))
	// otherPod led and froze: its lease expired, its label is still there.
	holderID := otherPod
	d := int32(1)
	old := metav1.NewMicroTime(time.Now().Add(-time.Hour))
	if _, err := client.CoordinationV1().Leases(testNamespace).Create(context.Background(), &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: testLease, Namespace: testNamespace},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holderID, LeaseDurationSeconds: &d, RenewTime: &old},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	e := newElector(t, client, testPod, shortTimings(), nil)
	e.campaignDelay = 0
	var serving atomic.Bool
	l := newLabeler(t, client, &serving, nil)
	l.want = func() bool { return serving.Load() && e.Fresh() }
	if err := l.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	runLabeler(t, l)

	var wg sync.WaitGroup
	c := start(t, e, false)
	lctx := c.waitLeading(t)
	if prev := e.Displaced(); prev != otherPod {
		t.Fatalf("Displaced() = %q", prev)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = l.RemoveStale(lctx, e.Displaced(), e.Fresh)
	}()
	serving.Store(true) // synced
	l.Kick()
	waitFor(t, "the new leader's label", func() bool { return labelled(t, client, testPod) })
	waitFor(t, "the old leader's label to go", func() bool { return !labelled(t, client, otherPod) })
	wg.Wait()

	// Shutdown: label first, then the lease.
	serving.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	if err := l.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if labelled(t, client, testPod) {
		t.Fatal("label still set after Close")
	}
	if err := c.stop(t); err != nil {
		t.Fatal(err)
	}
	if h := holder(getLease(t, client)); h != "" {
		t.Fatalf("lease not released: %q", h)
	}
}
