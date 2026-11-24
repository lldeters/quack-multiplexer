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
// over plain HTTP on 0.0.0.0:QUACKMUX_PORT with token QUACK_TOKEN, runs
// QUACKMUX_POST_START_SQL and stays in the foreground until SIGTERM. GET /
// answers once the server listens, while the post-start SQL may still run.
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

	// EnvPostStartSQL holds the script that the engine runs right after the
	// Quack server has started (see RenderPostStartSQL).
	EnvPostStartSQL = "QUACKMUX_POST_START_SQL"

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
	writeScript(&b, p.InitSQL)
	return b.String()
}

// RenderPostStartSQL returns the script the engine runs right after the
// Quack server has started, on the connection of the init SQL: settings
// that would break the start, such as SET enable_external_access = false.
func RenderPostStartSQL(p config.Profile) string {
	var b strings.Builder
	writeScript(&b, p.PostStartSQL)
	return b.String()
}

// writeScript writes the script s, trimmed, with its last statement
// terminated, so that more statements may follow. Blank scripts write
// nothing. A ";" at the end of the last line does not always end a
// statement: after a dot command of the DuckDB CLI it becomes part of the
// argument (".read file;" reads the file "file;"), and after "--" it is part
// of the comment. Such a line gets the ";" on a line of its own, which ends
// a pending statement and is an empty statement otherwise. A line that
// starts with "." may also continue a pending statement (".5"); the ";"
// ends it too.
func writeScript(b *strings.Builder, s string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return
	}
	b.WriteString(s)
	last := s[strings.LastIndexByte(s, '\n')+1:]
	switch {
	case strings.HasSuffix(s, ";"):
	case strings.HasPrefix(last, ".") || strings.Contains(last, "--"):
		b.WriteString("\n;")
	default:
		b.WriteString(";")
	}
	b.WriteString("\n")
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
