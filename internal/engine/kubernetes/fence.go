package kubernetes

import (
	"context"
	"net/http"

	"k8s.io/client-go/rest"

	"quackmux/internal/engine"
)

// The HA fence of a request (engine.LaunchRequest.Fence) is checked when the
// request is sent, not when the provider decides to send it: client-go's
// rate limiter holds a request back before it reaches the transport, for
// seconds under a burst of launches, and a leader whose lease goes stale
// meanwhile must not create an engine pod afterwards. The provider checks
// the fence before such a call as well (for clients without this
// transport, such as the fake clientset of the tests).

// fenceKey is the context key of a request's fence.
type fenceKey struct{}

// withFence returns ctx for a request that may only be sent while fence
// reports true (nil: no fence).
func withFence(ctx context.Context, fence func() bool) context.Context {
	if fence == nil {
		return ctx
	}
	return context.WithValue(ctx, fenceKey{}, fence)
}

// fenceTransport refuses a request whose context carries a fence that
// reports false, right before the request is written to the connection. The
// error wraps engine.ErrNotLeader; client-go does not retry it (it retries
// only GET requests, and only after connection errors).
type fenceTransport struct{ next http.RoundTripper }

func (t fenceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if fence, ok := req.Context().Value(fenceKey{}).(func() bool); ok && !fence() {
		if req.Body != nil {
			_ = req.Body.Close() // a RoundTripper must close the body, even on errors
		}
		return nil, engine.ErrNotLeader
	}
	return t.next.RoundTrip(req)
}

// fencedConfig returns a copy of rc whose transport checks the fence of
// every request (see withFence).
func fencedConfig(rc *rest.Config) *rest.Config {
	c := rest.CopyConfig(rc)
	c.Wrap(func(rt http.RoundTripper) http.RoundTripper { return fenceTransport{next: rt} })
	return c
}
