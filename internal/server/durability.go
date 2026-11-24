package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/velocor/velocor/internal/hardware"
	"github.com/velocor/velocor/internal/store"
)

// handleUpdateDisk changes a disk's operator settings: the power-loss-protection declaration and
// the write cache policy (docs/review-history.md P0-4). Disabling the cache takes effect at once and is then
// enforced on every reconciler pass; going back to "leave" switches the cache on again.
func (s *Server) handleUpdateDisk(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PowerLossProtected *bool                   `json:"power_loss_protected"`
		WriteCachePolicy   *store.WriteCachePolicy `json:"write_cache_policy"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	id := r.PathValue("id")
	d, err := s.store.GetDisk(id)
	if err != nil {
		writeError(w, err)
		return
	}
	if d.IsBootDevice {
		writeError(w, fmt.Errorf("%w: %s is a boot device; its cache is the operating system's business", store.ErrInvalidArgument, id))
		return
	}
	if req.WriteCachePolicy != nil {
		switch *req.WriteCachePolicy {
		case store.WriteCachePolicyLeave, store.WriteCachePolicyDisabled:
		default:
			writeError(w, fmt.Errorf("%w: write_cache_policy must be \"\" or %q", store.ErrInvalidArgument, store.WriteCachePolicyDisabled))
			return
		}
		if *req.WriteCachePolicy != d.WriteCachePolicy {
			target := *d
			target.WriteCachePolicy = *req.WriteCachePolicy
			state, err := s.applyWriteCachePolicy(r.Context(), &target)
			if err != nil {
				writeError(w, err)
				return
			}
			d.WriteCache = state
		}
	}
	updated, err := s.store.MutateDisk(id, func(cur *store.Disk) error {
		if req.PowerLossProtected != nil {
			cur.PowerLossProtected = *req.PowerLossProtected
		}
		if req.WriteCachePolicy != nil {
			cur.WriteCachePolicy = *req.WriteCachePolicy
			cur.WriteCache = d.WriteCache
			cur.WriteCacheError = "" // applied (or LEAVE): nothing outstanding
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"disk": updated})
}

// applyWriteCachePolicy makes the drive match d.WriteCachePolicy and returns its cache state.
// An NVMe drive is switched by the reconciler once it is claimed; before that its policy is only
// recorded, and applied when it is claimed.
func (s *Server) applyWriteCachePolicy(ctx context.Context, d *store.Disk) (store.WriteCacheState, error) {
	if d.Type == store.DiskTypeNVMe {
		if !d.Claimed || d.Handle == "" {
			return d.WriteCache, nil
		}
		if err := s.requireDataPlane(); err != nil {
			return d.WriteCache, err
		}
		state, err := s.dp.SetNVMeWriteCache(ctx, d, d.WriteCachePolicy != store.WriteCachePolicyDisabled)
		if err != nil {
			return state, fmt.Errorf("%w: %v", store.ErrInvalidArgument, err)
		}
		return state, nil
	}
	if s.hw == nil {
		return d.WriteCache, errors.New("hardware access is not available")
	}
	state, err := s.hw.SetWriteCache(d, d.WriteCachePolicy != store.WriteCachePolicyDisabled)
	if errors.Is(err, hardware.ErrWriteCacheUnsupported) {
		return d.WriteCache, fmt.Errorf("%w: %v", store.ErrInvalidArgument, err)
	}
	return state, err
}

// durabilityWarnings flags disks that can lose acknowledged writes on power failure.
func (s *Server) durabilityWarnings() []string {
	disks, err := s.store.ListDisks()
	if err != nil {
		return nil
	}
	var atRisk, refused, unknown []string
	refusal := ""
	for _, d := range disks {
		if !d.Claimed && d.PoolID == "" {
			continue
		}
		switch d.Durability() {
		case store.DurabilityAtRisk:
			if d.WriteCacheError != "" {
				refused = append(refused, d.Name)
				if refusal == "" {
					refusal = d.WriteCacheError
				}
				continue
			}
			atRisk = append(atRisk, d.Name)
		case store.DurabilityUnknown:
			unknown = append(unknown, d.Name)
		}
	}
	var out []string
	slices.Sort(refused)
	slices.Sort(atRisk)
	slices.Sort(unknown)
	if len(refused) > 0 {
		out = append(out, fmt.Sprintf("The write cache of %s can't be turned off (%s), so power loss can lose acknowledged writes. Declare the disks power-loss protected if they are (e.g. a virtual disk whose host protects its cache), or set their write-cache policy to leave the drive alone.", strings.Join(refused, ", "), refusal))
	}
	if len(atRisk) > 0 {
		out = append(out, fmt.Sprintf("Power loss can lose acknowledged writes on %s: the volatile write cache is on and the disk isn't declared power-loss protected. Disable the cache, or declare the disk protected if it is.", strings.Join(atRisk, ", ")))
	}
	if len(unknown) > 0 {
		out = append(out, fmt.Sprintf("The write cache of %s could not be determined.", strings.Join(unknown, ", ")))
	}
	return out
}
