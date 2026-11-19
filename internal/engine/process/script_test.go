package process

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"quackmux/internal/config"
	"quackmux/internal/engine"
)

func TestServeScript(t *testing.T) {
	prof := config.Profile{InitSQL: "ATTACH 'lake.db' AS lake;", Settings: map[string]string{"memory_limit": "1GB"}}
	got := serveScript(prof, "127.0.0.1", 19500, "it's-a-token")
	want := "SET memory_limit = '1GB';\nATTACH 'lake.db' AS lake;\n" +
		".output " + os.DevNull + "\n" +
		"CALL quack_serve('quack:127.0.0.1:19500', token = 'it''s-a-token', disable_ssl = true, allow_other_hostname = false);\n" +
		".output stdout\n"
	if got != want {
		t.Fatalf("script:\n%s\nwant:\n%s", got, want)
	}
	for host, other := range map[string]bool{
		"localhost": false, "LOCALHOST": false, "::1": false, "0.0.0.0": true, "192.168.1.10": true, "::": true, "devbox": true,
	} {
		s := serveScript(config.Profile{}, host, 1, "t")
		if !strings.Contains(s, "allow_other_hostname = "+map[bool]string{true: "true", false: "false"}[other]) {
			t.Errorf("%s: %s", host, s)
		}
	}
	if s := serveScript(config.Profile{}, "::1", 19500, "t"); !strings.Contains(s, "'quack:[::1]:19500'") {
		t.Errorf("IPv6 address must be bracketed: %s", s)
	}
	if s := serveScript(config.Profile{}, "localhost", 1, "t"); !strings.HasPrefix(s, ".output ") {
		t.Errorf("without settings and init SQL the script starts with .output: %q", s)
	}
}

func TestHosts(t *testing.T) {
	for bind, want := range map[string]string{
		"0.0.0.0": "127.0.0.1", "::": "::1", "127.0.0.1": "127.0.0.1", "::1": "::1",
		"localhost": "localhost", "192.168.1.10": "192.168.1.10", "devbox": "devbox",
	} {
		if got := dialHost(bind); got != want {
			t.Errorf("dialHost(%q) = %q, want %q", bind, got, want)
		}
	}
}

func TestDatabaseArg(t *testing.T) {
	for in, want := range map[string]string{
		"":                ":memory:",
		":memory:":        ":memory:",
		"data.duckdb":     "data.duckdb",
		"/srv/lake.db":    "/srv/lake.db",
		"-weird.duckdb":   "." + string(filepath.Separator) + "-weird.duckdb",
		"md:my_db?x=1":    "md:my_db?x=1",
		"C:\\data\\a.db":  "C:\\data\\a.db",
		"--help.duckdb":   "." + string(filepath.Separator) + "--help.duckdb",
		"relative/sub.db": "relative/sub.db",
	} {
		if got := databaseArg(in); got != want {
			t.Errorf("databaseArg(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEngineEnv(t *testing.T) {
	base := []string{
		"PATH=/usr/bin", "=C:=C:\\work", "QUACKMUX_SESSION_KEY=secret", "quackmux_admin_token=x",
		"QUACK_TOKEN=gateway-token", "Quack_Token=other", "HOME=/home/dev", "QUACKMUXER=keep",
	}
	req := engine.LaunchRequest{
		ID:          "proc-user-alice-abcde",
		Key:         engine.Key{ShareLevel: engine.ShareUser, Owner: "alice", Subdomain: "etl", Profile: "large"},
		ProfileName: "large",
		Token:       "engine-token",
		Profile: config.Profile{Env: []corev1.EnvVar{
			{Name: "AWS_REGION", Value: "eu-central-1"},
			{Name: "FROM_SECRET", ValueFrom: &corev1.EnvVarSource{}},
			{Name: "QUACK_TOKEN", Value: "override"},
			{Name: "Quackmux_Port", Value: "1"},
			{Name: "", Value: "nameless"},
			{Name: "A=B", Value: "x"},
		}},
	}
	env, skipped := engineEnv(base, req, "lake.db", 19501)
	want := []string{
		"PATH=/usr/bin", "=C:=C:\\work", "HOME=/home/dev", "QUACKMUXER=keep",
		"AWS_REGION=eu-central-1",
		"QUACK_TOKEN=engine-token", "QUACKMUX_DATABASE=lake.db", "QUACKMUX_PORT=19501",
		"QUACKMUX_ENGINE_ID=proc-user-alice-abcde", "QUACKMUX_SHARE_LEVEL=USER", "QUACKMUX_OWNER=alice",
		"QUACKMUX_SUBDOMAIN=etl", "QUACKMUX_PROFILE=large",
	}
	if !slices.Equal(env, want) {
		t.Fatalf("env:\n%q\nwant:\n%q", env, want)
	}
	if !slices.Equal(skipped, []string{"FROM_SECRET", "QUACK_TOKEN", "Quackmux_Port", "", "A=B"}) {
		t.Fatalf("skipped %q", skipped)
	}
	// Without a base the gateway's environment is used.
	t.Setenv("QUACKMUX_TEST_SECRET", "s3cr3t")
	t.Setenv("QUACKMUX_TEST_KEEP_OUT", "x")
	env, _ = engineEnv(nil, req, ":memory:", 1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "QUACKMUX_TEST_") {
			t.Fatalf("inherited %q", kv)
		}
	}
	if _, ok := envValue(env, "PATH"); !ok {
		t.Fatal("PATH must be inherited")
	}
}

func TestValidID(t *testing.T) {
	for id, want := range map[string]bool{
		"proc-user-alice-x7k2p": true, "a": true, "Engine_1.2": true,
		"": false, ".": false, "..": false, "../x": false, "a/b": false, `a\b`: false, "a b": false,
		"-a": false, "a-": false, "a.": false, strings.Repeat("a", 128): true, strings.Repeat("a", 129): false,
	} {
		if got := validID(id); got != want {
			t.Errorf("validID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestRedact(t *testing.T) {
	tok := "9f86d081884c7d659a2feaa0c55ad015"
	for in, want := range map[string]string{
		"no secrets here":                     "no secrets here",
		"token=" + tok + " end":               "token=" + redactedToken + " end",
		tok + tok:                             redactedToken + redactedToken,
		tok[10:] + " rest":                    redactedToken + " rest",  // cut off at the start
		"start " + tok[:20]:                   "start " + redactedToken, // cut off at the end
		"abc" + tok[len(tok)-3:] + "x":        "abc" + tok[len(tok)-3:] + "x",
		tok[len(tok)-3:] + " short fragments": tok[len(tok)-3:] + " short fragments",
	} {
		if got := redact(in, tok); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
	if got := redact("text", ""); got != "text" {
		t.Errorf("empty token: %q", got)
	}
}

func TestOutputTail(t *testing.T) {
	tok := strings.Repeat("f", 64)
	o := newOutput(tok, slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if o.Tail() != "" {
		t.Fatal("empty output")
	}
	_, _ = o.Write([]byte(strings.Repeat("a", 10000)))
	_, _ = o.Write([]byte("\nerror near token '" + tok + "'\n"))
	tail := o.Tail()
	if len(tail) > outputTailBytes || !strings.HasSuffix(tail, "error near token '"+redactedToken+"'") {
		t.Fatalf("tail (%d bytes) ends with %q", len(tail), tail[max(0, len(tail)-60):])
	}
	// A token cut in half by the tail buffer does not leak either.
	o = newOutput(tok, slog.New(slog.DiscardHandler))
	_, _ = o.Write([]byte(tok))
	_, _ = o.Write([]byte(strings.Repeat("b", o.tailCap-len(tok)/2)))
	if strings.Contains(o.Tail(), "ffff") {
		t.Fatal("tail reveals part of the token")
	}
	// One write larger than the buffer.
	o = newOutput("", slog.New(slog.DiscardHandler))
	_, _ = o.Write([]byte(strings.Repeat("c", 5000) + "end"))
	if tail := o.Tail(); len(tail) != outputTailBytes || !strings.HasSuffix(tail, "end") {
		t.Fatalf("tail %d bytes", len(tail))
	}
}
