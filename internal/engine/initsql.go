package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"quackmux/internal/config"
)

// The engine container contract. Every provider starts engines with these
// environment variables, and the engine image (images/engine) implements
// them: it opens QUACKMUX_DATABASE, runs QUACKMUX_INIT_SQL, then serves Quack
// over plain HTTP on 0.0.0.0:QUACKMUX_PORT with token QUACK_TOKEN and stays in
// the foreground until SIGTERM. GET / answers once the server listens.
const (
	EnvToken      = "QUACK_TOKEN"
	EnvDatabase   = "QUACKMUX_DATABASE"
	EnvInitSQL    = "QUACKMUX_INIT_SQL"
	EnvPort       = "QUACKMUX_PORT"
	EnvEngineID   = "QUACKMUX_ENGINE_ID"
	EnvShareLevel = "QUACKMUX_SHARE_LEVEL"
	EnvOwner      = "QUACKMUX_OWNER"
	EnvSubdomain  = "QUACKMUX_SUBDOMAIN"
	EnvProfile    = "QUACKMUX_PROFILE"

	// EnvStopTimeout is how many seconds the engine waits for DuckDB to
	// stop after SIGTERM before it kills it (images/engine: default 20).
	// Providers whose platform kills the engine after a grace period set
	// it below that period.
	EnvStopTimeout = "QUACKMUX_STOP_TIMEOUT"

	// EnginePort is the Quack port inside engine containers.
	EnginePort = 9494
)

// RenderInitSQL turns a profile's settings and init SQL into the script the
// engine runs before it starts serving. Settings come first (sorted by name)
// so that InitSQL can rely on them.
func RenderInitSQL(p config.Profile) string {
	var b strings.Builder
	names := make([]string, 0, len(p.Settings))
	for k := range p.Settings {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		// Setting names are validated as identifiers by config.Validate.
		b.WriteString("SET ")
		b.WriteString(k)
		b.WriteString(" = ")
		b.WriteString(QuoteSQLString(p.Settings[k]))
		b.WriteString(";\n")
	}
	if s := strings.TrimSpace(p.InitSQL); s != "" {
		b.WriteString(s)
		if !strings.HasSuffix(s, ";") {
			b.WriteString(";")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// QuoteSQLString renders s as a single-quoted SQL string literal.
func QuoteSQLString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// HashJSON returns a short, stable digest of v's JSON encoding (map keys are
// sorted by encoding/json, so equal values give equal hashes).
func HashJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "unhashable"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}
