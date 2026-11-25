package e2e

import (
	"bytes"
	"path/filepath"
	"testing"

	"quackmux/internal/wire"
)

// The engine image, relative to this package.
var engineImageDir = filepath.Join("..", "..", "images", "engine")

// The authorization macro of the engine image lets exactly the query through
// that the gateway sends to list the connections of an engine, and its cases
// check that it does.
func TestAuthorizeMacroAllowsTheListing(t *testing.T) {
	macro := readFile(t, filepath.Join(engineImageDir, "authorize.sql"))
	if want := "WHEN sql = ('" + wire.ActiveConnectionsSQL + "' COLLATE c) THEN"; !bytes.Contains(macro, []byte(want)) {
		t.Errorf("authorize.sql does not let the gateway's listing through: no line %q", want)
	}
	cases := readFile(t, filepath.Join(engineImageDir, "authorize-cases.tsv"))
	if want := "\tOK\tQ\t" + wire.ActiveConnectionsSQL + "\n"; !bytes.Contains(cases, []byte(want)) {
		t.Errorf("authorize-cases.tsv has no case that the listing %q passes", wire.ActiveConnectionsSQL)
	}
}
