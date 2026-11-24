package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/store"
)

// Deleting a volume (P1-4) moves it to a recycle state for the configured retention period:
// it disappears from the API, can't be exported, snapshotted or resized, and its lvol stays
// untouched, so an administrator can restore it. The reconciler purges it when the period
// ends; an administrator can purge it sooner. Volumes with deletion protection can't be
// deleted at all until an administrator turns the protection off.

// liveVolume returns a volume that is not deleted; a deleted one is reported as not found.
func (s *Server) liveVolume(id string) (*store.Volume, error) {
	vol, err := s.store.GetVolume(id)
	if err != nil {
		return nil, err
	}
	if vol.Deleted() {
		return nil, fmt.Errorf("%w: volume %s is deleted", store.ErrNotFound, id)
	}
	return vol, nil
}

// liveSnapshot returns a snapshot that is not deleted; a deleted one is reported as not found.
func (s *Server) liveSnapshot(id string) (*store.Snapshot, error) {
	snap, err := s.store.GetSnapshot(id)
	if err != nil {
		return nil, err
	}
	if snap.Deleted() {
		return nil, fmt.Errorf("%w: snapshot %s is deleted", store.ErrNotFound, id)
	}
	return snap, nil
}

func (s *Server) volumeRetention() time.Duration {
	st, err := s.store.GetSettings()
	if err != nil {
		return store.DefaultVolumeRetention
	}
	return st.VolumeRetention()
}

// handleDeleteVolume: DELETE /api/v1/volumes/{id}. With a retention period (the default) the
// volume is kept and hidden; without one it is deleted at once. Deleting a deleted volume
// succeeds without changing anything (CSI retries).
func (s *Server) handleDeleteVolume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	vol, err := s.store.GetVolume(id)
	if err != nil {
		writeError(w, err)
		return
	}
	if vol.Deleted() {
		writeJSON(w, http.StatusOK, map[string]any{"volume": vol})
		return
	}
	retention := s.volumeRetention()
	if retention == 0 {
		if err := s.store.CheckVolumeDeletable(id); err != nil {
			writeError(w, err)
			return
		}
		if err := s.purgeVolume(r.Context(), vol); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	now := time.Now()
	trashed, err := s.store.TrashVolume(id, now, now.Add(retention), subjectFromContext(r.Context()))
	if err != nil {
		writeError(w, err)
		return
	}
	s.logger.Info("volume deleted; kept for recovery until purge time",
		"volume", id, "name", trashed.Name, "by", trashed.DeletedBy, "purge_at", trashed.PurgeAt)
	writeJSON(w, http.StatusOK, map[string]any{"volume": trashed})
}

// purgeVolume deletes a volume's lvol and record for good.
func (s *Server) purgeVolume(ctx context.Context, vol *store.Volume) error {
	if err := s.requireDataPlane(); err != nil {
		return err
	}
	if err := s.dp.DeleteVolume(ctx, vol.UUID); err != nil && !dataplane.IsNotFound(err) {
		return fmt.Errorf("delete the volume: %w", err)
	}
	if err := s.store.DeleteVolume(vol.ID); err != nil {
		return err
	}
	s.updatePoolSpace(vol.PoolID)
	return nil
}

// handleListDeletedVolumes: GET /api/v1/deleted-volumes[?pool_id=].
func (s *Server) handleListDeletedVolumes(w http.ResponseWriter, r *http.Request) {
	vols, err := s.store.ListVolumes(r.URL.Query().Get("pool_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	out := []*store.Volume{}
	for _, v := range vols {
		if v.Deleted() {
			out = append(out, v)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"volumes": out, "retention_hours": s.volumeRetention().Hours()})
}

// handleRestoreVolume: POST /api/v1/deleted-volumes/{id}/restore {name?}.
func (s *Server) handleRestoreVolume(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.Name != "" {
		if err := validateDisplayName("volume", req.Name); err != nil {
			writeError(w, err)
			return
		}
	}
	vol, err := s.store.RestoreVolume(r.PathValue("id"), req.Name)
	if err != nil {
		writeError(w, err)
		return
	}
	s.logger.Info("deleted volume restored", "volume", vol.ID, "name", vol.Name, "by", subjectFromContext(r.Context()))
	writeJSON(w, http.StatusOK, map[string]any{"volume": vol})
}

// handlePurgeVolume: DELETE /api/v1/deleted-volumes/{id}, which deletes a deleted volume's
// data now instead of at the end of the retention period.
func (s *Server) handlePurgeVolume(w http.ResponseWriter, r *http.Request) {
	vol, err := store.StartPurge(s.store, r.PathValue("id"), time.Time{})
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.purgeVolume(r.Context(), vol); err != nil {
		writeError(w, err)
		return
	}
	s.logger.Warn("deleted volume purged", "volume", vol.ID, "name", vol.Name, "by", subjectFromContext(r.Context()))
	writeJSON(w, http.StatusOK, map[string]any{})
}

// handleSetVolumeProtection: PUT /api/v1/volumes/{id}/protection {deletion_protection}.
func (s *Server) handleSetVolumeProtection(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeletionProtection *bool `json:"deletion_protection"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.DeletionProtection == nil {
		writeError(w, fmt.Errorf("%w: body must be {\"deletion_protection\": true|false}", store.ErrInvalidArgument))
		return
	}
	id := r.PathValue("id")
	if _, err := s.liveVolume(id); err != nil {
		writeError(w, err)
		return
	}
	updated, err := s.store.MutateVolume(id, func(v *store.Volume) error {
		if v.Deleted() {
			return fmt.Errorf("%w: volume %s is deleted", store.ErrNotFound, id)
		}
		v.DeletionProtection = *req.DeletionProtection
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"volume": updated})
}

// handleGetVolumeRetention / handlePutVolumeRetention: GET|PUT /api/v1/system/volume-retention
// {hours} (0 = delete volumes at once). A change applies to volumes deleted afterwards.
func (s *Server) handleGetVolumeRetention(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"hours": s.volumeRetention().Hours()})
}

func (s *Server) handlePutVolumeRetention(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hours *uint32 `json:"hours"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Hours == nil || *req.Hours > store.MaxVolumeRetentionHours {
		writeError(w, fmt.Errorf("%w: body must be {\"hours\": 0..%d} (0 = delete volumes at once)", store.ErrInvalidArgument, store.MaxVolumeRetentionHours))
		return
	}
	st, err := s.store.GetSettings()
	if err != nil {
		writeError(w, err)
		return
	}
	h := *req.Hours
	st.VolumeRetentionHours = &h
	if err := s.store.SaveSettings(st); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hours": h})
}
