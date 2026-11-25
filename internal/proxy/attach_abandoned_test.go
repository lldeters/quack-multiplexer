package proxy_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"quackmux/internal/engine"
	"quackmux/internal/engine/fake"
	"quackmux/internal/wire"
)

// giveUpConnect sends a handshake and gives up after d, as a DuckDB client
// does when its request timeout runs out (http_timeout, 30s by default, for
// a client that pins ssl_fingerprint; docs/tls.md).
func (h *harness) giveUpConnect(token string, d time.Duration) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL+"/quack", bytes.NewReader(connectMsg(token)))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", wire.ContentType)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
		h.t.Fatalf("the handshake was answered within %s (HTTP %d), while its engine should still start", d, resp.StatusCode)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		h.t.Fatalf("handshake: %v, want the client's own deadline", err)
	}
}

// An ATTACH that its client gives up while the engine starts creates no
// session, and the engine keeps starting. A new ATTACH of a shared key (here
// USER) waits for that engine. With share level CONNECTION the key belongs
// to the session: the new ATTACH starts another engine, and the first one
// ends unused once it is ready (engine.idleTimeout.CONNECTION 0). A client
// whose timeout is shorter than the start of a CONNECTION engine therefore
// never gets through.
func TestAttachGivenUpWhileTheEngineStarts(t *testing.T) {
	for _, tc := range []struct {
		shareLevel string
		launches   int
	}{
		{"user", 1},
		{"connection", 2},
	} {
		t.Run(tc.shareLevel, func(t *testing.T) {
			prov := fake.New()
			prov.StartDelay = time.Second
			h := newHarness(t, "", prov)
			token := "tok?user=alice&share_level=" + tc.shareLevel

			h.giveUpConnect(token, 100*time.Millisecond)
			eventually(t, "the handshake counted as client_gone", func() bool {
				return requestCount(t, h.met, "client_gone") == 1
			})
			if n := h.sessions.Count(); n != 0 {
				t.Fatalf("%d sessions after the abandoned handshake, want none", n)
			}
			views := h.engines.List()
			if len(views) != 1 || views[0].State != engine.StateStarting || views[0].Reserved != 0 {
				t.Fatalf("engines after the abandoned handshake: %+v, want one STARTING without a reservation", views)
			}
			first := views[0].ID

			conn := h.connect(token, nil)
			if got := prov.Launches(); got != tc.launches {
				t.Fatalf("%d launches after the second ATTACH, want %d", got, tc.launches)
			}
			reached := h.sessionEngine(conn)
			if tc.launches == 1 {
				if reached != first {
					t.Fatalf("the second ATTACH reached %s, want the engine %s that was still starting", reached, first)
				}
				return
			}
			if reached == first {
				t.Fatalf("the CONNECTION session reached %s, the engine of the abandoned handshake", first)
			}
			eventually(t, "the unused engine "+first+" to end", func() bool {
				h.engines.Reconcile()
				v, ok := h.engines.Get(first)
				return !ok || v.State == engine.StateTerminating
			})
		})
	}
}
