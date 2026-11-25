package proxy_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"quackmux/internal/auth"
	"quackmux/internal/config"
	"quackmux/internal/engine"
	"quackmux/internal/engine/fake"
	"quackmux/internal/metrics"
	"quackmux/internal/proxy"
	"quackmux/internal/session"
	"quackmux/internal/upstream"
	"quackmux/internal/wire"
)

// The cost of a proxied request, measured with an engine that answers every
// message the same way, whatever the gateway's version: with the query time
// limit off, the gateway's request path must cost what it did before the
// limit existed (compare the benchmark with an older commit).

// benchEngine answers PREPARE with an empty result, FETCH with the end of
// the result, the handshake with a connection, and anything else with
// SUCCESS. It allocates the same for every request.
func benchEngine(tb testing.TB) *httptest.Server {
	tb.Helper()
	conn := wire.ConnectionResponse{ServerDuckDBVersion: "v2.0.0", ServerPlatform: "bench", QuackVersion: wire.ProtocolVersion,
		HeartbeatTimeoutSeconds: 3600}.Encode(wire.Header{ConnectionID: "0123456789abcdef0123456789abcdef"})
	prepare := append(wire.Header{Type: wire.TypePrepareResponse}.Encode(), 0x05, 0x00, 0x07, 0x09, 0xff, 0xff)
	fetch := wire.EncodeEmpty(wire.Header{Type: wire.TypeFetchResponse})
	success := wire.EncodeEmpty(wire.Header{Type: wire.TypeSuccessResponse})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var head [3]byte
		_, _ = io.ReadFull(r.Body, head[:])
		_, _ = io.Copy(io.Discard, r.Body)
		answer := success
		switch wire.MessageType(head[2]) {
		case wire.TypeConnectionRequest:
			answer = conn
		case wire.TypePrepareRequest:
			answer = prepare
		case wire.TypeFetchRequest:
			answer = fetch
		}
		w.Header()["Content-Type"] = []string{wire.ContentType}
		_, _ = w.Write(answer)
	}))
	tb.Cleanup(srv.Close)
	return srv
}

type benchAuth struct{}

func (benchAuth) Authenticate(context.Context, auth.Request) (auth.Identity, error) {
	return auth.Identity{User: "alice", Method: "test"}, nil
}

func (benchAuth) Close() error { return nil }

// benchGateway builds a gateway with the given configuration whose engine
// requests all reach benchEngine, and opens a session.
func benchGateway(tb testing.TB, yamlExtra string) (*proxy.Gateway, string) {
	tb.Helper()
	cfg, err := config.Parse([]byte(baseConfig + yamlExtra))
	if err != nil {
		tb.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	met := metrics.New()
	up := upstream.New(cfg.Server.Upstream)
	engineAddr := benchEngine(tb).Listener.Addr().String()
	var d net.Dialer
	up.HTTP.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return d.DialContext(ctx, network, engineAddr)
	}
	sealer, err := session.NewSealer(testKey)
	if err != nil {
		tb.Fatal(err)
	}
	prov := fake.New()
	engines := engine.NewManager(cfg, prov, log, met)
	sessions := session.NewManager(cfg, engines, up, sealer, log, met)
	engines.SetSessionCloser(sessions)
	ctx, cancel := context.WithCancel(context.Background())
	tb.Cleanup(cancel)
	if err := engines.Start(ctx); err != nil {
		tb.Fatal(err)
	}
	gw := proxy.New(proxy.Options{Config: cfg, Auth: benchAuth{}, Engines: engines, Sessions: sessions, Upstream: up,
		Log: log, Metrics: met})
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/quack", bytes.NewReader(connectMsg("tok"))))
	hdr, _, err := wire.DecodeConnectionResponse(rec.Body.Bytes())
	if err != nil {
		tb.Fatalf("connect: %v", err)
	}
	return gw, hdr.ConnectionID
}

// benchRequests are the messages the benchmark sends, by name.
func benchRequests(connID string) map[string][]byte {
	u := wire.Hugeint{Upper: 7, Lower: 9}
	return map[string][]byte{
		"PREPARE": wire.PrepareRequest{SQL: "SELECT 1", QueryUUID: u}.Encode(wire.Header{ConnectionID: connID}),
		"FETCH":   wire.FetchRequest{QueryUUID: u, BatchIndex: 1}.Encode(wire.Header{ConnectionID: connID}),
	}
}

func serveOnce(tb testing.TB, gw *proxy.Gateway, msg []byte) {
	req := httptest.NewRequest(http.MethodPost, "/quack", bytes.NewReader(msg))
	req.Header.Set("Content-Type", wire.ContentType)
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() < 3 || rec.Body.Bytes()[2] == byte(wire.TypeErrorResponse) {
		tb.Fatalf("HTTP %d, answer %x", rec.Code, rec.Body.Bytes())
	}
}

// benchLimits are the configurations the request is measured with.
var benchLimits = []struct{ name, yaml string }{{"limit=off", ""}, {"limit=on", "limits: {queryTimeout: 1h}\n"}}

// BenchmarkRequest measures a PREPARE and a FETCH through the gateway, with
// the query time limit off (as before the limit existed) and on.
func BenchmarkRequest(b *testing.B) {
	for _, limit := range benchLimits {
		b.Run(limit.name, func(b *testing.B) {
			gw, c := benchGateway(b, limit.yaml)
			for _, name := range []string{"PREPARE", "FETCH"} {
				msg := benchRequests(c)[name]
				b.Run(name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						serveOnce(b, gw, msg)
					}
				})
			}
		})
	}
}

// TestRequestAllocations reports the allocations of a request without and
// with a query time limit (go test -run TestRequestAllocations -v).
func TestRequestAllocations(t *testing.T) {
	for _, limit := range benchLimits {
		t.Run(limit.name, func(t *testing.T) {
			gw, c := benchGateway(t, limit.yaml)
			for _, name := range []string{"PREPARE", "FETCH"} {
				msg := benchRequests(c)[name]
				for i := 0; i < 200; i++ {
					serveOnce(t, gw, msg)
				}
				t.Logf("%s: %.1f allocations per request", name, testing.AllocsPerRun(2000, func() { serveOnce(t, gw, msg) }))
			}
		})
	}
}
