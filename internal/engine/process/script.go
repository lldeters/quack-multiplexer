package process

import (
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"quackmux/internal/config"
	"quackmux/internal/engine"
)

// serveScript renders what the provider writes to the CLI's standard input:
// the profile's settings and init SQL, the Quack server start, then the
// post-start SQL. Every connection of the engine can read two things of the
// CLI: the lines it read from stdin (shell_history()) and its last result
// (FROM _). So the token is read from the environment (QUACK_TOKEN), only
// the address of the row that quack_serve returns is selected (with the
// output on the null device), and a harmless SELECT follows the post-start
// SQL.
func serveScript(prof config.Profile, bindHost string, port int) string {
	var b strings.Builder
	b.WriteString(engine.RenderInitSQL(prof))
	b.WriteString(".output " + os.DevNull + "\n")
	fmt.Fprintf(&b, "SELECT listen_uri FROM quack_serve(%s, token = getenv('%s'), disable_ssl = true, allow_other_hostname = %t);\n",
		engine.QuoteSQLString(quackURI(bindHost, port)), engine.EnvToken, !isLocalHost(bindHost))
	b.WriteString(".output stdout\n")
	if sql := engine.RenderPostStartSQL(prof); sql != "" {
		b.WriteString(sql)
		b.WriteString(".output " + os.DevNull + "\n")
		b.WriteString("SELECT 'started' AS quackmux_engine;\n")
		b.WriteString(".output stdout\n")
	}
	return b.String()
}

// quackURI is the address quack_serve listens on, e.g. quack:127.0.0.1:19500
// (IPv6 hosts in brackets).
func quackURI(host string, port int) string {
	return "quack:" + net.JoinHostPort(host, strconv.Itoa(port))
}

// isLocalHost reports whether quack_serve accepts host without
// allow_other_hostname.
func isLocalHost(host string) bool {
	return strings.EqualFold(host, "localhost") || host == "127.0.0.1" || host == "::1"
}

// dialHost returns the address the gateway uses to reach an engine bound to
// bind: the loopback address for the unspecified ("any") address, which
// cannot be dialed everywhere (Windows), and bind itself otherwise.
func dialHost(bind string) string {
	if ip := net.ParseIP(bind); ip != nil && ip.IsUnspecified() {
		if ip.To4() != nil {
			return "127.0.0.1"
		}
		return "::1"
	}
	return bind
}

// databaseArg returns the database argument of the CLI. A path starting with
// '-' would be taken for an option, so it is made explicitly relative.
func databaseArg(db string) string {
	if db == "" {
		return ":memory:"
	}
	if strings.HasPrefix(db, "-") {
		return "." + string(filepath.Separator) + db
	}
	return db
}

// reservedEnv reports whether an environment variable belongs to the
// gateway or the engine contract and must not be inherited by engines.
// Windows treats names case-insensitively; elsewhere being stricter does no
// harm.
func reservedEnv(name string) bool {
	upper := strings.ToUpper(name)
	return upper == engine.EnvToken || strings.HasPrefix(upper, "QUACKMUX_")
}

// engineEnv returns the environment of an engine process: base (the
// gateway's environment if nil) without reserved variables, the profile's
// plain-value env entries and the engine container contract. It also returns
// the names of profile entries it had to skip.
func engineEnv(base []string, req engine.LaunchRequest, database string, port int) (env, skipped []string) {
	if base == nil {
		base = os.Environ()
	}
	env = make([]string, 0, len(base)+len(req.Profile.Env)+8)
	for _, kv := range base {
		// Windows has entries like "=C:=C:\dir" (the name is empty here).
		name, _, _ := strings.Cut(kv, "=")
		if reservedEnv(name) {
			continue
		}
		env = append(env, kv)
	}
	for _, ev := range req.Profile.Env {
		if ev.ValueFrom != nil || ev.Name == "" || strings.ContainsAny(ev.Name, "=\x00") || reservedEnv(ev.Name) {
			skipped = append(skipped, ev.Name)
			continue
		}
		env = append(env, ev.Name+"="+ev.Value)
	}
	env = append(env,
		engine.EnvToken+"="+req.Token,
		engine.EnvDatabase+"="+database,
		engine.EnvPort+"="+strconv.Itoa(port),
		engine.EnvEngineID+"="+req.ID,
		engine.EnvShareLevel+"="+string(req.Key.ShareLevel),
		engine.EnvOwner+"="+req.Key.Owner,
		engine.EnvSubdomain+"="+req.Key.Subdomain,
		engine.EnvProfile+"="+req.ProfileName,
	)
	return env, skipped
}

// idRE matches the ids Launch accepts. They name a directory, so they must be
// a single, portable path element.
var idRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,126}[A-Za-z0-9])?$`)

func validID(id string) bool { return idRE.MatchString(id) }

const (
	// maxIDPart bounds each readable part of an id (paths on Windows are
	// limited, and the engine creates files in its directory).
	maxIDPart      = 24
	suffixLength   = 5
	suffixAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
)

// idHead joins "proc" and the sanitized, non-empty parts with '-'.
func idHead(parts ...string) string {
	head := "proc"
	for _, part := range parts {
		if s := idPart(part); s != "" {
			head += "-" + s
		}
	}
	return head
}

// idPart lowercases s, turns everything outside [a-z0-9] into '-', collapses
// runs of '-', trims them at both ends and shortens the result.
func idPart(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	out := b.String()
	if len(out) > maxIDPart {
		out = out[:maxIDPart]
	}
	return strings.TrimRight(out, "-")
}

func randomSuffix() string {
	b := make([]byte, suffixLength)
	for i := range b {
		b[i] = suffixAlphabet[rand.IntN(len(suffixAlphabet))]
	}
	return string(b)
}
