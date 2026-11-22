package proxy_test

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"quackmux/internal/metrics"
	"quackmux/internal/proxy"
	"quackmux/internal/upstream"
	"quackmux/internal/wire"
)

// lockedBuffer collects log output from concurrent handlers.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// With leader election every healthy replica is ready, so Quack requests can
// reach a follower (a port-forward to a pod, a Service without the leader
// label). The follower answers each one with HTTP 200 and an ERROR_RESPONSE
// without running it, also when it does not read a large body, counts it as
// outcome not_leader and warns once.
func TestFollowerAnswersEveryRequest(t *testing.T) {
	h := newHarness(t, "", nil)
	connID := h.connect("tok?user=alice", nil) // a valid session on the leader

	for _, tc := range []struct {
		reason, outcome string
		warnings        int
	}{
		{proxy.ReasonNotLeader, "not_leader", 1},
		{"starting up", "not_ready", 0},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			met := metrics.New()
			logs := &lockedBuffer{}
			gw := proxy.New(proxy.Options{
				Config: h.cfg, Auth: noneAuth{}, Engines: h.engines, Sessions: h.sessions,
				Upstream: upstream.New(h.cfg.Server.Upstream), Log: slog.New(slog.NewTextHandler(logs, nil)), Metrics: met,
				Ready: func() (bool, string) { return false, tc.reason },
			})
			srv := httptest.NewServer(gw)
			defer srv.Close()

			bigInsert := wire.Header{Type: wire.TypeSendDataRequest, ConnectionID: connID}.Encode()
			bigInsert = append(bigInsert, bytes.Repeat([]byte{0}, 8<<20)...) // never read
			for name, msg := range map[string][]byte{
				"handshake": connectMsg("tok?user=alice"),
				"query":     wire.PrepareRequest{SQL: "SELECT 1", QueryUUID: wire.Hugeint{Upper: 7, Lower: 9}}.Encode(wire.Header{ConnectionID: connID}),
				"upload":    bigInsert,
			} {
				req, err := http.NewRequest(http.MethodPost, srv.URL+"/quack", bytes.NewReader(msg))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Expect", "100-continue") // what curl sends above 1 MiB
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatalf("%s: no answer: %v", name, err)
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("%s: HTTP %d", name, resp.StatusCode)
				}
				e := mustError(t, body, "quackmux: gateway is not ready: "+tc.reason)
				if e.MustInvalidate {
					t.Errorf("%s: must_invalidate set: the client would drop its attachment", name)
				}
			}
			if got := requestCount(t, met, tc.outcome); got != 3 {
				t.Errorf("requests with outcome %s = %v, want 3", tc.outcome, got)
			}
			if n := strings.Count(logs.String(), "does not lead"); n != tc.warnings {
				t.Errorf("logged the follower warning %d times, want %d", n, tc.warnings)
			}
		})
	}
	// Nothing reached the engine: the session still works on the leader.
	mustType(t, h.query(connID, "SELECT 1"), wire.TypePrepareResponse)
}

// requestCount sums quackmux_requests_total for one outcome.
func requestCount(t *testing.T, met *metrics.Metrics, outcome string) float64 {
	t.Helper()
	mfs, err := met.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var n float64
	for _, mf := range mfs {
		if mf.GetName() != "quackmux_requests_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "outcome" && lp.GetValue() == outcome {
					n += m.GetCounter().GetValue()
				}
			}
		}
	}
	return n
}
