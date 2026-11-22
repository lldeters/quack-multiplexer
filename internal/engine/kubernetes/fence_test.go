package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"quackmux/internal/config"
	"quackmux/internal/engine"
)

// gateLimiter is a client-go rate limiter that holds every request until the
// test lets it pass, like a token bucket that a burst of launches emptied.
type gateLimiter struct {
	waiting chan struct{} // a request waits for a token
	pass    chan struct{} // a token for one request
}

func newGateLimiter() *gateLimiter {
	return &gateLimiter{waiting: make(chan struct{}, 16), pass: make(chan struct{})}
}

func (l *gateLimiter) TryAccept() bool { return false }
func (l *gateLimiter) Stop()           {}
func (l *gateLimiter) QPS() float32    { return 1 }
func (l *gateLimiter) Accept()         { <-l.pass }

func (l *gateLimiter) Wait(ctx context.Context) error {
	l.waiting <- struct{}{}
	select {
	case <-l.pass:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// fakeAPIServer answers pod creates and claim deletes and counts them.
type fakeAPIServer struct {
	*httptest.Server
	creates, deletes atomic.Int32
}

func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	s := &fakeAPIServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pods"):
			// The body may be protobuf; the answer is JSON, which the client accepts.
			s.creates.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(&corev1.Pod{
				TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
				ObjectMeta: metav1.ObjectMeta{Name: "created", Namespace: testNamespace},
			})
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/persistentvolumeclaims/"):
			s.deletes.Add(1)
			_ = json.NewEncoder(w).Encode(&metav1.Status{Status: metav1.StatusSuccess})
		default:
			http.Error(w, "unexpected request "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// A launch whose lease goes stale while client-go's rate limiter holds back
// the pod create (the provider's own check before the call passed) must not
// create the pod: the fence is checked again when the request is sent.
// Without that, a leader that lost its lease during a burst of launches
// would create pods after another replica took over.
func TestFenceCheckedWhenTheRequestIsSent(t *testing.T) {
	api := newFakeAPIServer(t)
	lim := newGateLimiter()
	client, err := kclient.NewForConfig(fencedConfig(&rest.Config{Host: api.URL, RateLimiter: lim}))
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, persistentYAML)
	p, err := New(cfg, client, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	pod := p.pod(podInput{Name: "quack-user-alice-abcde", Key: personalKey, User: "alice", ProfileName: "personal",
		Profile: cfg.Profiles["personal"]})

	var fresh atomic.Bool
	fresh.Store(true)
	create := func() <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := p.createPod(context.Background(), pod, fresh.Load)
			done <- err
		}()
		return done
	}
	done := create()
	<-lim.waiting      // past the provider's check, waiting for a token
	fresh.Store(false) // meanwhile the lease goes stale
	lim.pass <- struct{}{}
	if err := <-done; !errors.Is(err, engine.ErrNotLeader) || err.Error() != engine.ErrNotLeader.Error() {
		t.Fatalf("create after the lease went stale: %v", err)
	}
	if n := api.creates.Load(); n != 0 {
		t.Fatalf("the API server received %d pod creates sent after the lease went stale", n)
	}

	fresh.Store(true)
	done = create()
	<-lim.waiting
	lim.pass <- struct{}{}
	if err := <-done; err != nil {
		t.Fatalf("create with a fresh lease: %v", err)
	}
	if n := api.creates.Load(); n != 1 {
		t.Fatalf("%d pod creates with a fresh lease, want 1", n)
	}

	// Requests without a fence are never held back by it.
	plain := make(chan error, 1)
	go func() {
		_, err := client.CoreV1().Pods(testNamespace).Create(context.Background(), pod, metav1.CreateOptions{})
		plain <- err
	}()
	<-lim.waiting
	fresh.Store(false)
	lim.pass <- struct{}{}
	if err := <-plain; err != nil {
		t.Fatalf("create without a fence: %v", err)
	}
}

// The same holds for the deletion of a data volume claim.
func TestDeleteVolumeFenceCheckedWhenTheRequestIsSent(t *testing.T) {
	api := newFakeAPIServer(t)
	lim := newGateLimiter()
	client, err := kclient.NewForConfig(fencedConfig(&rest.Config{Host: api.URL, RateLimiter: lim}))
	if err != nil {
		t.Fatal(err)
	}
	var fresh atomic.Bool
	fresh.Store(true)
	done := make(chan error, 1)
	go func() {
		done <- client.CoreV1().PersistentVolumeClaims(testNamespace).Delete(withFence(context.Background(), fresh.Load),
			"quackmux-data-x", metav1.DeleteOptions{})
	}()
	<-lim.waiting
	fresh.Store(false)
	lim.pass <- struct{}{}
	if err := <-done; !errors.Is(err, engine.ErrNotLeader) {
		t.Fatalf("delete after the lease went stale: %v", err)
	}
	if n := api.deletes.Load(); n != 0 {
		t.Fatalf("the API server received %d claim deletes sent after the lease went stale", n)
	}
}

// Leader election gets a client with a rate limiter of its own, so that a
// burst of engine launches cannot delay the renewals of the lease. Within
// one clientset, all API groups share one token bucket (QPS > 0).
func TestLeaderElectionClientHasItsOwnRateLimiter(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "http://127.0.0.1:1"}}]
users: [{name: u, user: {}}]
contexts: [{name: x, context: {cluster: c, user: u}}]
current-context: x
`), 0o600); err != nil {
		t.Fatal(err)
	}
	engines, rc, err := NewClient(config.KubernetesProviderConfig{Kubeconfig: kubeconfig, QPS: 20, Burst: 40})
	if err != nil {
		t.Fatal(err)
	}
	election, err := NewClientForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	launches := engines.CoreV1().RESTClient().GetRateLimiter()
	if launches == nil || launches != engines.CoordinationV1().RESTClient().GetRateLimiter() {
		t.Fatal("expected one rate limiter per clientset")
	}
	if renewals := election.CoordinationV1().RESTClient().GetRateLimiter(); renewals == nil || renewals == launches {
		t.Fatal("lease renewals share the rate limiter of the engine launches")
	}
	if rc.WrapTransport != nil {
		t.Error("the rest.Config for further clients carries the provider's fence transport")
	}
}

// A request that the fence refuses never reaches the transport below.
func TestFenceTransport(t *testing.T) {
	var sent atomic.Int32
	rt := fenceTransport{next: roundTripFunc(func(*http.Request) (*http.Response, error) {
		sent.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, c := range []struct {
		ctx  context.Context
		sent bool
	}{
		{ctx, true},
		{withFence(ctx, nil), true},
		{withFence(ctx, func() bool { return true }), true},
		{withFence(ctx, func() bool { return false }), false},
	} {
		before := sent.Load()
		req, _ := http.NewRequestWithContext(c.ctx, http.MethodPost, "http://api/x", strings.NewReader("{}"))
		resp, err := rt.RoundTrip(req)
		if got := sent.Load() > before; got != c.sent {
			t.Errorf("sent %v, want %v (err %v)", got, c.sent, err)
		}
		if !c.sent && !errors.Is(err, engine.ErrNotLeader) {
			t.Errorf("refused request: %v", err)
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
