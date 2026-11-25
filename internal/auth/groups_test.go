package auth

import (
	"reflect"
	"testing"

	"quackmux/internal/config"
)

// The static authenticator knows the groups of a user from the users file
// as it is now, also of a disabled user; unknown users have none.
func TestStaticGroupsOf(t *testing.T) {
	path := writeFile(t, t.TempDir(), "users.yaml", []byte(staticUsersYAML()))
	a, _ := newTestStatic(t, path, 0, newFakeClock(staticNow).Now)
	var gl GroupLookup = a
	for user, want := range map[string][]string{
		"alice": {"analytics", "admins"}, "dave": nil, "nobody": nil,
	} {
		if got := gl.GroupsOf(user); !reflect.DeepEqual(got, want) {
			t.Errorf("GroupsOf(%q) = %v, want %v", user, got, want)
		}
	}
	gl.GroupsOf("alice")[0] = "changed"
	if got := gl.GroupsOf("alice"); got[0] != "analytics" {
		t.Fatalf("the groups were shared: %v", got)
	}
	rewrite(t, path, "users:\n  - name: dave\n    groups: [etl]\n", 2)
	if err := a.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := gl.GroupsOf("dave"); !reflect.DeepEqual(got, []string{"etl"}) {
		t.Fatalf("after a reload GroupsOf(dave) = %v, want [etl]", got)
	}
	if got := gl.GroupsOf("alice"); got != nil {
		t.Fatalf("after a reload GroupsOf(alice) = %v, want none", got)
	}
}

// With auth.mode none every user has the default groups; a JWT carries the
// groups itself, so the jwt authenticator cannot look them up.
func TestNoneGroupsOfAndNoLookupForJWT(t *testing.T) {
	a, err := newNone(config.NoneAuthConfig{DefaultGroups: []string{"devs", "all", "devs"}}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if got := GroupLookup(a).GroupsOf("anyone"); !reflect.DeepEqual(got, []string{"devs", "all"}) {
		t.Fatalf("GroupsOf = %v, want [devs all]", got)
	}
	if _, ok := any(&jwtAuthenticator{}).(GroupLookup); ok {
		t.Fatal("the jwt authenticator must not claim to know groups")
	}
}
