package process

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"quackmux/internal/config"
	"quackmux/internal/engine"
	"quackmux/internal/metrics"
)

// The tests run the test binary itself as the engine process (the
// TestHelperProcess pattern of os/exec): it behaves like the DuckDB CLI as far
// as the provider can tell. It checks its command line, reads the SQL from
// stdin, serves GET / on the address of the quack_serve statement and exits
// at end of input. Markers in the profile's init SQL change its behavior:
//
//	-- helper:fail               write output and an error, exit 1 (like -bail)
//	-- helper:hang               never start serving
//	-- helper:ignore-eof         keep running after end of input
//	-- helper:crash-after-ready  exit 5 shortly after serving started
//
// GET /received answers what the fake engine saw (arguments, environment,
// working directory, script).

const helperEnv = "GO_WANT_HELPER_PROCESS"

// fakeBanner mimics the stock Quack server.
const fakeBanner = "This is a DuckDB Quack RPC endpoint. Use ATTACH 'quack:...' to connect here.\n"

func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		return
	}
	os.Exit(fakeEngine())
}

// helperCommand is the CommandFunc of the tests.
func helperCommand(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestHelperProcess$", "--", name}, args...)...)
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	return cmd
}

var serveRE = regexp.MustCompile(`^CALL quack_serve\('quack:([^']+)', token = '([^']*)', disable_ssl = true, allow_other_hostname = (true|false)\);$`)

type received struct {
	Args   []string `json:"args"`
	Env    []string `json:"env"`
	Dir    string   `json:"dir"`
	Script string   `json:"script"`
}

func fakeEngine() int {
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	if len(args) != 3 || args[1] != "-bail" {
		fmt.Fprintf(os.Stderr, "fake engine: unexpected command line %q\n", args)
		return 2
	}
	token := os.Getenv(engine.EnvToken)
	if token == "" {
		fmt.Fprintln(os.Stderr, "fake engine: QUACK_TOKEN is not set")
		return 2
	}

	var mu sync.Mutex
	var script strings.Builder
	snapshot := func() received {
		mu.Lock()
		defer mu.Unlock()
		dir, _ := os.Getwd()
		return received{Args: os.Args, Env: os.Environ(), Dir: dir, Script: script.String()}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, fakeBanner)
	})
	mux.HandleFunc("GET /received", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(snapshot())
	})

	var srv *http.Server
	in := bufio.NewReader(os.Stdin)
	prev, ignoreEOF := "", false
	for {
		raw, err := in.ReadString('\n')
		if raw != "" {
			mu.Lock()
			script.WriteString(raw)
			mu.Unlock()
			line := strings.TrimRight(raw, "\r\n")
			switch {
			case strings.Contains(line, "helper:fail"):
				fmt.Fprintln(os.Stdout, strings.Repeat("x", 3000))
				fmt.Fprintf(os.Stderr, "Error: fake failure, token %s\n", token)
				return 1
			case strings.Contains(line, "helper:ignore-eof"):
				ignoreEOF = true
			case strings.HasPrefix(line, "CALL quack_serve("):
				m := serveRE.FindStringSubmatch(line)
				switch {
				case m == nil:
					fmt.Fprintln(os.Stderr, "fake engine: malformed quack_serve statement")
					return 3
				case prev != ".output "+os.DevNull:
					fmt.Fprintf(os.Stderr, "fake engine: quack_serve not preceded by .output %s\n", os.DevNull)
					return 3
				case m[2] != token:
					fmt.Fprintln(os.Stderr, "fake engine: quack_serve token differs from QUACK_TOKEN")
					return 3
				}
				mu.Lock()
				hang := strings.Contains(script.String(), "helper:hang")
				crash := strings.Contains(script.String(), "helper:crash-after-ready")
				mu.Unlock()
				if hang {
					break
				}
				l, err := net.Listen("tcp", m[1])
				if err != nil {
					fmt.Fprintln(os.Stderr, "fake engine:", err)
					return 4
				}
				srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
				go func() { _ = srv.Serve(l) }()
				if crash {
					go func() {
						time.Sleep(300 * time.Millisecond)
						fmt.Fprintln(os.Stderr, "fake engine: crashing")
						os.Exit(5)
					}()
				}
			}
			prev = line
		}
		if err != nil {
			break // end of input
		}
	}
	if ignoreEOF {
		select {}
	}
	if srv != nil {
		_ = srv.Close()
	}
	return 0
}

// recorder collects the events of a provider.
type recorder struct {
	mu     sync.Mutex
	events []engine.Instance
	closed bool
}

func record(p *Provider) *recorder {
	r := &recorder{}
	go func() {
		for ev := range p.Events() {
			r.mu.Lock()
			r.events = append(r.events, ev)
			r.mu.Unlock()
		}
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
	}()
	return r
}

// wait returns the first event of id with the given phase.
func (r *recorder) wait(t *testing.T, id string, phase engine.Phase) engine.Instance {
	t.Helper()
	var found engine.Instance
	eventually(t, fmt.Sprintf("%s event for %s", phase, id), func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, ev := range r.events {
			if ev.ID == id && ev.Phase == phase {
				found = ev
				return true
			}
		}
		return false
	})
	return found
}

func (r *recorder) phases(id string) []engine.Phase {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []engine.Phase
	for _, ev := range r.events {
		if ev.ID == id {
			out = append(out, ev.Phase)
		}
	}
	return out
}

func (r *recorder) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// freePortRange finds n consecutive ports on 127.0.0.1 that can be bound.
func freePortRange(t *testing.T, n int) (int, int) {
	t.Helper()
	for attempt := 0; attempt < 100; attempt++ {
		start := 20000 + mrand.IntN(25000)
		ok := true
		for port := start; port < start+n && ok; port++ {
			ok = portFree("127.0.0.1", port)
		}
		if ok {
			return start, start + n - 1
		}
	}
	t.Fatal("no free port range found")
	return 0, 0
}

func testConfig(t *testing.T, ports int) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte("auth: {mode: none}\nprovider: {type: process}\n"))
	if err != nil {
		t.Fatal(err)
	}
	start, end := freePortRange(t, ports)
	cfg.Provider.Process = config.ProcessProviderConfig{
		DuckDBPath:     "fake-duckdb",
		WorkDir:        filepath.Join(t.TempDir(), "engines"),
		BindHost:       "127.0.0.1",
		PortRangeStart: start,
		PortRangeEnd:   end,
	}
	return cfg
}

type harness struct {
	p      *Provider
	cfg    *config.Config
	rec    *recorder
	cancel context.CancelFunc
	runErr chan error
}

func newHarness(t *testing.T, ports int, opts ...Option) *harness {
	t.Helper()
	cfg := testConfig(t, ports)
	opts = append([]Option{WithCommand(helperCommand), WithProbeInterval(20 * time.Millisecond), WithStopTimeout(5 * time.Second)}, opts...)
	p, err := New(cfg, slog.New(slog.DiscardHandler), opts...)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{p: p, cfg: cfg, cancel: cancel, runErr: make(chan error, 1)}
	go func() { h.runErr <- p.Run(ctx) }()
	h.rec = record(p)
	t.Cleanup(func() { h.stop(t) })
	return h
}

// stop shuts the provider down and waits for Run (idempotent).
func (h *harness) stop(t *testing.T) {
	t.Helper()
	h.cancel()
	select {
	case err, ok := <-h.runErr:
		if ok && err != nil {
			t.Errorf("Run: %v", err)
		}
		if ok {
			close(h.runErr)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func newToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func (h *harness) request(t *testing.T, prof config.Profile) engine.LaunchRequest {
	t.Helper()
	if prof.Database == "" {
		prof.Database = ":memory:"
	}
	key := engine.Key{ShareLevel: engine.ShareUser, Owner: "alice", Subdomain: "default", Profile: "default"}
	return engine.LaunchRequest{
		ID: h.p.NewID(key, false), Key: key, User: "alice", ProfileName: "default", Profile: prof,
		Token: newToken(t), SpecHash: h.p.SpecHash("default", prof),
	}
}

func (h *harness) launch(t *testing.T, req engine.LaunchRequest) engine.Instance {
	t.Helper()
	inst, err := h.p.Launch(context.Background(), req)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	return inst
}

func (h *harness) terminate(t *testing.T, id, reason string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.p.Terminate(ctx, id, reason); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
}

func get(url string) (int, string, error) {
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Get(url)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), err
}

func fetchReceived(t *testing.T, endpoint string) received {
	t.Helper()
	var rcv received
	eventually(t, "the complete script at the fake engine", func() bool {
		code, body, err := get(endpoint + "/received")
		if err != nil || code != http.StatusOK {
			return false
		}
		if err := json.Unmarshal([]byte(body), &rcv); err != nil {
			t.Fatalf("decode /received: %v", err)
		}
		return strings.HasSuffix(rcv.Script, ".output stdout\n")
	})
	return rcv
}

// sameDir compares directories by identity (paths may differ in case or by
// symbolic links, e.g. /var and /private/var on macOS).
func sameDir(a, b string) bool {
	fa, errA := os.Stat(a)
	fb, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(fa, fb)
}

func envValue(env []string, name string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, name) {
			return v, true
		}
	}
	return "", false
}

func TestEngineLifecycle(t *testing.T) {
	t.Setenv("QUACKMUX_SESSION_KEY", "gateway-secret")
	h := newHarness(t, 4)
	prof := config.Profile{
		Database: ":memory:",
		InitSQL:  "SELECT 42",
		Settings: map[string]string{"threads": "2"},
		Env: []corev1.EnvVar{
			{Name: "DUCKDB_TEST_VAR", Value: "1"},
			{Name: "FROM_SECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{Key: "k"}}},
			{Name: "QUACKMUX_OWNER", Value: "mallory"},
		},
	}
	req := h.request(t, prof)

	inst := h.launch(t, req)
	if inst.Phase != engine.PhaseStarting || inst.Endpoint != "" || inst.ID != req.ID || inst.Token != req.Token ||
		inst.Key != req.Key || inst.User != "alice" || inst.SpecHash != req.SpecHash || inst.CreatedAt.IsZero() {
		t.Fatalf("Launch returned %+v", inst)
	}
	ready := h.rec.wait(t, req.ID, engine.PhaseReady)
	if !strings.HasPrefix(ready.Endpoint, "http://127.0.0.1:") {
		t.Fatalf("endpoint %q", ready.Endpoint)
	}
	port := strings.TrimPrefix(ready.Endpoint, "http://127.0.0.1:")
	if code, body, err := get(ready.Endpoint + "/"); err != nil || code != http.StatusOK || body != fakeBanner {
		t.Fatalf("GET /: %d %q %v", code, body, err)
	}

	rcv := fetchReceived(t, ready.Endpoint)
	wantScript := "SET threads = '2';\nSELECT 42;\n.output " + os.DevNull + "\n" +
		"CALL quack_serve('quack:127.0.0.1:" + port + "', token = '" + req.Token +
		"', disable_ssl = true, allow_other_hostname = false);\n.output stdout\n"
	if rcv.Script != wantScript {
		t.Fatalf("script:\n%s\nwant:\n%s", rcv.Script, wantScript)
	}
	if args := rcv.Args[slices.Index(rcv.Args, "--")+1:]; !slices.Equal(args, []string{"fake-duckdb", "-bail", ":memory:"}) {
		t.Fatalf("engine command line %q", args)
	}
	scratch := filepath.Join(h.cfg.Provider.Process.WorkDir, req.ID)
	if !sameDir(rcv.Dir, scratch) {
		t.Fatalf("engine runs in %q, want %q", rcv.Dir, scratch)
	}
	want := map[string]string{
		engine.EnvToken: req.Token, engine.EnvDatabase: ":memory:", engine.EnvPort: port,
		engine.EnvEngineID: req.ID, engine.EnvShareLevel: "USER", engine.EnvOwner: "alice",
		engine.EnvSubdomain: "default", engine.EnvProfile: "default", "DUCKDB_TEST_VAR": "1",
	}
	for name, v := range want {
		if got, ok := envValue(rcv.Env, name); !ok || got != v {
			t.Errorf("engine env %s = %q (set %v), want %q", name, got, ok, v)
		}
	}
	for _, name := range []string{engine.EnvInitSQL, "QUACKMUX_SESSION_KEY", "FROM_SECRET"} {
		if v, ok := envValue(rcv.Env, name); ok {
			t.Errorf("engine env must not contain %s (= %q)", name, v)
		}
	}
	var withToken int
	for _, kv := range rcv.Env {
		if strings.Contains(kv, req.Token) {
			withToken++
		}
	}
	if withToken != 1 {
		t.Errorf("the token appears in %d environment entries, want 1 (QUACK_TOKEN)", withToken)
	}

	list, err := h.p.List(context.Background())
	if err != nil || len(list) != 1 || list[0].Phase != engine.PhaseReady || list[0].Token != req.Token ||
		list[0].Endpoint != ready.Endpoint || !strings.Contains(list[0].Message, "port "+port) {
		t.Fatalf("List: %+v %v", list, err)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Fatalf("scratch directory: %v", err)
	}

	h.terminate(t, req.ID, "idle for 30m0s")
	gone := h.rec.wait(t, req.ID, engine.PhaseGone)
	if gone.Message != "idle for 30m0s" {
		t.Fatalf("gone message %q", gone.Message)
	}
	if got := h.rec.phases(req.ID); !slices.Equal(got, []engine.Phase{engine.PhaseReady, engine.PhaseGone}) {
		t.Fatalf("events %v", got)
	}
	if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scratch directory still exists: %v", err)
	}
	if _, _, err := get(ready.Endpoint + "/"); err == nil {
		t.Fatal("engine still answers after Terminate")
	}
	if list, _ := h.p.List(context.Background()); len(list) != 0 {
		t.Fatalf("List after Terminate: %+v", list)
	}
	// Terminating again is fine.
	h.terminate(t, req.ID, "again")
}

func TestTokenNeverOnCommandLine(t *testing.T) {
	var mu sync.Mutex
	var cmds []*exec.Cmd
	capture := func(name string, args ...string) *exec.Cmd {
		cmd := helperCommand(name, args...)
		mu.Lock()
		cmds = append(cmds, cmd)
		mu.Unlock()
		return cmd
	}
	h := newHarness(t, 2, WithCommand(capture))
	req := h.request(t, config.Profile{InitSQL: "SELECT 1"})
	h.launch(t, req)
	// The fake engine only serves if the token of the quack_serve statement
	// (read from stdin) equals QUACK_TOKEN.
	h.rec.wait(t, req.ID, engine.PhaseReady)

	mu.Lock()
	defer mu.Unlock()
	if len(cmds) != 1 {
		t.Fatalf("%d commands created", len(cmds))
	}
	cmd := cmds[0]
	if strings.Contains(strings.Join(cmd.Args, " "), req.Token) || strings.Contains(cmd.Path, req.Token) {
		t.Fatalf("token on the command line: %q", cmd.Args)
	}
	if got := cmd.Args[len(cmd.Args)-2:]; !slices.Equal(got, []string{"-bail", ":memory:"}) {
		t.Fatalf("command line ends with %q", got)
	}
	var tokenEnv []string
	for _, kv := range cmd.Env {
		if strings.Contains(kv, req.Token) {
			tokenEnv = append(tokenEnv, kv)
		}
	}
	if !slices.Equal(tokenEnv, []string{engine.EnvToken + "=" + req.Token}) {
		t.Fatalf("token in environment entries %q", tokenEnv)
	}
}

func TestFailureReportsOutputTail(t *testing.T) {
	h := newHarness(t, 2)
	req := h.request(t, config.Profile{InitSQL: "-- helper:fail"})
	h.launch(t, req)
	failed := h.rec.wait(t, req.ID, engine.PhaseFailed)
	msg := failed.Message
	for _, want := range []string{"fake-duckdb exited before the Quack server was ready", "exit status 1", "Error: fake failure, token " + redactedToken} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, req.Token) || strings.Contains(msg, req.Token[:16]) {
		t.Fatal("message reveals the token")
	}
	if len(msg) > outputTailBytes+200 {
		t.Fatalf("message has %d bytes, the output tail must be bounded", len(msg))
	}
	if got := h.rec.phases(req.ID); !slices.Equal(got, []engine.Phase{engine.PhaseFailed}) {
		t.Fatalf("events %v", got)
	}
	// The failed engine stays (with its directory) until it is terminated.
	scratch := filepath.Join(h.cfg.Provider.Process.WorkDir, req.ID)
	if list, _ := h.p.List(context.Background()); len(list) != 1 || list[0].Phase != engine.PhaseFailed || list[0].Message != msg {
		t.Fatalf("List: %+v", list)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Fatalf("scratch directory of a failed engine: %v", err)
	}
	h.terminate(t, req.ID, "failed: test")
	h.rec.wait(t, req.ID, engine.PhaseGone)
	if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scratch directory still exists: %v", err)
	}
}

func TestCrashAfterReadyFails(t *testing.T) {
	h := newHarness(t, 2)
	req := h.request(t, config.Profile{InitSQL: "-- helper:crash-after-ready"})
	h.launch(t, req)
	h.rec.wait(t, req.ID, engine.PhaseReady)
	failed := h.rec.wait(t, req.ID, engine.PhaseFailed)
	if !strings.Contains(failed.Message, "exited unexpectedly (exit status 5): fake engine: crashing") {
		t.Fatalf("message %q", failed.Message)
	}
	h.terminate(t, req.ID, "failed")
	h.rec.wait(t, req.ID, engine.PhaseGone)
	if got := h.rec.phases(req.ID); !slices.Equal(got, []engine.Phase{engine.PhaseReady, engine.PhaseFailed, engine.PhaseGone}) {
		t.Fatalf("events %v", got)
	}
}

func TestTerminateWhileStarting(t *testing.T) {
	h := newHarness(t, 2)
	req := h.request(t, config.Profile{InitSQL: "-- helper:hang"})
	h.launch(t, req)
	time.Sleep(200 * time.Millisecond) // let it run for a few probes
	h.terminate(t, req.ID, "startup timeout")
	h.rec.wait(t, req.ID, engine.PhaseGone)
	if got := h.rec.phases(req.ID); !slices.Equal(got, []engine.Phase{engine.PhaseGone}) {
		t.Fatalf("events %v", got)
	}
}

func TestTerminateKillsEngineThatIgnoresEndOfInput(t *testing.T) {
	h := newHarness(t, 1, WithStopTimeout(300*time.Millisecond))
	req := h.request(t, config.Profile{InitSQL: "-- helper:ignore-eof"})
	h.launch(t, req)
	ready := h.rec.wait(t, req.ID, engine.PhaseReady)
	start := time.Now()
	h.terminate(t, req.ID, "admin")
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Fatalf("Terminate returned after %s, before the stop timeout", d)
	}
	if _, _, err := get(ready.Endpoint + "/"); err == nil {
		t.Fatal("engine still answers after it was killed")
	}
	// The only port of the range is free again.
	req2 := h.request(t, config.Profile{})
	h.launch(t, req2)
	if r2 := h.rec.wait(t, req2.ID, engine.PhaseReady); r2.Endpoint != ready.Endpoint {
		t.Fatalf("endpoint %q, want the released %q", r2.Endpoint, ready.Endpoint)
	}
}

func TestPortExhaustion(t *testing.T) {
	h := newHarness(t, 1)
	first := h.request(t, config.Profile{})
	h.launch(t, first)
	ready := h.rec.wait(t, first.ID, engine.PhaseReady)

	_, err := h.p.Launch(context.Background(), h.request(t, config.Profile{}))
	if err == nil || !strings.Contains(err.Error(), "no free TCP port") {
		t.Fatalf("second launch: %v", err)
	}
	h.terminate(t, first.ID, "done")
	second := h.request(t, config.Profile{})
	h.launch(t, second)
	h.rec.wait(t, second.ID, engine.PhaseReady)
	h.terminate(t, second.ID, "done")

	// A port another process holds is skipped as well.
	l, err := net.Listen("tcp", strings.TrimPrefix(ready.Endpoint, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := h.p.Launch(context.Background(), h.request(t, config.Profile{})); err == nil || !strings.Contains(err.Error(), "no free TCP port") {
		t.Fatalf("launch with the port taken: %v", err)
	}
}

func TestLaunchErrorLeavesNothingBehind(t *testing.T) {
	var calls atomic.Int32
	firstBroken := func(name string, args ...string) *exec.Cmd {
		if calls.Add(1) == 1 {
			return exec.Command(filepath.Join(os.TempDir(), "quackmux-no-such-binary-"+strconv.Itoa(mrand.Int())))
		}
		return helperCommand(name, args...)
	}
	h := newHarness(t, 1, WithCommand(firstBroken))
	req := h.request(t, config.Profile{})
	if _, err := h.p.Launch(context.Background(), req); err == nil {
		t.Fatal("Launch must fail")
	}
	if list, _ := h.p.List(context.Background()); len(list) != 0 {
		t.Fatalf("List: %+v", list)
	}
	entries, err := os.ReadDir(h.cfg.Provider.Process.WorkDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("work directory: %v %v", entries, err)
	}
	if got := h.rec.phases(req.ID); len(got) != 0 {
		t.Fatalf("a failed launch must not emit events, got %v", got)
	}
	// The manager terminates engines whose launch failed and waits for
	// PhaseGone; the launch can no longer happen afterwards.
	h.terminate(t, req.ID, "launch failed")
	if gone := h.rec.wait(t, req.ID, engine.PhaseGone); gone.Message != "launch failed" {
		t.Fatalf("gone %+v", gone)
	}
	if _, err := h.p.Launch(context.Background(), req); err == nil || !strings.Contains(err.Error(), "terminated") {
		t.Fatalf("launch after terminate: %v", err)
	}
	// The only port of the range was released again.
	ok := h.request(t, config.Profile{})
	h.launch(t, ok)
	h.rec.wait(t, ok.ID, engine.PhaseReady)
}

func TestRunStopsAllEngines(t *testing.T) {
	h := newHarness(t, 4)
	var ids, endpoints []string
	for i := 0; i < 2; i++ {
		req := h.request(t, config.Profile{})
		h.launch(t, req)
		ids = append(ids, req.ID)
		endpoints = append(endpoints, h.rec.wait(t, req.ID, engine.PhaseReady).Endpoint)
	}
	hang := h.request(t, config.Profile{InitSQL: "-- helper:hang"})
	h.launch(t, hang)

	h.stop(t)
	select {
	case <-h.p.Done():
	default:
		t.Fatal("Done must be closed once Run returned")
	}
	eventually(t, "events channel closed", h.rec.isClosed)
	for _, ep := range endpoints {
		if _, _, err := get(ep + "/"); err == nil {
			t.Fatalf("%s still answers after shutdown", ep)
		}
	}
	entries, err := os.ReadDir(h.cfg.Provider.Process.WorkDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("work directory after shutdown: %v %v", entries, err)
	}
	if list, _ := h.p.List(context.Background()); len(list) != 0 {
		t.Fatalf("List after shutdown: %+v", list)
	}
	if _, err := h.p.Launch(context.Background(), h.request(t, config.Profile{})); !errors.Is(err, errStopped) {
		t.Fatalf("Launch after shutdown: %v", err)
	}
	// Terminate after shutdown neither blocks nor panics.
	h.terminate(t, ids[0], "late")
}

func TestWarmEngineClaim(t *testing.T) {
	h := newHarness(t, 2)
	warmKey := engine.Key{ShareLevel: engine.ShareConnection, Profile: "default"}
	id := h.p.NewID(warmKey, true)
	if !regexp.MustCompile(`^proc-warm-default-[a-z0-9]{5}$`).MatchString(id) {
		t.Fatalf("warm id %q", id)
	}
	h.launch(t, engine.LaunchRequest{ID: id, Key: warmKey, Warm: true, ProfileName: "default",
		Profile: config.Profile{Database: ":memory:"}, Token: newToken(t)})
	h.rec.wait(t, id, engine.PhaseReady)
	claimed := engine.Key{ShareLevel: engine.ShareConnection, Owner: "session-1", Profile: "default"}
	if err := h.p.Claim(context.Background(), id, claimed); err != nil {
		t.Fatal(err)
	}
	list, _ := h.p.List(context.Background())
	if len(list) != 1 || list[0].Key != claimed || list[0].Warm {
		t.Fatalf("after claim: %+v", list)
	}
	if err := h.p.Claim(context.Background(), "proc-unknown-aaaaa", claimed); err == nil {
		t.Fatal("claiming an unknown engine must fail")
	}
}

func TestLaunchRejectsBadRequests(t *testing.T) {
	h := newHarness(t, 1)
	good := h.request(t, config.Profile{})
	for name, mutate := range map[string]func(*engine.LaunchRequest){
		"path in id":  func(r *engine.LaunchRequest) { r.ID = "../escape" },
		"empty id":    func(r *engine.LaunchRequest) { r.ID = "" },
		"no token":    func(r *engine.LaunchRequest) { r.Token = "" },
		"dot at end":  func(r *engine.LaunchRequest) { r.ID = "proc-x." },
		"separator":   func(r *engine.LaunchRequest) { r.ID = `proc\x` },
		"space in id": func(r *engine.LaunchRequest) { r.ID = "proc x" },
	} {
		req := good
		mutate(&req)
		if _, err := h.p.Launch(context.Background(), req); err == nil {
			t.Errorf("%s: Launch must fail", name)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.p.Launch(ctx, good); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: %v", err)
	}
	h.launch(t, good)
	if _, err := h.p.Launch(context.Background(), good); err == nil {
		t.Error("launching an id twice must fail")
	}
}

func TestNewIDAndSpecHash(t *testing.T) {
	cfg := testConfig(t, 1)
	p, err := New(cfg, nil, WithCommand(helperCommand))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key  engine.Key
		want string
	}{
		{engine.Key{ShareLevel: engine.ShareUser, Owner: "alice"}, `^proc-user-alice-[a-z0-9]{5}$`},
		{engine.Key{ShareLevel: engine.ShareUser, Owner: "Bob.Smith@Example.COM"}, `^proc-user-bob-smith-example-com-[a-z0-9]{5}$`},
		{engine.Key{ShareLevel: engine.ShareGroup, Owner: "data_eng"}, `^proc-group-data-eng-[a-z0-9]{5}$`},
		{engine.Key{ShareLevel: engine.ShareServer}, `^proc-server-[a-z0-9]{5}$`},
		{engine.Key{ShareLevel: engine.ShareConnection, Owner: "Ab3_xYz-9QwErTyU"}, `^proc-connection-ab3-xyz-9qwertyu-[a-z0-9]{5}$`},
		{engine.Key{ShareLevel: engine.ShareUser, Owner: strings.Repeat("verylongname", 10)}, `^proc-user-verylongnameverylongname-[a-z0-9]{5}$`},
		{engine.Key{ShareLevel: engine.ShareUser, Owner: "---"}, `^proc-user-[a-z0-9]{5}$`},
	} {
		id := p.NewID(tc.key, false)
		if !regexp.MustCompile(tc.want).MatchString(id) || !validID(id) {
			t.Errorf("NewID(%+v) = %q, want %s", tc.key, id, tc.want)
		}
	}
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := p.NewID(engine.Key{ShareLevel: engine.ShareServer}, false)
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}

	prof := config.Profile{InitSQL: "SELECT 1"}
	h1 := p.SpecHash("default", prof)
	if h1 == "" || h1 != p.SpecHash("default", prof) {
		t.Fatal("SpecHash must be stable and non-empty")
	}
	if h1 == p.SpecHash("other", prof) || h1 == p.SpecHash("default", config.Profile{InitSQL: "SELECT 2"}) {
		t.Fatal("SpecHash must cover the profile name and the profile")
	}
	cfg2 := testConfig(t, 1)
	cfg2.Provider.Process.DuckDBPath = "/opt/duckdb-2.0.1/duckdb"
	p2, err := New(cfg2, nil, WithCommand(helperCommand))
	if err != nil {
		t.Fatal(err)
	}
	if h1 == p2.SpecHash("default", prof) {
		t.Fatal("SpecHash must cover the DuckDB binary")
	}
	if p.Name() != "process" || !p.CanLaunch() || p.WaitSynced(context.Background()) != nil {
		t.Fatal("provider basics")
	}
}

func TestNewValidatesConfiguration(t *testing.T) {
	if _, err := New(nil, nil); err == nil {
		t.Fatal("New(nil) must fail")
	}
	for _, r := range [][2]int{{0, 100}, {100, 99}, {65000, 65536}, {-1, 10}} {
		cfg := testConfig(t, 1)
		cfg.Provider.Process.PortRangeStart, cfg.Provider.Process.PortRangeEnd = r[0], r[1]
		if _, err := New(cfg, nil, WithCommand(helperCommand)); err == nil {
			t.Errorf("port range %d-%d must be rejected", r[0], r[1])
		}
	}
	// Unset fields get the defaults.
	cfg := testConfig(t, 1)
	cfg.Provider.Process = config.ProcessProviderConfig{BindHost: "[::1]"}
	p, err := New(cfg, nil, WithCommand(helperCommand))
	if err != nil {
		t.Fatal(err)
	}
	if p.duckdbPath != "duckdb" || p.portStart != 19500 || p.portEnd != 19999 || p.bindHost != "::1" ||
		p.dialHost != "::1" || p.workDir != filepath.Join(os.TempDir(), "quackmux-engines") {
		t.Fatalf("defaults: %+v", p)
	}
}

func TestRunOnlyOnce(t *testing.T) {
	h := newHarness(t, 1)
	eventually(t, "Run started", h.p.running.Load)
	if err := h.p.Run(context.Background()); err == nil {
		t.Fatal("a second Run must fail at once")
	}
}

func TestTerminateUnknownEngine(t *testing.T) {
	h := newHarness(t, 1)
	h.terminate(t, "proc-user-ghost-abcde", "admin")
	if gone := h.rec.wait(t, "proc-user-ghost-abcde", engine.PhaseGone); gone.Message != "admin" {
		t.Fatalf("gone %+v", gone)
	}
}

func TestManagerIntegration(t *testing.T) {
	cfg := testConfig(t, 4)
	p, err := New(cfg, nil, WithCommand(helperCommand), WithProbeInterval(20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	m := engine.NewManager(cfg, p, slog.New(slog.DiscardHandler), metrics.New())
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		select {
		case <-p.Done():
		case <-time.After(30 * time.Second):
			t.Error("provider did not stop")
		}
	}()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	key := engine.Key{ShareLevel: engine.ShareUser, Owner: "alice", Subdomain: "default", Profile: "default"}
	acquireCtx, acquireCancel := context.WithTimeout(ctx, 30*time.Second)
	defer acquireCancel()
	lease, err := m.Acquire(acquireCtx, key, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if code, _, err := get(lease.Endpoint + "/"); err != nil || code != http.StatusOK {
		t.Fatalf("engine endpoint: %d %v", code, err)
	}
	lease.Release()
	if err := m.Terminate(lease.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "engine removed from the registry", func() bool {
		_, ok := m.Get(lease.ID)
		return !ok
	})
	if list, _ := p.List(ctx); len(list) != 0 {
		t.Fatalf("provider still lists %+v", list)
	}
}
