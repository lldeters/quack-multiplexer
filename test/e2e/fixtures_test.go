package e2e

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"quackmux/internal/auth"
	"quackmux/internal/config"
	"quackmux/internal/engine"
	"quackmux/internal/policy"
)

// Fixtures of the docker compose test, relative to this package.
var (
	composeDir     = "compose"
	envFile        = filepath.Join(composeDir, "e2e.env")
	composeFile    = filepath.Join(composeDir, "docker-compose.yaml")
	runScript      = filepath.Join(composeDir, "run.sh")
	gatewayConfig  = filepath.Join(composeDir, "config", "gateway.yaml")
	idleConfig     = filepath.Join(composeDir, "config", "gateway-idle.yaml")
	usersFile      = filepath.Join(composeDir, "config", "users.yaml")
	adminTokenFile = filepath.Join(composeDir, "config", "admin.token")
)

// Paths inside the gateway containers (docker-compose.yaml mounts them).
const (
	mountConfig     = "/etc/quackmux/config.yaml"
	mountUsers      = "/etc/quackmux/users.yaml"
	mountAdminToken = "/etc/quackmux/admin.token"
)

// envKeys are the variables of e2e.env.
var envKeys = []string{
	"E2E_ALICE_TOKEN", "E2E_BOB_TOKEN", "E2E_WRONG_TOKEN", "E2E_ADMIN_TOKEN",
	"E2E_ENGINE_SHARED_TOKEN", "E2E_ENGINE_ANALYTICS_TOKEN", "E2E_SESSION_KEY",
}

// safeValue restricts the env file values: they appear unquoted in shell, in
// SQL string literals, in YAML and in token options ('?' and '&' are syntax).
var safeValue = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

var envLine = regexp.MustCompile(`^([A-Z][A-Z0-9_]*)=(.*)$`)

// wantEngines are the static engines both gateway configurations route to,
// keyed by engine name.
var wantEngines = map[string]struct {
	url, tokenVar, shareLevel, owner, service string
}{
	"shared":    {"http://engine-shared:9494", "E2E_ENGINE_SHARED_TOKEN", "SERVER", "", "engine-shared"},
	"analytics": {"http://engine-analytics:9494", "E2E_ENGINE_ANALYTICS_TOKEN", "GROUP", "analytics", "engine-analytics"},
}

// scenarios are the connection attempts of run.sh, with the token options it
// sends, and the outcome the gateway must produce.
var scenarios = []struct {
	name     string
	tokenVar string
	options  string
	config   string
	// engine is the static engine the session must reach.
	engine string
	// denied is part of the policy error if the gateway must refuse.
	denied string
	// unauthenticated: the token must be rejected.
	unauthenticated bool
}{
	{name: "a: alice, SERVER", tokenVar: "E2E_ALICE_TOKEN", options: "share_level=server", config: gatewayConfig, engine: "shared"},
	{name: "b: bob, GROUP analytics", tokenVar: "E2E_BOB_TOKEN", options: "share_level=group&group=analytics", config: gatewayConfig, denied: "not a member"},
	{name: "c: alice, GROUP analytics", tokenVar: "E2E_ALICE_TOKEN", options: "share_level=group&group=analytics", config: gatewayConfig, engine: "analytics"},
	{name: "d: wrong token", tokenVar: "E2E_WRONG_TOKEN", options: "share_level=server", config: gatewayConfig, unauthenticated: true},
	{name: "e: bob, SERVER", tokenVar: "E2E_BOB_TOKEN", options: "share_level=server", config: gatewayConfig, engine: "shared"},
	{name: "f: alice, SERVER, gateway-idle", tokenVar: "E2E_ALICE_TOKEN", options: "share_level=server", config: idleConfig, engine: "shared"},
	{name: "h: bob, SERVER, query time limit", tokenVar: "E2E_BOB_TOKEN", options: "share_level=server", config: gatewayConfig, engine: "shared"},
}

// readEnv parses e2e.env: KEY=VALUE lines, comments and blank lines.
func readEnv(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := envLine.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("%s:%d: not a KEY=VALUE line: %q", envFile, n, line)
		}
		if _, dup := env[m[1]]; dup {
			t.Fatalf("%s:%d: %s is set twice", envFile, n, m[1])
		}
		env[m[1]] = m[2]
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return env
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func loadConfig(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return cfg
}

func staticEngine(t *testing.T, cfg *config.Config, path, name string) config.StaticEngine {
	t.Helper()
	for _, e := range cfg.Provider.Static.Engines {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("%s: no static engine %q", path, name)
	return config.StaticEngine{}
}

// engineKey is the engine key a static engine answers for.
func engineKey(t *testing.T, e config.StaticEngine) engine.Key {
	t.Helper()
	lvl, err := engine.ParseShareLevel(e.ShareLevel)
	if err != nil {
		t.Fatalf("static engine %q: %v", e.Name, err)
	}
	return engine.Key{ShareLevel: lvl, Owner: e.Owner, Subdomain: e.Subdomain, Profile: e.Profile, PoolIndex: e.PoolIndex}
}

func newAuthenticator(t *testing.T) auth.Authenticator {
	t.Helper()
	a, err := auth.New(config.AuthConfig{
		Mode:   "static",
		Static: config.StaticAuthConfig{File: usersFile, ReloadInterval: config.D(0)},
	}, nil)
	if err != nil {
		t.Fatalf("%s: %v", usersFile, err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func TestEnvFile(t *testing.T) {
	env := readEnv(t)
	owner := map[string]string{}
	for _, k := range envKeys {
		v, ok := env[k]
		if !ok {
			t.Errorf("%s: %s is missing", envFile, k)
			continue
		}
		if !safeValue.MatchString(v) {
			t.Errorf("%s: %s=%q may only contain [A-Za-z0-9._-]", envFile, k, v)
		}
		if other, dup := owner[v]; dup {
			t.Errorf("%s: %s and %s have the same value", envFile, other, k)
		}
		owner[v] = k
	}
	for k := range env {
		if !slices.Contains(envKeys, k) {
			t.Errorf("%s: unexpected variable %s", envFile, k)
		}
	}
	// The gateway refuses session keys under 16 bytes, the engine image
	// (images/engine/entrypoint.sh) QUACK_TOKEN values under 16 characters.
	if n := len(env["E2E_SESSION_KEY"]); n < 16 {
		t.Errorf("%s: E2E_SESSION_KEY has %d bytes, the gateway needs at least 16", envFile, n)
	}
	for _, k := range []string{"E2E_ENGINE_SHARED_TOKEN", "E2E_ENGINE_ANALYTICS_TOKEN"} {
		if n := len(env[k]); n < 16 {
			t.Errorf("%s: %s has %d characters, the engine image needs at least 16", envFile, k, n)
		}
	}
}

func TestAdminTokenFile(t *testing.T) {
	env := readEnv(t)
	if got := strings.TrimSpace(string(readFile(t, adminTokenFile))); got != env["E2E_ADMIN_TOKEN"] {
		t.Errorf("%s holds %q, want the value of E2E_ADMIN_TOKEN (%q)", adminTokenFile, got, env["E2E_ADMIN_TOKEN"])
	}
}

func TestUsersFile(t *testing.T) {
	if err := auth.ValidateUsersFile(usersFile); err != nil {
		t.Fatal(err)
	}
	env := readEnv(t)
	a := newAuthenticator(t)
	for user, want := range map[string]struct {
		tokenVar    string
		inAnalytics bool
	}{
		"alice": {"E2E_ALICE_TOKEN", true},
		"bob":   {"E2E_BOB_TOKEN", false},
	} {
		id, err := a.Authenticate(context.Background(), auth.Request{Credential: env[want.tokenVar]})
		if err != nil {
			t.Errorf("%s does not accept %s: %v", usersFile, want.tokenVar, err)
			continue
		}
		if id.User != user {
			t.Errorf("%s authenticates %s as %q, want %q", usersFile, want.tokenVar, id.User, user)
		}
		if got := id.InGroup("analytics"); got != want.inAnalytics {
			t.Errorf("%s: %s in group analytics = %v, want %v", usersFile, user, got, want.inAnalytics)
		}
	}
	if _, err := a.Authenticate(context.Background(), auth.Request{Credential: env["E2E_WRONG_TOKEN"]}); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("%s accepts E2E_WRONG_TOKEN (err %v)", usersFile, err)
	}
	// The users file stores hashes only.
	data := readFile(t, usersFile)
	for _, k := range envKeys {
		if bytes.Contains(data, []byte(env[k])) {
			t.Errorf("%s contains the plain value of %s; store its sha256", usersFile, k)
		}
	}
}

func TestGatewayConfigs(t *testing.T) {
	env := readEnv(t)
	gw := loadConfig(t, gatewayConfig)
	idle := loadConfig(t, idleConfig)
	for _, c := range []struct {
		path string
		cfg  *config.Config
	}{{gatewayConfig, gw}, {idleConfig, idle}} {
		cfg := c.cfg
		if cfg.Server.Listen != ":9494" || cfg.Admin.Listen != ":9495" {
			t.Errorf("%s: listeners %q and %q, want :9494 and :9495 (docker-compose.yaml publishes them)", c.path, cfg.Server.Listen, cfg.Admin.Listen)
		}
		if cfg.Admin.TokenFile != mountAdminToken {
			t.Errorf("%s: admin.tokenFile %q, want %q", c.path, cfg.Admin.TokenFile, mountAdminToken)
		}
		if cfg.Auth.Mode != "static" || cfg.Auth.Static.File != mountUsers {
			t.Errorf("%s: auth %s with file %q, want static with %q", c.path, cfg.Auth.Mode, cfg.Auth.Static.File, mountUsers)
		}
		if cfg.Provider.Type != "static" {
			t.Errorf("%s: provider.type %q, want static", c.path, cfg.Provider.Type)
		}
		if n := len(cfg.Provider.Static.Engines); n != len(wantEngines) {
			t.Errorf("%s: %d static engines, want %d", c.path, n, len(wantEngines))
		}
		for name, want := range wantEngines {
			e := staticEngine(t, cfg, c.path, name)
			if e.URL != want.url {
				t.Errorf("%s: engine %s has url %q, want %q", c.path, name, e.URL, want.url)
			}
			if e.Token != env[want.tokenVar] || e.TokenFile != "" {
				t.Errorf("%s: engine %s must use the token %s from %s", c.path, name, want.tokenVar, envFile)
			}
			if !strings.EqualFold(e.ShareLevel, want.shareLevel) || e.Owner != want.owner {
				t.Errorf("%s: engine %s answers %s/%q, want %s/%q", c.path, name, e.ShareLevel, e.Owner, want.shareLevel, want.owner)
			}
			if e.Subdomain != cfg.Defaults.Subdomain || e.Profile != cfg.Defaults.Profile {
				t.Errorf("%s: engine %s has subdomain %q and profile %q; sessions without options use %q and %q",
					c.path, name, e.Subdomain, e.Profile, cfg.Defaults.Subdomain, cfg.Defaults.Profile)
			}
		}
	}

	// Scenario f attaches, waits 20s and expects the session to be closed.
	ss := idle.Session
	if ss.IdleTimeout.Duration != 15*time.Second {
		t.Errorf("%s: session.idleTimeout %s, want 15s", idleConfig, ss.IdleTimeout.Duration)
	}
	if ss.CheckInterval.Duration != time.Second || idle.Engine.CheckInterval.Duration != time.Second {
		t.Errorf("%s: check intervals %s/%s, want 1s/1s", idleConfig, ss.CheckInterval.Duration, idle.Engine.CheckInterval.Duration)
	}
	// The other scenarios must never meet the main gateway's idle timeout.
	if d := gw.Session.IdleTimeout.Duration; d != 0 && d < 10*time.Minute {
		t.Errorf("%s: session.idleTimeout %s is short enough to break scenarios", gatewayConfig, d)
	}

	// Scenario k restarts gateway, which then probes the connections that no
	// client asks for once their engine lease has run out. The lease must
	// outlast the restart, so that the living clients find their
	// connections, and the grace must fit the run. gateway-idle never
	// restarts.
	if !gw.CloseOrphanedConnectionsEnabled() || idle.CloseOrphanedConnectionsEnabled() {
		t.Errorf("engine.closeOrphanedConnections must be on in %s only", gatewayConfig)
	}
	if l := gw.Session.BackendHeartbeatTimeout.Duration; l < time.Minute {
		t.Errorf("%s: session.backendHeartbeatTimeout %s is too short for the living clients of scenario k", gatewayConfig, l)
	}
	if d := gw.OrphanGrace(); d > 2*time.Minute {
		t.Errorf("%s: the orphan grace %s is too long for scenario k", gatewayConfig, d)
	}

	// Apart from the timing and the orphan sweep, the two files must agree.
	adjusted := *idle
	adjusted.Session = gw.Session
	adjusted.Engine.CheckInterval = gw.Engine.CheckInterval
	adjusted.Engine.CloseOrphanedConnections = gw.Engine.CloseOrphanedConnections
	adjusted.Engine.RecoveryGrace = gw.Engine.RecoveryGrace // derived from session.maxHeartbeatTimeout
	if !reflect.DeepEqual(&adjusted, gw) {
		t.Errorf("%s and %s differ in more than the session and engine timing and the orphan sweep", gatewayConfig, idleConfig)
	}
}

// TestQueryTimeLimits pins the query time limits scenario h relies on: bob's
// queries are cancelled after 5s, the other scenarios run without a limit.
func TestQueryTimeLimits(t *testing.T) {
	for _, path := range []string{gatewayConfig, idleConfig} {
		cfg := loadConfig(t, path)
		for user, want := range map[string]time.Duration{"bob": 5 * time.Second, "alice": 0} {
			if got, source := policy.QueryTimeout(cfg, user, nil, cfg.Defaults.Profile); got != want {
				t.Errorf("%s: %s's queries have the time limit %s (%s), want %s", path, user, got, source, want)
			}
		}
	}
	script := string(readFile(t, runScript))
	if !strings.Contains(script, "time limit of 5s (overrides[0].queryTimeout)") {
		t.Errorf("%s does not expect bob's limit of 5s from overrides[0]", runScript)
	}
}

// TestScenarioRouting runs the handshakes of run.sh through the gateway's
// authentication and policy code with the compose configuration.
func TestScenarioRouting(t *testing.T) {
	env := readEnv(t)
	script := readFile(t, runScript)
	a := newAuthenticator(t)
	configs := map[string]*config.Config{gatewayConfig: loadConfig(t, gatewayConfig), idleConfig: loadConfig(t, idleConfig)}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			if ref := `"${` + sc.tokenVar + `}?` + sc.options + `"`; !bytes.Contains(script, []byte(ref)) {
				t.Errorf("%s does not send the token %s", runScript, ref)
			}
			cfg := configs[sc.config]
			credential, tokenOpts, err := auth.ParseToken(env[sc.tokenVar] + "?" + sc.options)
			if err != nil {
				t.Fatal(err)
			}
			id, err := a.Authenticate(context.Background(), auth.Request{Credential: credential, Options: tokenOpts})
			if sc.unauthenticated {
				if !errors.Is(err, auth.ErrUnauthenticated) {
					t.Fatalf("Authenticate: %v, want %v", err, auth.ErrUnauthenticated)
				}
				return
			}
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			opts, err := policy.Options(tokenOpts, http.Header{})
			if err != nil {
				t.Fatal(err)
			}
			dec, err := policy.Resolve(cfg, id, opts, "e2e-session")
			if sc.denied != "" {
				if !errors.Is(err, policy.ErrDenied) || !strings.Contains(err.Error(), sc.denied) {
					t.Fatalf("Resolve: %v, want a denial with %q", err, sc.denied)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if want := engineKey(t, staticEngine(t, cfg, sc.config, sc.engine)); dec.Key != want {
				t.Errorf("the session gets key %s, but engine %s answers %s", dec.Key, sc.engine, want)
			}
		})
	}
}

// composeProject is the part of docker-compose.yaml the test looks at.
type composeProject struct {
	Name     string                    `json:"name"`
	Services map[string]composeService `json:"services"`
}

type composeService struct {
	Image       string            `json:"image"`
	PullPolicy  string            `json:"pull_policy"`
	Entrypoint  []string          `json:"entrypoint"`
	Command     []string          `json:"command"`
	Environment map[string]string `json:"environment"`
	Volumes     []string          `json:"volumes"`
	Profiles    []string          `json:"profiles"`
	ReadOnly    bool              `json:"read_only"`
}

// contractEnv is the engine container contract (internal/engine/initsql.go).
var contractEnv = []string{
	engine.EnvToken, engine.EnvDatabase, engine.EnvInitSQL, engine.EnvPort, engine.EnvEngineID,
	engine.EnvShareLevel, engine.EnvOwner, engine.EnvSubdomain, engine.EnvProfile, engine.EnvPostStartSQL,
}

func TestComposeFile(t *testing.T) {
	var cf composeProject
	if err := yaml.Unmarshal(readFile(t, composeFile), &cf); err != nil {
		t.Fatalf("%s: %v", composeFile, err)
	}
	// run.sh passes -p "$E2E_PROJECT", whose default must be the file's name.
	if ref := "E2E_PROJECT=${E2E_PROJECT:-" + cf.Name + "}"; cf.Name == "" || !bytes.Contains(readFile(t, runScript), []byte(ref)) {
		t.Errorf("%s: project name %q does not match the default run.sh uses", composeFile, cf.Name)
	}
	if !bytes.Contains(readFile(t, runScript), []byte(`docker compose -p "$E2E_PROJECT" `)) {
		t.Errorf("%s: docker compose does not get the project name E2E_PROJECT", runScript)
	}
	gw := loadConfig(t, gatewayConfig)

	for name, want := range wantEngines {
		svc, ok := cf.Services[want.service]
		if !ok {
			t.Errorf("%s: service %s is missing", composeFile, want.service)
			continue
		}
		env := svc.Environment
		for k := range env {
			if !slices.Contains(contractEnv, k) {
				t.Errorf("%s: %s sets %s, which the engine container contract does not define", composeFile, want.service, k)
			}
		}
		if got, ref := env[engine.EnvToken], "${"+want.tokenVar+":?"; !strings.HasPrefix(got, ref) {
			t.Errorf("%s: %s gets %s=%q, want a reference to %s", composeFile, want.service, engine.EnvToken, got, want.tokenVar)
		}
		if got, sql := env[engine.EnvInitSQL], fmt.Sprintf("CREATE TABLE whoami AS SELECT '%s' AS engine;", name); !strings.HasPrefix(got, sql) {
			t.Errorf("%s: %s gets %s=%q, want it to start with %q (run.sh reads whoami)", composeFile, want.service, engine.EnvInitSQL, got, sql)
		}
		if got := env[engine.EnvPort]; got != strconv.Itoa(engine.EnginePort) {
			t.Errorf("%s: %s gets %s=%q, want %d", composeFile, want.service, engine.EnvPort, got, engine.EnginePort)
		}
		e := staticEngine(t, gw, gatewayConfig, name)
		if !strings.EqualFold(env[engine.EnvShareLevel], e.ShareLevel) || env[engine.EnvOwner] != e.Owner ||
			env[engine.EnvSubdomain] != e.Subdomain || env[engine.EnvProfile] != e.Profile {
			t.Errorf("%s: the key in the environment of %s differs from static engine %s", composeFile, want.service, name)
		}
		if u, err := url.Parse(e.URL); err != nil || u.Hostname() != want.service || u.Port() != strconv.Itoa(engine.EnginePort) {
			t.Errorf("%s: static engine %s (%s) does not point at service %s", gatewayConfig, name, e.URL, want.service)
		}
		if !svc.ReadOnly || svc.PullPolicy != "never" {
			t.Errorf("%s: %s must use the local image (pull_policy: never) with a read-only root file system", composeFile, want.service)
		}
	}

	for service, cfgFile := range map[string]string{"gateway": "gateway.yaml", "gateway-idle": "gateway-idle.yaml"} {
		svc, ok := cf.Services[service]
		if !ok {
			t.Errorf("%s: service %s is missing", composeFile, service)
			continue
		}
		if want := []string{"serve", "-config", mountConfig}; !slices.Equal(svc.Command, want) {
			t.Errorf("%s: %s runs %q, want %q", composeFile, service, svc.Command, want)
		}
		if got := svc.Environment["QUACKMUX_SESSION_KEY"]; !strings.HasPrefix(got, "${E2E_SESSION_KEY:?") {
			t.Errorf("%s: %s gets QUACKMUX_SESSION_KEY=%q, want a reference to E2E_SESSION_KEY", composeFile, service, got)
		}
		mounts := map[string]string{}
		for _, v := range svc.Volumes {
			parts := strings.Split(v, ":")
			if len(parts) != 3 || parts[2] != "ro" {
				t.Errorf("%s: %s mounts %q; want SOURCE:TARGET:ro", composeFile, service, v)
				continue
			}
			mounts[parts[1]] = parts[0]
			if _, err := os.Stat(filepath.Join(composeDir, filepath.FromSlash(parts[0]))); err != nil {
				t.Errorf("%s: %s mounts %s: %v", composeFile, service, parts[0], err)
			}
		}
		for target, src := range map[string]string{
			mountConfig:     "./config/" + cfgFile,
			mountUsers:      "./config/users.yaml",
			mountAdminToken: "./config/admin.token",
		} {
			if mounts[target] != src {
				t.Errorf("%s: %s mounts %q at %s, want %q", composeFile, service, mounts[target], target, src)
			}
		}
	}

	client, ok := cf.Services["client"]
	switch {
	case !ok:
		t.Errorf("%s: service client is missing", composeFile)
	case !slices.Equal(client.Entrypoint, []string{"/usr/local/bin/duckdb"}):
		t.Errorf("%s: client entrypoint %q, want the DuckDB CLI", composeFile, client.Entrypoint)
	case !slices.Contains(client.Profiles, "client"):
		t.Errorf("%s: the client service needs the profile \"client\" (docker compose up must not start it)", composeFile)
	}
}
