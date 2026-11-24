package engine

import (
	"testing"

	"quackmux/internal/config"
)

// The last statement of the init SQL is terminated so that the start of the
// server can follow it. Where a ";" at the end of the last line would not
// end it, the ";" goes on a line of its own: after a dot command of the
// DuckDB CLI (".read authorize.sql;" would read the file "authorize.sql;"),
// after "--" (the ";" would be part of the comment) and after a line that
// continues a pending statement with ".".
func TestRenderInitSQLTerminatesTheLastStatement(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    config.Profile
		want string
	}{
		{"blank", config.Profile{InitSQL: " \n\t"}, ""},
		{"terminated", config.Profile{InitSQL: "  SELECT 1;\n"}, "SELECT 1;\n"},
		{"not terminated", config.Profile{InitSQL: "SELECT 42"}, "SELECT 42;\n"},
		{"dot command last", config.Profile{InitSQL: ".read /usr/local/share/quackmux/authorize.sql"},
			".read /usr/local/share/quackmux/authorize.sql\n;\n"},
		{"dot command after settings", config.Profile{Settings: map[string]string{"threads": "4"}, InitSQL: ".read /x.sql"},
			"SET threads = '4';\n.read /x.sql\n;\n"},
		{"dot command after statements and comments", config.Profile{
			InitSQL: "CREATE TABLE t AS\nSELECT 1 AS x;\n-- the macro\n\n.read /x.sql",
		}, "CREATE TABLE t AS\nSELECT 1 AS x;\n-- the macro\n\n.read /x.sql\n;\n"},
		{"dot command after an inline comment", config.Profile{
			InitSQL: "CREATE TABLE notes AS SELECT 1 AS x; -- demo table\n.read /usr/local/share/quackmux/authorize.sql",
		}, "CREATE TABLE notes AS SELECT 1 AS x; -- demo table\n.read /usr/local/share/quackmux/authorize.sql\n;\n"},
		{"dot commands only", config.Profile{InitSQL: ".read /a.sql\n.read /b.sql"}, ".read /a.sql\n.read /b.sql\n;\n"},
		// A ";" after "--" would be part of the comment.
		{"comment after the statement", config.Profile{InitSQL: "SELECT 2 AS y -- the answer"}, "SELECT 2 AS y -- the answer\n;\n"},
		{"comment line last", config.Profile{InitSQL: "SELECT 1;\n-- done"}, "SELECT 1;\n-- done\n;\n"},
		{"dashes in a string", config.Profile{InitSQL: "SELECT '--' AS dashes"}, "SELECT '--' AS dashes\n;\n"},
		// A line that starts with "." can also continue a pending statement.
		{"pending statement", config.Profile{InitSQL: "SELECT *\nFROM lake\n.main.t"}, "SELECT *\nFROM lake\n.main.t\n;\n"},
		{"pending number", config.Profile{InitSQL: "SELECT 1 +\n.5"}, "SELECT 1 +\n.5\n;\n"},
		// Only a dot command at the start of a line is one.
		{"indented", config.Profile{InitSQL: "SELECT 1;\n  .read /x.sql"}, "SELECT 1;\n  .read /x.sql;\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RenderInitSQL(tc.p); got != tc.want {
				t.Errorf("RenderInitSQL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRenderPostStartSQL(t *testing.T) {
	for in, want := range map[string]string{
		"":   "",
		"  ": "",
		"SET enable_external_access = false;\nSET lock_configuration = true": "SET enable_external_access = false;\nSET lock_configuration = true;\n",
		"SET lock_configuration = true;\n":                                   "SET lock_configuration = true;\n",
		".read /etc/quackmux/post-start.sql":                                 ".read /etc/quackmux/post-start.sql\n;\n",
	} {
		// Settings belong to the init SQL; the post-start SQL is the field alone.
		p := config.Profile{PostStartSQL: in, InitSQL: "SELECT 1", Settings: map[string]string{"threads": "4"}}
		if got := RenderPostStartSQL(p); got != want {
			t.Errorf("RenderPostStartSQL(%q) = %q, want %q", in, got, want)
		}
	}
}
