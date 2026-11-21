package kubernetes

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"quackmux/internal/engine"
)

// Terminate of an engine without a pod reports it gone: the informer has
// nothing to report for a pod that never existed, and the manager forgets an
// engine only on Gone.
func TestTerminateWithoutPodReportsGone(t *testing.T) {
	cfg := testConfig(t, "")
	e := newTestEnv(t, cfg)
	ctx := e.start(t)
	if err := e.p.Terminate(ctx, "quack-user-alice-nopod", "launch failed"); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, e.p, "Gone for the engine without a pod", func(inst engine.Instance) bool {
		return inst.ID == "quack-user-alice-nopod" && inst.Phase == engine.PhaseGone
	})
}

// A launch whose pod the API server rejects (quota, Pod Security, webhook)
// leaves no engine behind.
func TestRejectedPodCreateLeavesNoEngine(t *testing.T) {
	cfg := testConfig(t, "")
	e := newTestEnv(t, cfg)
	e.client.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "x",
			errors.New("exceeded quota: compute, requested: requests.cpu=1"))
	})
	m := engine.NewManager(cfg, e.p, slog.New(slog.DiscardHandler), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := m.Acquire(ctx, userKey, "alice"); err == nil {
			t.Fatal("Acquire succeeded although the pod was rejected")
		}
	}
	waitFor(t, "rejected engines removed", func() bool { return len(m.List()) == 0 })
}
