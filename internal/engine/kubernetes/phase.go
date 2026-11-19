package kubernetes

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"

	"quackmux/internal/engine"
)

// maxMessageLength bounds instance messages (termination messages may be
// several KiB long).
const maxMessageLength = 1024

// permanentWaitingReasons are container waiting reasons that never resolve
// by themselves: the pod can never start.
var permanentWaitingReasons = map[string]bool{
	"InvalidImageName":  true,
	"ErrImageNeverPull": true,
}

// instanceFromPod translates an engine pod into an Instance (without the
// token, which lives in the pod's secret). It fails for pods whose key
// annotation is missing or does not parse.
func instanceFromPod(pod *corev1.Pod) (engine.Instance, error) {
	raw, ok := pod.Annotations[AnnotationKey]
	if !ok {
		return engine.Instance{}, fmt.Errorf("pod %s has no %s annotation", pod.Name, AnnotationKey)
	}
	key, err := engine.ParseKey(raw)
	if err != nil {
		return engine.Instance{}, fmt.Errorf("pod %s: %w", pod.Name, err)
	}
	profile := pod.Annotations[AnnotationProfile]
	if profile == "" {
		profile = key.Profile
	}
	phase, endpoint, msg := podStatus(pod)
	return engine.Instance{
		ID:          pod.Name,
		Key:         key,
		Warm:        pod.Labels[LabelWarm] == "true",
		ProfileName: profile,
		SpecHash:    pod.Annotations[AnnotationSpecHash],
		Phase:       phase,
		Endpoint:    endpoint,
		User:        pod.Annotations[AnnotationUser],
		CreatedAt:   pod.CreationTimestamp.Time,
		Message:     msg,
	}, nil
}

// podStatus derives the engine phase, the Quack endpoint (only when ready)
// and an explanatory message from a pod.
func podStatus(pod *corev1.Pod) (phase engine.Phase, endpoint, message string) {
	if pod.DeletionTimestamp != nil {
		return engine.PhaseFailed, "", "pod is being deleted"
	}
	st := &pod.Status
	main := containerStatus(st.ContainerStatuses, ContainerName)
	switch st.Phase {
	case corev1.PodSucceeded, corev1.PodFailed:
		return engine.PhaseFailed, "", truncate(failedMessage(pod, main))
	}
	// With restartPolicy Never a terminated engine never comes back, even
	// if sidecars keep the pod running.
	if main != nil && main.State.Terminated != nil {
		return engine.PhaseFailed, "", truncate("engine container terminated: " + describeTermination(main.State.Terminated))
	}
	if cs := waitingContainer(pod, func(reason string) bool { return permanentWaitingReasons[reason] }); cs != nil {
		return engine.PhaseFailed, "", truncate(waitingMessage(cs))
	}
	if st.Phase == corev1.PodRunning {
		if !podReady(pod) {
			return engine.PhaseStarting, "", "not ready"
		}
		if st.PodIP == "" {
			return engine.PhaseStarting, "", "ready, waiting for the pod IP"
		}
		return engine.PhaseReady, engineEndpoint(st.PodIP), ""
	}
	if c := podCondition(pod, corev1.PodScheduled); c != nil && c.Status == corev1.ConditionFalse {
		// For example "Unschedulable: 0/3 nodes are available: 3 Insufficient memory."
		msg := joinNonEmpty(": ", c.Reason, strings.TrimSpace(c.Message))
		if msg == "" {
			msg = "not scheduled yet"
		}
		return engine.PhaseStarting, "", truncate(msg)
	}
	if cs := waitingContainer(pod, func(reason string) bool { return reason != "" }); cs != nil {
		return engine.PhaseStarting, "", truncate(waitingMessage(cs))
	}
	if st.Phase == corev1.PodUnknown {
		return engine.PhaseStarting, "", truncate(joinNonEmpty(": ", "pod status unknown", st.Reason, st.Message))
	}
	return engine.PhaseStarting, "", "pending"
}

// engineEndpoint returns the base URL of the Quack server of a pod.
func engineEndpoint(podIP string) string {
	return "http://" + net.JoinHostPort(podIP, strconv.Itoa(engine.EnginePort))
}

func containerStatus(list []corev1.ContainerStatus, name string) *corev1.ContainerStatus {
	for i := range list {
		if list[i].Name == name {
			return &list[i]
		}
	}
	return nil
}

func podCondition(pod *corev1.Pod, t corev1.PodConditionType) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == t {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

func podReady(pod *corev1.Pod) bool {
	c := podCondition(pod, corev1.PodReady)
	return c != nil && c.Status == corev1.ConditionTrue
}

// waitingContainer returns the first waiting container whose reason
// matches: the engine container first, then init containers, then the
// others.
func waitingContainer(pod *corev1.Pod, match func(reason string) bool) *corev1.ContainerStatus {
	waiting := func(cs *corev1.ContainerStatus) bool {
		return cs != nil && cs.State.Waiting != nil && match(cs.State.Waiting.Reason)
	}
	if cs := containerStatus(pod.Status.ContainerStatuses, ContainerName); waiting(cs) {
		return cs
	}
	for _, list := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for i := range list {
			if waiting(&list[i]) {
				return &list[i]
			}
		}
	}
	return nil
}

// waitingMessage renders "<reason>: <message>", naming the container unless
// it is the engine container.
func waitingMessage(cs *corev1.ContainerStatus) string {
	w := cs.State.Waiting
	msg := joinNonEmpty(": ", w.Reason, strings.TrimSpace(w.Message))
	if cs.Name != ContainerName {
		msg = "container " + cs.Name + ": " + msg
	}
	return msg
}

// failedMessage explains a pod in phase Failed or Succeeded: the pod-level
// reason (Evicted, DeadlineExceeded, ...) and how the engine container (or,
// failing that, another container) terminated.
func failedMessage(pod *corev1.Pod, main *corev1.ContainerStatus) string {
	st := &pod.Status
	var parts []string
	if st.Reason != "" || st.Message != "" {
		parts = append(parts, "pod "+strings.ToLower(string(st.Phase))+": "+joinNonEmpty(": ", st.Reason, strings.TrimSpace(st.Message)))
	}
	if main != nil && main.State.Terminated != nil {
		parts = append(parts, "engine container terminated: "+describeTermination(main.State.Terminated))
	} else if cs := failedContainer(pod); cs != nil {
		parts = append(parts, "container "+cs.Name+" terminated: "+describeTermination(cs.State.Terminated))
	}
	if len(parts) == 0 {
		return "pod " + strings.ToLower(string(st.Phase))
	}
	return strings.Join(parts, "; ")
}

// failedContainer returns the first container (init containers first) that
// terminated with a non-zero exit code.
func failedContainer(pod *corev1.Pod) *corev1.ContainerStatus {
	for _, list := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for i := range list {
			if t := list[i].State.Terminated; t != nil && t.ExitCode != 0 {
				return &list[i]
			}
		}
	}
	return nil
}

// describeTermination renders e.g. "OOMKilled (exit code 137)" or
// "Error (exit code 1): <termination message>".
func describeTermination(t *corev1.ContainerStateTerminated) string {
	var b strings.Builder
	if t.Reason != "" {
		b.WriteString(t.Reason)
		b.WriteByte(' ')
	}
	fmt.Fprintf(&b, "(exit code %d", t.ExitCode)
	if t.Signal != 0 {
		fmt.Fprintf(&b, ", signal %d", t.Signal)
	}
	b.WriteByte(')')
	if m := strings.TrimSpace(t.Message); m != "" {
		b.WriteString(": ")
		b.WriteString(m)
	}
	return b.String()
}

func joinNonEmpty(sep string, parts ...string) string {
	kept := parts[:0:0]
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}

// truncate shortens s to maxMessageLength bytes without splitting a UTF-8
// sequence.
func truncate(s string) string {
	if len(s) <= maxMessageLength {
		return s
	}
	cut := maxMessageLength - len("...")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
