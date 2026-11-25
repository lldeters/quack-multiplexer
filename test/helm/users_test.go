package helm

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"quackmux/internal/auth"
)

// TestChartUsersMatchGateway gives users files to the chart (as the values
// "users", which have the layout of a users file) and to the gateway's own
// check: the chart must refuse every file the gateway refuses. The gateway
// keeps its previous users when a changed file is invalid and does not start
// with one, so a file the chart let through would make helm upgrade report
// success while the change never applies (a user meant to be disabled stays
// enabled), and the next restart would crash-loop. The chart refuses more in
// one respect only: a number where the file wants a string, which the
// gateway converts (values.schema.json types every field).
func TestChartUsersMatchGateway(t *testing.T) {
	const sha = "5e884898da28047151d0e56f8dc6292773603d0d6aabbdd62a11ef721d1542d8"
	long := strings.Repeat("ä", 129) // 129 characters, 258 bytes
	for _, tc := range []struct {
		name  string
		file  string
		valid bool // for the gateway; the chart must agree unless strict
		// strict: the chart refuses a file the gateway accepts.
		strict bool
	}{
		{"examples/users.yaml", string(readRepoFile(t, filepath.Join("examples", "users.yaml"))), true, false},
		{"testdata/values-users.yaml", string(readRepoFile(t, filepath.Join("test", "helm", "testdata", "values-users.yaml"))), true, false},
		{"no users", "users: []\n", true, false},
		{"plain token", "users: [{name: dev, tokens: [{plain: dev-token}]}]\n", true, false},
		{"sha256 in upper case with spaces", "users: [{name: a, tokens: [{sha256: \" " + strings.ToUpper(sha) + " \"}]}]\n", true, false},
		{"expires with offset and fraction", "users: [{name: a, tokens: [{sha256: " + sha + ", expires: \"2027-06-30T12:00:00.5+02:00\"}]}]\n", true, false},
		{"names of 256 bytes", "users: [{name: " + strings.Repeat("ä", 128) + ", groups: [" + strings.Repeat("g", 256) + "]}]\n", true, false},

		{"unknown user field", "users: [{name: alice, disable: true}]\n", false, false},
		{"unknown token field", "users: [{name: alice, tokens: [{sha265: " + sha + "}]}]\n", false, false},
		{"disabled is no boolean", "users: [{name: alice, disabled: \"yes\"}]\n", false, false},
		{"name is a number", "users: [{name: 42}]\n", true, true},
		{"plain token is a number", "users: [{name: a, tokens: [{plain: 1234}]}]\n", true, true},
		{"empty name", "users: [{name: \"\"}]\n", false, false},
		{"no name", "users: [{groups: [a]}]\n", false, false},
		{"name with white space", "users: [{name: \"alice \"}]\n", false, false},
		{"name longer than 256 bytes", "users: [{name: " + long + "}]\n", false, false},
		{"group with a control character", "users: [{name: alice, groups: [\"a\\x07b\"]}]\n", false, false},
		{"empty group", "users: [{name: alice, groups: [\"\"]}]\n", false, false},
		{"duplicate user", "users: [{name: alice}, {name: alice}]\n", false, false},
		{"sha256 not hex", "users: [{name: alice, tokens: [{sha256: zz}]}]\n", false, false},
		{"sha256 too short", "users: [{name: alice, tokens: [{sha256: " + sha[1:] + "}]}]\n", false, false},
		{"sha256 and plain", "users: [{name: alice, tokens: [{sha256: " + sha + ", plain: x}]}]\n", false, false},
		{"neither sha256 nor plain", "users: [{name: alice, tokens: [{description: laptop}]}]\n", false, false},
		{"plain with a question mark", "users: [{name: alice, tokens: [{plain: \"a?b\"}]}]\n", false, false},
		// "password" hashes to sha: the same token twice.
		{"duplicate token", "users: [{name: a, tokens: [{plain: password}]}, {name: b, tokens: [{sha256: " + strings.ToUpper(sha) + "}]}]\n", false, false},
		{"expires month 13", "users: [{name: a, tokens: [{sha256: " + sha + ", expires: \"2027-13-01T00:00:00Z\"}]}]\n", false, false},
		{"expires without zone", "users: [{name: a, tokens: [{sha256: " + sha + ", expires: \"2027-06-30T00:00:00\"}]}]\n", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "users.yaml")
			if err := os.WriteFile(path, []byte(tc.file), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := auth.ValidateUsersFile(path); (err == nil) != tc.valid {
				t.Fatalf("the test case is wrong: the gateway's check returns %v for\n%s", err, tc.file)
			}
			out, err := helmTemplate(t, releaseName, "-f", path, "--show-only", "templates/secrets.yaml")
			if tc.strict {
				if err == nil {
					t.Fatalf("the chart accepts a number where the users file wants a string:\n%s", tc.file)
				}
				return
			}
			if !tc.valid {
				if err == nil {
					t.Fatalf("the chart accepts a users file that the gateway refuses:\n%s", tc.file)
				}
				return
			}
			if err != nil {
				t.Fatalf("the chart refuses a users file that the gateway accepts: %v", err)
			}
			users := secretValue(t, parseObjects(t, out), "quackmux-users", "users.yaml")
			validateUsers(t, users)
			if got, want := yamlMap(t, []byte(users)), yamlMap(t, []byte(tc.file)); !reflect.DeepEqual(got, want) {
				t.Errorf("the chart's users.yaml differs from the values:\n%s", users)
			}
		})
	}
}
