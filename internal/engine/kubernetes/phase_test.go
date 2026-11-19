package kubernetes

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"quackmux/internal/engine"
)

func terminated(name, reason string, code int32, msg string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		Reason: reason, ExitCode: code, Message: msg,
	}}}
}

func waiting(name, reason, msg string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
		Reason: reason, Message: msg,
	}}}
}

func TestPodStatus(t *testing.T) {
	now := metav1.Now()
	unschedulable := corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable",
		Message: "0/3 nodes are available: 3 Insufficient memory.",
	}}}
	notReady := readyStatus("10.1.2.3")
	notReady.Conditions[1].Status = corev1.ConditionFalse
	noIP := readyStatus("")

	cases := []struct {
		name     string
		deleting bool
		status   corev1.PodStatus
		phase    engine.Phase
		endpoint string
		message  string // substring
	}{
		{name: "ready", status: readyStatus("10.1.2.3"), phase: engine.PhaseReady, endpoint: "http://10.1.2.3:9494"},
		{name: "ready ipv6", status: readyStatus("fd00::12"), phase: engine.PhaseReady, endpoint: "http://[fd00::12]:9494"},
		{name: "deleting", deleting: true, status: readyStatus("10.1.2.3"), phase: engine.PhaseFailed, message: "pod is being deleted"},
		{name: "running not ready", status: notReady, phase: engine.PhaseStarting, message: "not ready"},
		{name: "ready without ip", status: noIP, phase: engine.PhaseStarting, message: "pod IP"},
		{name: "empty status", status: corev1.PodStatus{}, phase: engine.PhaseStarting, message: "pending"},
		{name: "pending", status: corev1.PodStatus{Phase: corev1.PodPending}, phase: engine.PhaseStarting, message: "pending"},
		{name: "unschedulable", status: unschedulable, phase: engine.PhaseStarting,
			message: "Unschedulable: 0/3 nodes are available: 3 Insufficient memory."},
		{name: "container creating", status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{
			waiting(ContainerName, "ContainerCreating", "")}}, phase: engine.PhaseStarting, message: "ContainerCreating"},
		{name: "image pull error", status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{
			waiting(ContainerName, "ErrImagePull", "rpc error: not found")}}, phase: engine.PhaseStarting,
			message: "ErrImagePull: rpc error: not found"},
		{name: "image pull backoff", status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{
			waiting(ContainerName, "ImagePullBackOff", `Back-off pulling image "x"`)}}, phase: engine.PhaseStarting,
			message: `ImagePullBackOff: Back-off pulling image "x"`},
		{name: "config error", status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{
			waiting(ContainerName, "CreateContainerConfigError", `secret "x-token" not found`)}}, phase: engine.PhaseStarting,
			message: `CreateContainerConfigError: secret "x-token" not found`},
		{name: "create error", status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{
			waiting(ContainerName, "CreateContainerError", "boom")}}, phase: engine.PhaseStarting, message: "CreateContainerError: boom"},
		{name: "sidecar pull backoff", status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{
			waiting("proxy", "ImagePullBackOff", "nope"), waiting(ContainerName, "", "")}}, phase: engine.PhaseStarting,
			message: "container proxy: ImagePullBackOff: nope"},
		{name: "invalid image", status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{
			waiting(ContainerName, "InvalidImageName", "couldn't parse image reference")}}, phase: engine.PhaseFailed,
			message: "InvalidImageName: couldn't parse image reference"},
		{name: "never pull", status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{
			waiting(ContainerName, "ErrImageNeverPull", "image not present")}}, phase: engine.PhaseFailed, message: "ErrImageNeverPull"},
		{name: "invalid init image", status: corev1.PodStatus{Phase: corev1.PodPending, InitContainerStatuses: []corev1.ContainerStatus{
			waiting("init", "InvalidImageName", "bad")}}, phase: engine.PhaseFailed, message: "container init: InvalidImageName: bad"},
		{name: "oom killed", status: corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{
			terminated(ContainerName, "OOMKilled", 137, "")}}, phase: engine.PhaseFailed,
			message: "engine container terminated: OOMKilled (exit code 137)"},
		{name: "init sql error", status: corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{
			terminated(ContainerName, "Error", 1, "Catalog Error: Table x does not exist")}}, phase: engine.PhaseFailed,
			message: "Error (exit code 1): Catalog Error: Table x does not exist"},
		{name: "exited", status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{
			terminated(ContainerName, "Completed", 0, "")}}, phase: engine.PhaseFailed, message: "Completed (exit code 0)"},
		{name: "evicted", status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted",
			Message: "The node was low on resource: memory."}, phase: engine.PhaseFailed,
			message: "pod failed: Evicted: The node was low on resource: memory."},
		{name: "deadline", status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "DeadlineExceeded",
			Message:           "Pod was active on the node longer than the specified deadline",
			ContainerStatuses: []corev1.ContainerStatus{terminated(ContainerName, "Error", 137, "")}}, phase: engine.PhaseFailed,
			message: "pod failed: DeadlineExceeded: Pod was active on the node longer than the specified deadline; engine container terminated: Error (exit code 137)"},
		{name: "init failed", status: corev1.PodStatus{Phase: corev1.PodFailed, InitContainerStatuses: []corev1.ContainerStatus{
			terminated("init", "Error", 2, "")}}, phase: engine.PhaseFailed, message: "container init terminated: Error (exit code 2)"},
		{name: "failed without details", status: corev1.PodStatus{Phase: corev1.PodFailed}, phase: engine.PhaseFailed, message: "pod failed"},
		{name: "engine died next to a sidecar", status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.1",
			ContainerStatuses: []corev1.ContainerStatus{terminated(ContainerName, "Error", 1, "")}}, phase: engine.PhaseFailed,
			message: "engine container terminated: Error (exit code 1)"},
		{name: "node lost", status: corev1.PodStatus{Phase: corev1.PodUnknown, Reason: "NodeLost"}, phase: engine.PhaseStarting,
			message: "pod status unknown: NodeLost"},
	}
	for _, c := range cases {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Status: c.status}
		if c.deleting {
			pod.DeletionTimestamp = &now
		}
		phase, endpoint, msg := podStatus(pod)
		if phase != c.phase || endpoint != c.endpoint || !strings.Contains(msg, c.message) {
			t.Errorf("%s: got (%s, %q, %q), want (%s, %q, *%q*)", c.name, phase, endpoint, msg, c.phase, c.endpoint, c.message)
		}
		if phase != engine.PhaseReady && endpoint != "" {
			t.Errorf("%s: endpoint %q set although not ready", c.name, endpoint)
		}
	}
}

func TestPodStatusTruncatesMessages(t *testing.T) {
	long := strings.Repeat("ä", 2000)
	pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{
		terminated(ContainerName, "Error", 1, long)}}}
	_, _, msg := podStatus(pod)
	if len(msg) > maxMessageLength || !strings.HasSuffix(msg, "...") || !strings.HasPrefix(msg, "engine container terminated") {
		t.Fatalf("message of %d bytes: %.60q", len(msg), msg)
	}
	if !utf8.ValidString(msg) {
		t.Fatal("truncation split a UTF-8 sequence")
	}
}

func TestInstanceFromPod(t *testing.T) {
	e := newTestEnv(t, testConfig(t, ""))
	created := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	pod, _ := e.enginePod("quack-user-alice-abcde", userKey, false, "t")
	pod.CreationTimestamp = metav1.NewTime(created)
	pod.Status = readyStatus("10.0.0.7")

	inst, err := instanceFromPod(pod)
	if err != nil {
		t.Fatal(err)
	}
	want := engine.Instance{
		ID: "quack-user-alice-abcde", Key: userKey, ProfileName: "default",
		SpecHash: e.p.SpecHash("default", e.p.cfg.Profiles["default"]), Phase: engine.PhaseReady,
		Endpoint: "http://10.0.0.7:9494", User: "alice", CreatedAt: created,
	}
	if inst != want {
		t.Fatalf("instance\n got %+v\nwant %+v", inst, want)
	}

	warmKey := engine.Key{ShareLevel: engine.ShareConnection, Profile: "default"}
	warm, _ := e.enginePod("quack-warm-default-abcde", warmKey, true, "t")
	if inst, err := instanceFromPod(warm); err != nil || !inst.Warm || inst.Key != warmKey || inst.User != "" || inst.Phase != engine.PhaseStarting {
		t.Fatalf("warm instance %+v %v", inst, err)
	}

	for _, ann := range []map[string]string{nil, {AnnotationKey: "garbage"}, {AnnotationKey: "USER/a/b#0"}} {
		bad := pod.DeepCopy()
		bad.Annotations = ann
		if _, err := instanceFromPod(bad); err == nil {
			t.Errorf("annotations %v accepted", ann)
		}
	}
}
