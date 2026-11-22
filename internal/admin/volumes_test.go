package admin

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"quackmux/internal/engine"
)

type fakeVolumes struct {
	vols      []engine.Volume
	deleted   []string
	err       error
	deleteErr error // returned by DeleteVolume if set
}

func (f *fakeVolumes) Volumes(context.Context) ([]engine.Volume, error) { return f.vols, f.err }

func (f *fakeVolumes) DeleteVolume(_ context.Context, name string) (engine.Volume, error) {
	if f.deleteErr != nil {
		return engine.Volume{}, f.deleteErr
	}
	for _, v := range f.vols {
		if v.Name != name {
			continue
		}
		if v.Engine != "" {
			return v, fmt.Errorf("%w: engine %s exists", engine.ErrVolumeInUse, v.Engine)
		}
		f.deleted = append(f.deleted, name)
		v.Deleting = true
		return v, nil
	}
	return engine.Volume{}, engine.ErrVolumeNotFound
}

func TestVolumesAPI(t *testing.T) {
	fv := &fakeVolumes{vols: []engine.Volume{
		{Name: "quackmux-data-user-alice-default-personal-0123456789abcdef", Key: "USER/alice/default/personal#0",
			ShareLevel: "USER", Owner: "alice", Subdomain: "default", Profile: "personal", Size: "10Gi",
			CreatedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), Engine: "quack-user-alice-x7k2p"},
		{Name: "quackmux-data-user-bob-default-personal-fedcba9876543210", Key: "USER/bob/default/personal#0",
			ShareLevel: "USER", Owner: "bob", Subdomain: "default", Profile: "personal", Size: "10Gi"},
	}}
	fx := newFixture(t, func(o *Options) { o.Volumes = fv })

	r := do(t, fx.srv, http.MethodGet, "/api/v1/volumes", "")
	assertJSON(t, r)
	if r.status != http.StatusOK || decode[volumeList](t, r.body).Count != 2 {
		t.Fatalf("list: %d %s", r.status, r.body)
	}
	r = do(t, fx.srv, http.MethodGet, "/api/v1/volumes?owner=bob", "")
	if l := decode[volumeList](t, r.body); l.Count != 1 || l.Volumes[0].Owner != "bob" {
		t.Fatalf("filtered list: %s", r.body)
	}
	if r := do(t, fx.srv, http.MethodGet, "/api/v1/volumes?user=bob", ""); r.status != http.StatusBadRequest {
		t.Errorf("unknown parameter: %d", r.status)
	}

	r = do(t, fx.srv, http.MethodDelete, "/api/v1/volumes/"+fv.vols[0].Name, "")
	if r.status != http.StatusConflict || !strings.Contains(errorMessage(t, r.body), "quack-user-alice-x7k2p") {
		t.Errorf("delete in use: %d %s", r.status, r.body)
	}
	r = do(t, fx.srv, http.MethodDelete, "/api/v1/volumes/"+fv.vols[1].Name, "")
	if r.status != http.StatusAccepted || !decode[volumeDeleted](t, r.body).Volume.Deleting || len(fv.deleted) != 1 {
		t.Errorf("delete: %d %s", r.status, r.body)
	}
	if r := do(t, fx.srv, http.MethodDelete, "/api/v1/volumes/missing", ""); r.status != http.StatusNotFound {
		t.Errorf("delete missing: %d", r.status)
	}
	if r := do(t, fx.srv, http.MethodPost, "/api/v1/volumes/x", ""); r.status != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", r.status)
	}

	// Without volume support: an empty list, nothing to delete.
	fx = newFixture(t)
	r = do(t, fx.srv, http.MethodGet, "/api/v1/volumes", "")
	if r.status != http.StatusOK || string(r.body) == "" || decode[volumeList](t, r.body).Volumes == nil {
		t.Errorf("list without volumes: %d %s", r.status, r.body)
	}
	if r := do(t, fx.srv, http.MethodDelete, "/api/v1/volumes/x", ""); r.status != http.StatusNotFound {
		t.Errorf("delete without volumes: %d", r.status)
	}
}

// Only a leader with a complete registry knows whether an engine uses a
// volume: anything else answers 503, and a failing API 502.
func TestDeleteVolumeUnavailable(t *testing.T) {
	fv := &fakeVolumes{}
	fx := newFixture(t, func(o *Options) { o.Volumes = fv })
	for err, want := range map[error]int{
		engine.ErrNotLeader:                        http.StatusServiceUnavailable,
		engine.ErrNotSynced:                        http.StatusServiceUnavailable,
		fmt.Errorf("connection refused"):           http.StatusBadGateway,
		fmt.Errorf("x: %w", engine.ErrVolumeInUse): http.StatusConflict,
	} {
		fv.deleteErr = err
		if r := do(t, fx.srv, http.MethodDelete, "/api/v1/volumes/quackmux-data-x", ""); r.status != want ||
			!strings.Contains(errorMessage(t, r.body), err.Error()) {
			t.Errorf("%v: %d %s, want %d", err, r.status, r.body, want)
		}
	}
	fv.deleteErr, fv.err = nil, fmt.Errorf("forbidden")
	if r := do(t, fx.srv, http.MethodGet, "/api/v1/volumes", ""); r.status != http.StatusBadGateway {
		t.Errorf("list with a failing API: %d %s", r.status, r.body)
	}
	// A follower (or a gateway still starting) knows no engines: its list
	// would show every volume unused, so it answers 503 instead.
	for _, err := range []error{engine.ErrNotLeader, engine.ErrNotSynced} {
		fv.err = err
		if r := do(t, fx.srv, http.MethodGet, "/api/v1/volumes", ""); r.status != http.StatusServiceUnavailable ||
			!strings.Contains(errorMessage(t, r.body), err.Error()) {
			t.Errorf("list with %v: %d %s, want 503", err, r.status, r.body)
		}
	}
}
