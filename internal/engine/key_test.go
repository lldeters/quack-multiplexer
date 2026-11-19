package engine

import (
	"strings"
	"testing"

	"quackmux/internal/config"
)

func TestKeyRoundTrip(t *testing.T) {
	keys := []Key{
		{ShareLevel: ShareUser, Owner: "alice", Subdomain: "default", Profile: "default"},
		{ShareLevel: ShareGroup, Owner: "team/a#b c", Subdomain: "etl", Profile: "large", PoolIndex: 3},
		{ShareLevel: ShareServer, Subdomain: "default", Profile: "default"},
		{ShareLevel: ShareConnection, Owner: "sess-123", Profile: "default"},
	}
	for _, k := range keys {
		got, err := ParseKey(k.String())
		if err != nil || got != k {
			t.Fatalf("%v: round trip %+v %v", k, got, err)
		}
		if len(k.Hash()) != 32 {
			t.Fatalf("hash length %d", len(k.Hash()))
		}
	}
	a := Key{ShareLevel: ShareGroup, Owner: "a/b", Subdomain: "c", Profile: "d"}
	b := Key{ShareLevel: ShareGroup, Owner: "a", Subdomain: "b/c", Profile: "d"}
	if a.String() == b.String() || a.Hash() == b.Hash() {
		t.Fatal("escaping must prevent key collisions")
	}
	for _, bad := range []string{"", "USER/a/b/c", "BOGUS/a/b/c#0", "USER/a/b#0", "USER/a/b/c#x", "USER/%zz/b/c#0"} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) accepted", bad)
		}
	}
}

func TestParseShareLevelAndSubdomain(t *testing.T) {
	for in, want := range map[string]ShareLevel{"connection": ShareConnection, " User ": ShareUser, "GROUP": ShareGroup, "server": ShareServer} {
		if got, err := ParseShareLevel(in); err != nil || got != want {
			t.Errorf("%q -> %s %v", in, got, err)
		}
	}
	if _, err := ParseShareLevel("SERVER_LOCAL"); err == nil {
		t.Error("unknown level accepted")
	}
	for s, ok := range map[string]bool{"default": true, "a": true, "etl-1": true, "-x": false, "x-": false, "A": false,
		"": false, strings.Repeat("a", 32): true, strings.Repeat("a", 33): false, "a_b": false} {
		if ValidSubdomain(s) != ok {
			t.Errorf("ValidSubdomain(%q) = %v", s, !ok)
		}
	}
}

func TestRenderInitSQL(t *testing.T) {
	sql := RenderInitSQL(config.Profile{
		Settings: map[string]string{"threads": "4", "memory_limit": "2GB", "search_path": "it's"},
		InitSQL:  "  ATTACH 'ducklake:x' AS lake  ",
	})
	want := "SET memory_limit = '2GB';\nSET search_path = 'it''s';\nSET threads = '4';\nATTACH 'ducklake:x' AS lake;\n"
	if sql != want {
		t.Fatalf("got\n%s\nwant\n%s", sql, want)
	}
	if RenderInitSQL(config.Profile{}) != "" {
		t.Fatal("empty profile must render nothing")
	}
}
