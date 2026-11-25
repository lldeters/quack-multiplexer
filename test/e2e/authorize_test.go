package e2e

import (
	"bytes"
	"path/filepath"
	"testing"

	"quackmux/internal/wire"
)

// The engine image, relative to this package.
var engineImageDir = filepath.Join("..", "..", "images", "engine")

// The authorization macro of the engine image rejects the query that the
// gateway sends to list the connections of an engine; its variant
// quackmux_authorize_listing lets exactly that query through, and the cases
// check both. The hardened engine of the compose suite runs the variant, so
// that scenario k can close its orphaned connections.
func TestAuthorizeMacroListing(t *testing.T) {
	macro := readFile(t, filepath.Join(engineImageDir, "authorize.sql"))
	for _, want := range []string{
		"WHEN listing AND sql = ('" + wire.ActiveConnectionsSQL + "' COLLATE c) THEN",
		"CREATE OR REPLACE MACRO quackmux_authorize(connection_id, sql) AS quackmux_check(sql, false);",
		"CREATE OR REPLACE MACRO quackmux_authorize_listing(connection_id, sql) AS quackmux_check(sql, true);",
		"\nSET GLOBAL quack_authorization_function = 'quackmux_authorize';\n",
	} {
		if !bytes.Contains(macro, []byte(want)) {
			t.Errorf("authorize.sql has no line %q", want)
		}
	}
	cases := readFile(t, filepath.Join(engineImageDir, "authorize-cases.tsv"))
	if want := "\tLISTING\tQ\t" + wire.ActiveConnectionsSQL + "\n"; !bytes.Contains(cases, []byte(want)) {
		t.Errorf("authorize-cases.tsv has no case for the listing %q", wire.ActiveConnectionsSQL)
	}
	compose := readFile(t, filepath.Join("compose", "docker-compose.yaml"))
	if want := "SET GLOBAL quack_authorization_function = 'quackmux_authorize_listing';"; !bytes.Contains(compose, []byte(want)) {
		t.Errorf("compose/docker-compose.yaml: engine-analytics does not switch to quackmux_authorize_listing")
	}
}
