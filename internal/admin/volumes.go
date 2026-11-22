package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"quackmux/internal/engine"
)

// Volumes is the part of the engine manager that manages the data volumes of
// profiles with persistence (one per engine key).
type Volumes interface {
	// Volumes lists the data volumes, each with the engine of its key. It
	// fails with engine.ErrNotLeader or engine.ErrNotSynced when this
	// replica does not know the engines.
	Volumes(ctx context.Context) ([]engine.Volume, error)
	// DeleteVolume deletes a data volume whose key has no engine. It fails
	// with engine.ErrVolumeNotFound, engine.ErrVolumeInUse,
	// engine.ErrNotLeader or engine.ErrNotSynced where that applies.
	DeleteVolume(ctx context.Context, name string) (engine.Volume, error)
}

var _ Volumes = (*engine.Manager)(nil)

// volumeTimeout bounds the Kubernetes calls of a volume request.
const volumeTimeout = 30 * time.Second

// volumeList is the response of GET /api/v1/volumes.
type volumeList struct {
	Volumes []engine.Volume `json:"volumes"`
	Count   int             `json:"count"`
}

// volumeDeleted is the response of DELETE /api/v1/volumes/{name}.
type volumeDeleted struct {
	Volume engine.Volume `json:"volume"`
}

func (s *Server) handleListVolumes(w http.ResponseWriter, r *http.Request) {
	q, ok := s.query(w, r, "owner")
	if !ok {
		return
	}
	out := []engine.Volume{}
	if s.volumes != nil {
		ctx, cancel := context.WithTimeout(r.Context(), volumeTimeout)
		defer cancel()
		vols, err := s.volumes.Volumes(ctx)
		switch {
		case errors.Is(err, engine.ErrNotLeader), errors.Is(err, engine.ErrNotSynced):
			// Without its engines the list would show every volume unused.
			s.writeError(w, http.StatusServiceUnavailable, "list data volumes: "+err.Error())
			return
		case err != nil:
			s.writeError(w, http.StatusBadGateway, "listing data volumes failed: "+err.Error())
			return
		}
		_, filter := q["owner"]
		for _, v := range vols {
			if !filter || v.Owner == q.Get("owner") {
				out = append(out, v)
			}
		}
	}
	s.writeJSON(w, http.StatusOK, volumeList{Volumes: out, Count: len(out)})
}

func (s *Server) handleDeleteVolume(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.query(w, r); !ok {
		return
	}
	name := r.PathValue("name")
	if s.volumes == nil {
		s.writeError(w, http.StatusNotFound, fmt.Sprintf("data volume %q not found", name))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), volumeTimeout)
	defer cancel()
	v, err := s.volumes.DeleteVolume(ctx, name)
	switch {
	case err == nil:
		s.log.Info("data volume deleted by operator", "volume", name, "key", v.Key, "remote", r.RemoteAddr)
		s.writeJSON(w, http.StatusAccepted, volumeDeleted{Volume: v})
	case errors.Is(err, engine.ErrVolumeNotFound):
		s.writeError(w, http.StatusNotFound, fmt.Sprintf("data volume %q not found", name))
	case errors.Is(err, engine.ErrVolumeInUse):
		s.writeError(w, http.StatusConflict, fmt.Sprintf("delete data volume %q: %v", name, err))
	case errors.Is(err, engine.ErrNotLeader), errors.Is(err, engine.ErrNotSynced):
		// Only a leader with a complete registry knows whether an engine uses
		// the volume.
		s.writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("delete data volume %q: %v", name, err))
	default:
		s.log.Warn("deleting a data volume failed", "volume", name, "err", err)
		s.writeError(w, http.StatusBadGateway, fmt.Sprintf("delete data volume %q: %v", name, err))
	}
}
