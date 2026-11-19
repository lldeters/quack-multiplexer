package auth

import (
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"quackmux/internal/config"
)

func TestParseToken(t *testing.T) {
	tests := []struct {
		name    string
		token   string
		cred    string
		opts    url.Values
		wantErr string
	}{
		{name: "no options", token: "secret", cred: "secret", opts: url.Values{}},
		{name: "empty token", token: "", cred: "", opts: url.Values{}},
		{name: "empty suffix", token: "secret?", cred: "secret", opts: url.Values{}},
		{
			name: "options", token: "secret?share_level=connection&profile=large", cred: "secret",
			opts: url.Values{"share_level": {"connection"}, "profile": {"large"}},
		},
		{
			name: "names are normalized, values are not", token: "s?Share-Level=SERVER&%20PROFILE%20=Large&SUB-DOMAIN=etl", cred: "s",
			opts: url.Values{"share_level": {"SERVER"}, "profile": {"Large"}, "sub_domain": {"etl"}},
		},
		{
			name: "spellings of one name keep their order", token: "s?Profile=a&profile=b&PROFILE=c", cred: "s",
			opts: url.Values{"profile": {"a", "b", "c"}},
		},
		{
			name: "value escapes", token: "s?user=alice%40example.com&x=a+b&y=a%2Bb", cred: "s",
			opts: url.Values{"user": {"alice@example.com"}, "x": {"a b"}, "y": {"a+b"}},
		},
		{name: "name without value", token: "s?flag", cred: "s", opts: url.Values{"flag": {""}}},
		{name: "empty pairs are skipped", token: "s?&&a=1&", cred: "s", opts: url.Values{"a": {"1"}}},
		{name: "credential with padding", token: "YWJj==?a=1", cred: "YWJj==", opts: url.Values{"a": {"1"}}},
		{name: "question mark in value", token: "s?a=1?2", cred: "s", opts: url.Values{"a": {"1?2"}}},
		{name: "only options", token: "?a=1", cred: "", opts: url.Values{"a": {"1"}}},
		{name: "bad escape in value", token: "s?a=%zz", wantErr: "invalid URL escape"},
		{name: "bad escape in name", token: "s?%g1=1", wantErr: "invalid URL escape"},
		{name: "truncated escape", token: "s?a=%4", wantErr: "invalid URL escape"},
		{name: "semicolon separator", token: "s?a=1;b=2", wantErr: "semicolon"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cred, opts, err := ParseToken(tt.token)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseToken(%q) error = %v, want %q", tt.token, err, tt.wantErr)
				}
				if !strings.HasPrefix(err.Error(), "invalid options in token") {
					t.Errorf("error %q lacks the common prefix", err)
				}
				if cred != "" || opts != nil {
					t.Errorf("on error got credential %q and options %v, want zero values", cred, opts)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseToken(%q): %v", tt.token, err)
			}
			if cred != tt.cred {
				t.Errorf("credential = %q, want %q", cred, tt.cred)
			}
			if !reflect.DeepEqual(opts, tt.opts) {
				t.Errorf("options = %v, want %v", opts, tt.opts)
			}
		})
	}
}

// ParseToken must accept and decode exactly what url.ParseQuery does.
func TestParseTokenMatchesParseQuery(t *testing.T) {
	queries := []string{
		"a=1", "a=1&b=2", "a=1&a=2", "a", "a=", "=1", "&", "a=1&&b=2", "a=%41%42", "a=b+c", "a%3Db=c",
		"x=%zz", "x=%", "%=1", "a=1;b=2", "a=1&b=%2", "k=v=w", "%E2%82%AC=%E2%82%AC",
	}
	for _, q := range queries {
		want, werr := url.ParseQuery(q)
		_, got, gerr := ParseToken("tok?" + q)
		if (werr == nil) != (gerr == nil) {
			t.Errorf("query %q: ParseQuery error %v, ParseToken error %v", q, werr, gerr)
			continue
		}
		if werr != nil {
			continue
		}
		norm := url.Values{}
		for k, v := range want {
			n := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(k)), "-", "_")
			norm[n] = append(norm[n], v...)
		}
		if !reflect.DeepEqual(got, norm) {
			t.Errorf("query %q: ParseToken options %v, want %v", q, got, norm)
		}
	}
}

func TestParseTokenErrorOmitsCredential(t *testing.T) {
	_, _, err := ParseToken("super-secret-credential?share_level=%zz")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "super-secret-credential") {
		t.Fatalf("error %q contains the credential", err)
	}
}

func TestIdentityInGroup(t *testing.T) {
	id := Identity{User: "alice", Groups: []string{"a", "b"}}
	for g, want := range map[string]bool{"a": true, "b": true, "c": false, "": false} {
		if got := id.InGroup(g); got != want {
			t.Errorf("InGroup(%q) = %v, want %v", g, got, want)
		}
	}
	if (Identity{}).InGroup("a") {
		t.Error("an identity without groups is in no group")
	}
}

func TestNew(t *testing.T) {
	dir := t.TempDir()
	users := writeFile(t, dir, "users.yaml", []byte("users: []\n"))
	secret := writeFile(t, dir, "secret", []byte("0123456789abcdef0123456789abcdef\n"))
	tests := []struct {
		name    string
		cfg     config.AuthConfig
		want    any
		wantErr string
	}{
		{name: "none", cfg: config.AuthConfig{Mode: "none"}, want: &noneAuthenticator{}},
		{name: "mode is case-insensitive", cfg: config.AuthConfig{Mode: " None "}, want: &noneAuthenticator{}},
		{name: "static", cfg: config.AuthConfig{Mode: "static", Static: config.StaticAuthConfig{File: users, ReloadInterval: config.D(0)}}, want: &staticAuthenticator{}},
		{name: "jwt", cfg: config.AuthConfig{Mode: "jwt", JWT: config.JWTAuthConfig{HMACSecretFile: secret}}, want: &jwtAuthenticator{}},
		{name: "empty mode", cfg: config.AuthConfig{}, wantErr: "auth.mode must be none, static or jwt"},
		{name: "unknown mode", cfg: config.AuthConfig{Mode: "ldap"}, wantErr: `"ldap"`},
		{name: "static without file", cfg: config.AuthConfig{Mode: "static"}, wantErr: "auth.static.file is required"},
		{name: "static with missing file", cfg: config.AuthConfig{Mode: "static", Static: config.StaticAuthConfig{File: dir + "/missing.yaml"}}, wantErr: "missing.yaml"},
		{name: "jwt without keys", cfg: config.AuthConfig{Mode: "jwt"}, wantErr: "needs hmacSecretFile, publicKeyFiles or jwksURL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := New(tt.cfg, nil) // a nil logger must work
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("New error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			if reflect.TypeOf(a) != reflect.TypeOf(tt.want) {
				t.Fatalf("New returned %T, want %T", a, tt.want)
			}
		})
	}
}

func TestCheckName(t *testing.T) {
	tests := []struct {
		in      string
		wantErr string
	}{
		{"alice", ""},
		{"Alice Smith", ""},
		{"alice+ci@example.com", ""},
		{"/realm/admins", ""},
		{"Jürgen", ""},
		{strings.Repeat("a", maxNameLen), ""},
		{"", "must not be empty"},
		{strings.Repeat("a", maxNameLen+1), "longer than"},
		{"\xff\xfe", "not valid UTF-8"},
		{" alice", "white space"},
		{"alice\n", "white space"},
		{"ali\x00ce", "control characters"},
		{"ali\u0085ce", "control characters"},
	}
	for _, tt := range tests {
		err := checkName(tt.in)
		switch {
		case tt.wantErr == "" && err != nil:
			t.Errorf("checkName(%q) = %v, want nil", tt.in, err)
		case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
			t.Errorf("checkName(%q) = %v, want %q", tt.in, err, tt.wantErr)
		}
	}
}

func TestValidOptionUser(t *testing.T) {
	tests := map[string]bool{
		"alice":                     true,
		"Alice.Smith_01@corp-x.com": true,
		"a":                         true,
		strings.Repeat("x", 64):     true,
		strings.Repeat("x", 65):     false,
		"":                          false,
		"alice smith":               false,
		"alice+ci":                  false,
		"al/ice":                    false,
		"Jürgen":                    false,
		"alice\n":                   false,
	}
	for in, want := range tests {
		if got := validOptionUser(in); got != want {
			t.Errorf("validOptionUser(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestDedupe(t *testing.T) {
	if got := dedupe(nil); got != nil {
		t.Errorf("dedupe(nil) = %v", got)
	}
	if got := dedupe([]string{"b", "a", "b", "c", "a"}); !reflect.DeepEqual(got, []string{"b", "a", "c"}) {
		t.Errorf("dedupe = %v", got)
	}
}

func TestDurationOr(t *testing.T) {
	if got := durationOr(config.Duration{}, time.Minute); got != time.Minute {
		t.Errorf("unset: %s", got)
	}
	if got := durationOr(config.D(0), time.Minute); got != 0 {
		t.Errorf("explicit 0: %s", got)
	}
	if got := durationOr(config.D(time.Second), time.Minute); got != time.Second {
		t.Errorf("explicit 1s: %s", got)
	}
}

func TestUnauthenticatedWraps(t *testing.T) {
	err := unauthenticated("user %q is disabled", "bob")
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("not an ErrUnauthenticated")
	}
	if want := `authentication failed: user "bob" is disabled`; err.Error() != want {
		t.Fatalf("got %q, want %q", err, want)
	}
}
