package ha

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// reviewer answers the SelfSubjectAccessReviews of a fake clientset with
// decide and records what was asked.
type reviewer struct {
	mu     sync.Mutex
	asked  []authorizationv1.ResourceAttributes
	decide func(authorizationv1.ResourceAttributes) (authorizationv1.SubjectAccessReviewStatus, error)
}

func newReviewer(client *fake.Clientset, decide func(authorizationv1.ResourceAttributes) (authorizationv1.SubjectAccessReviewStatus, error)) *reviewer {
	r := &reviewer{decide: decide}
	client.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		review := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview).DeepCopy()
		attrs := *review.Spec.ResourceAttributes
		r.mu.Lock()
		r.asked = append(r.asked, attrs)
		r.mu.Unlock()
		status, err := r.decide(attrs)
		if err != nil {
			return true, nil, err
		}
		review.Status = status
		return true, review, nil
	})
	return r
}

// allowExcept allows everything but the requests that match one of denied
// ("verb resource namespace").
func allowExcept(denied ...string) func(authorizationv1.ResourceAttributes) (authorizationv1.SubjectAccessReviewStatus, error) {
	return func(a authorizationv1.ResourceAttributes) (authorizationv1.SubjectAccessReviewStatus, error) {
		for _, d := range denied {
			if d == a.Verb+" "+a.Resource+" "+a.Namespace {
				return authorizationv1.SubjectAccessReviewStatus{Allowed: false}, nil
			}
		}
		return authorizationv1.SubjectAccessReviewStatus{Allowed: true}, nil
	}
}

func TestCheckAccess(t *testing.T) {
	t.Run("everything allowed", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		r := newReviewer(client, allowExcept())
		e := newElector(t, client, testPod, shortTimings(), nil)
		if err := e.CheckAccess(context.Background(), "engines"); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, a := range r.asked {
			got = append(got, a.Verb+" "+a.Group+"/"+a.Resource+" "+a.Namespace+"/"+a.Name)
		}
		want := []string{
			"get coordination.k8s.io/leases " + testNamespace + "/" + testLease,
			"create coordination.k8s.io/leases " + testNamespace + "/", // a create has no name yet
			"update coordination.k8s.io/leases " + testNamespace + "/" + testLease,
			"list /pods engines/",
			"watch /pods engines/",
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("asked\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})
	t.Run("no Role in the namespace of the lease", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		newReviewer(client, allowExcept("get leases "+testNamespace, "create leases "+testNamespace, "update leases "+testNamespace))
		e := newElector(t, client, testPod, shortTimings(), nil)
		err := e.CheckAccess(context.Background(), "engines")
		if err == nil {
			t.Fatal("a replica that may not use the lease starts")
		}
		t.Log(err)
		for _, s := range []string{"could never lead", "get leases.coordination.k8s.io " + testNamespace + "/" + testLease,
			"create leases.coordination.k8s.io in namespace " + testNamespace, "rbac.yaml"} {
			if !strings.Contains(err.Error(), s) {
				t.Errorf("the error does not say %q: %v", s, err)
			}
		}
		if strings.Contains(err.Error(), "list pods") || strings.Contains(err.Error(), "watch pods") {
			t.Errorf("the error names allowed requests: %v", err)
		}
	})
	t.Run("engine pods may not be watched", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		newReviewer(client, func(a authorizationv1.ResourceAttributes) (authorizationv1.SubjectAccessReviewStatus, error) {
			if a.Resource == "pods" && a.Verb == "watch" {
				return authorizationv1.SubjectAccessReviewStatus{Allowed: false, Reason: "no RBAC policy matched"}, nil
			}
			return authorizationv1.SubjectAccessReviewStatus{Allowed: true}, nil
		})
		e := newElector(t, client, testPod, shortTimings(), nil)
		err := e.CheckAccess(context.Background(), "engines")
		if err == nil || !strings.Contains(err.Error(), "watch pods in namespace engines (no RBAC policy matched)") ||
			strings.Contains(err.Error(), "list pods") {
			t.Fatalf("CheckAccess = %v", err)
		}
	})
	t.Run("without an engine namespace only the lease is checked", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		r := newReviewer(client, allowExcept("list pods engines"))
		e := newElector(t, client, testPod, shortTimings(), nil)
		if err := e.CheckAccess(context.Background(), " "); err != nil {
			t.Fatal(err)
		}
		if len(r.asked) != 3 {
			t.Fatalf("asked %d reviews, want 3 (the lease)", len(r.asked))
		}
	})
	t.Run("an API server that cannot answer does not stop the start", func(t *testing.T) {
		for name, decide := range map[string]func(authorizationv1.ResourceAttributes) (authorizationv1.SubjectAccessReviewStatus, error){
			"request fails": func(authorizationv1.ResourceAttributes) (authorizationv1.SubjectAccessReviewStatus, error) {
				return authorizationv1.SubjectAccessReviewStatus{}, errors.New("injected: API server unreachable")
			},
			"not evaluated": func(authorizationv1.ResourceAttributes) (authorizationv1.SubjectAccessReviewStatus, error) {
				return authorizationv1.SubjectAccessReviewStatus{EvaluationError: "injected: webhook timed out"}, nil
			},
		} {
			client := fake.NewSimpleClientset()
			newReviewer(client, decide)
			e := newElector(t, client, testPod, shortTimings(), nil)
			if err := e.CheckAccess(context.Background(), "engines"); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
	})
}
