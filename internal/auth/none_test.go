package auth

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"quackmux/internal/config"
)

func TestNoneAuthenticate(t *testing.T) {
	a, err := newNone(config.NoneAuthConfig{DefaultUser: "dev", DefaultGroups: []string{"devs", "all", "devs"}}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	tests := []struct {
		name       string
		credential string
		opts       url.Values
		wantUser   string
		wantErr    bool
	}{
		{name: "default user", credential: "anything", wantUser: "dev"},
		{name: "empty credential", credential: "", wantUser: "dev"},
		{name: "nil options", credential: "x", opts: nil, wantUser: "dev"},
		{name: "user option", credential: "x", opts: url.Values{"user": {"alice"}}, wantUser: "alice"},
		{name: "user option with all allowed characters", opts: url.Values{"user": {"A.b_c@d-9"}}, wantUser: "A.b_c@d-9"},
		{name: "empty user option", opts: url.Values{"user": {""}}, wantUser: "dev"},
		{name: "64 characters", opts: url.Values{"user": {strings.Repeat("u", 64)}}, wantUser: strings.Repeat("u", 64)},
		{name: "65 characters", opts: url.Values{"user": {strings.Repeat("u", 65)}}, wantErr: true},
		{name: "space", opts: url.Values{"user": {"alice smith"}}, wantErr: true},
		{name: "slash", opts: url.Values{"user": {"../etc"}}, wantErr: true},
		{name: "non-ASCII", opts: url.Values{"user": {"jürgen"}}, wantErr: true},
		{name: "control character", opts: url.Values{"user": {"alice\r\n"}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := a.Authenticate(context.Background(), Request{Credential: tt.credential, Options: tt.opts})
			if tt.wantErr {
				if !errors.Is(err, ErrUnauthenticated) {
					t.Fatalf("error = %v, want ErrUnauthenticated", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := Identity{User: tt.wantUser, Groups: []string{"devs", "all"}, Method: MethodNone}
			if !reflect.DeepEqual(id, want) {
				t.Fatalf("identity = %+v, want %+v", id, want)
			}
		})
	}
}

func TestNoneDefaults(t *testing.T) {
	a, err := newNone(config.NoneAuthConfig{}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	id, err := a.Authenticate(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if id.User != "anonymous" || id.Groups != nil || id.Method != "none" {
		t.Fatalf("identity = %+v", id)
	}
}

func TestNoneGroupsAreCopied(t *testing.T) {
	a, err := newNone(config.NoneAuthConfig{DefaultGroups: []string{"g1"}}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := a.Authenticate(context.Background(), Request{})
	id.Groups[0] = "changed"
	id2, _ := a.Authenticate(context.Background(), Request{})
	if id2.Groups[0] != "g1" {
		t.Fatalf("modifying a returned identity changed the authenticator: %v", id2.Groups)
	}
}

func TestNoneConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.NoneAuthConfig
		wantErr string
	}{
		{name: "bad default user", cfg: config.NoneAuthConfig{DefaultUser: " root"}, wantErr: "auth.none.defaultUser"},
		{name: "empty group", cfg: config.NoneAuthConfig{DefaultGroups: []string{"a", ""}}, wantErr: "auth.none.defaultGroups[1]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newNone(tt.cfg, discardLogger())
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestNoneLogsWarning(t *testing.T) {
	log, buf := testLogger()
	if _, err := newNone(config.NoneAuthConfig{}, log); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "development") {
		t.Fatalf("missing warning, log: %s", buf)
	}
}
