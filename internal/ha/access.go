package ha

import (
	"context"
	"fmt"
	"strings"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// accessCheckTimeout bounds each SelfSubjectAccessReview of CheckAccess.
const accessCheckTimeout = 5 * time.Second

// permission is a request a replica must be allowed to make to lead.
type permission struct {
	verb, group, resource, namespace, name string
}

func (p permission) String() string {
	r := p.resource
	if p.group != "" {
		r += "." + p.group
	}
	if p.name != "" {
		return fmt.Sprintf("%s %s %s/%s", p.verb, r, p.namespace, p.name)
	}
	return fmt.Sprintf("%s %s in namespace %s", p.verb, r, p.namespace)
}

// CheckAccess asks the API server (SelfSubjectAccessReview) whether this
// replica may do what it needs to lead: get, create and update the lease,
// and list and watch the engine pods in engineNamespace, from which a new
// leader syncs its engine registry before it serves ("" skips them). Every
// replica campaigns, and one that may not do all of that would take the
// lease over and never serve, or never take it over. CheckAccess then
// returns an error that names what is denied, and the gateway does not
// start: a rollout of such replicas stalls, and the old ones keep serving.
// If the API server cannot answer, it only logs a warning (the readiness of
// a follower also needs a successful request for the lease, StandbyReady).
func (e *Elector) CheckAccess(ctx context.Context, engineNamespace string) error {
	const leases = "leases"
	const coordination = "coordination.k8s.io"
	need := []permission{
		{verb: "get", group: coordination, resource: leases, namespace: e.namespace, name: e.name},
		// RBAC cannot restrict create to a name: the request has none yet.
		{verb: "create", group: coordination, resource: leases, namespace: e.namespace},
		{verb: "update", group: coordination, resource: leases, namespace: e.namespace, name: e.name},
	}
	if ns := strings.TrimSpace(engineNamespace); ns != "" {
		need = append(need,
			permission{verb: "list", resource: "pods", namespace: ns},
			permission{verb: "watch", resource: "pods", namespace: ns})
	}
	var denied []string
	for _, p := range need {
		allowed, reason, err := e.allowed(ctx, p)
		if err != nil {
			e.log.Warn("could not check the permissions a leader needs; a missing one shows when the replica campaigns "+
				"or leads", "permission", p.String(), "err", err)
			return nil
		}
		if !allowed {
			if reason != "" {
				denied = append(denied, p.String()+" ("+reason+")")
			} else {
				denied = append(denied, p.String())
			}
		}
	}
	if len(denied) == 0 {
		return nil
	}
	return fmt.Errorf("ha: this replica could never lead: the gateway may not %s. The namespace of the lease "+
		"(ha.namespace, else the engine namespace) and the engine namespace (provider.kubernetes.namespace, else the "+
		"gateway's) each need the Role and RoleBinding of deploy/kubernetes/base/rbac.yaml", strings.Join(denied, ", "))
}

// allowed asks the API server whether the gateway may make the request p.
// An answer that could not be evaluated counts as an error, not a denial.
func (e *Elector) allowed(ctx context.Context, p permission) (ok bool, reason string, err error) {
	ctx, cancel := context.WithTimeout(ctx, accessCheckTimeout)
	defer cancel()
	r, err := e.client.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{
			Namespace: p.namespace, Verb: p.verb, Group: p.group, Resource: p.resource, Name: p.name,
		}},
	}, metav1.CreateOptions{})
	if err != nil {
		return false, "", err
	}
	if !r.Status.Allowed && r.Status.EvaluationError != "" {
		return false, "", fmt.Errorf("access review not evaluated: %s", r.Status.EvaluationError)
	}
	return r.Status.Allowed, strings.TrimSpace(r.Status.Reason), nil
}
