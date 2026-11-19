package server

// Endpoints for editing who may reach an export, seeing who is connected, and live volume
// usage. Access changes are applied to SPDK before (or, for shared initiator groups, right
// after) they are persisted, and never leave an export open to everyone by accident.

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/store"
)

// handleUpdateExport changes an export's initiator groups and/or allow_any_host on a live export.
func (s *Server) handleUpdateExport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		InitiatorGroupIDs *[]string `json:"initiator_group_ids"`
		AllowAnyHost      *bool     `json:"allow_any_host"`
		// node_ids: the CSI nodes of a shared export, changed together with their groups.
		NodeIDs *[]string `json:"node_ids"`
		// enabled: serve (true) or stop serving (false) the export.
		Enabled *bool `json:"enabled"`
		// portal_ids: the portals it's served on (replaces the list).
		PortalIDs *[]string `json:"portal_ids"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.InitiatorGroupIDs == nil && req.AllowAnyHost == nil && req.NodeIDs == nil && req.Enabled == nil && req.PortalIDs == nil {
		writeError(w, fmt.Errorf("%w: nothing to change; set initiator_group_ids, allow_any_host, portal_ids or enabled", store.ErrInvalidArgument))
		return
	}
	exp, err := s.store.GetExport(id)
	if err != nil {
		writeError(w, err)
		return
	}
	vol, err := s.store.GetVolume(exp.VolumeID)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.requireSPDK(); err != nil {
		writeError(w, err)
		return
	}
	if req.NodeIDs != nil && !exp.Shared {
		writeError(w, fmt.Errorf("%w: node_ids can only be changed on a shared export", store.ErrInvalidArgument))
		return
	}
	updated, err := dataplane.UpdateExport(r.Context(), s.spdk, s.store, id, vol, func(e *store.Export) error {
		if req.NodeIDs != nil {
			e.NodeIDs = slices.Compact(slices.Sorted(slices.Values(*req.NodeIDs)))
		}
		if req.InitiatorGroupIDs != nil {
			e.InitiatorGroupIDs = slices.Compact(slices.Sorted(slices.Values(*req.InitiatorGroupIDs)))
		}
		if req.AllowAnyHost != nil {
			e.AllowAnyHost = *req.AllowAnyHost
		}
		if req.PortalIDs != nil {
			ids := slices.Compact(slices.Sorted(slices.Values(*req.PortalIDs)))
			if len(ids) == 0 {
				return fmt.Errorf("%w: an export needs at least one portal", store.ErrInvalidArgument)
			}
			for _, id := range ids {
				p, err := s.store.GetPortal(id)
				if err != nil {
					return fmt.Errorf("%w: portal %s: %v", store.ErrInvalidArgument, id, err)
				}
				if p.Protocol != e.Protocol {
					return fmt.Errorf("%w: portal %s is %s, the export is %s", store.ErrInvalidArgument, id, p.Protocol, e.Protocol)
				}
			}
			e.PortalIDs = ids
		}
		if req.Enabled != nil {
			e.Disabled = !*req.Enabled
			e.DisabledReason = ""
			if e.Disabled {
				e.DisabledReason = "disabled by an administrator"
			}
		}
		acl, err := dataplane.ResolveACL(s.store, e.InitiatorGroupIDs)
		if err != nil {
			return fmt.Errorf("%w: %v", store.ErrInvalidArgument, err)
		}
		if err := dataplane.CheckACL(e, acl); err != nil {
			return fmt.Errorf("%w: %v", store.ErrInvalidArgument, err)
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"export": updated})
}

func (s *Server) handleExportConnections(w http.ResponseWriter, r *http.Request) {
	exp, err := s.store.GetExport(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.requireSPDK(); err != nil {
		writeError(w, err)
		return
	}
	conns, err := dataplane.Connections(r.Context(), s.spdk, exp)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"connections": conns})
}

// handleUpdateInitiatorGroup edits a group's members and re-applies every export that uses
// it. A change that would leave an export (without allow_any_host) reachable by nobody is
// refused up front: such an export could not be converged, and SPDK would keep the old ACL.
func (s *Server) handleUpdateInitiatorGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		HostIDs       *[]string `json:"host_ids"`
		HostNQNs      *[]string `json:"host_nqns"`
		InitiatorIQNs *[]string `json:"initiator_iqns"`
		AllowedCIDRs  *[]string `json:"allowed_cidrs"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.HostNQNs != nil {
		for _, nqn := range *req.HostNQNs {
			if err := dataplane.ValidateNQN(strings.TrimSpace(nqn)); err != nil {
				writeError(w, fmt.Errorf("%w: %v", store.ErrInvalidArgument, err))
				return
			}
		}
	}
	current, err := s.store.GetInitiatorGroup(id)
	if err != nil {
		writeError(w, err)
		return
	}
	apply := func(g *store.InitiatorGroup) {
		set := func(dst *[]string, v *[]string) {
			if v != nil {
				clean := make([]string, 0, len(*v))
				for _, x := range *v {
					if x = strings.TrimSpace(x); x != "" && !slices.Contains(clean, x) {
						clean = append(clean, x)
					}
				}
				*dst = clean
			}
		}
		set(&g.HostIDs, req.HostIDs)
		set(&g.HostNQNs, req.HostNQNs)
		set(&g.InitiatorIQNs, req.InitiatorIQNs)
		set(&g.AllowedCIDRs, req.AllowedCIDRs)
	}

	// Pre-flight every dependent export against the proposed membership.
	proposed := *current
	apply(&proposed)
	exports, err := s.store.ListExports("", "")
	if err != nil {
		writeError(w, err)
		return
	}
	var dependents []*store.Export
	for _, e := range exports {
		if !slices.Contains(e.InitiatorGroupIDs, id) {
			continue
		}
		dependents = append(dependents, e)
		acl, err := s.resolveWithOverride(e.InitiatorGroupIDs, &proposed)
		if err != nil {
			writeError(w, err)
			return
		}
		if err := dataplane.CheckACL(e, acl); err != nil {
			writeError(w, fmt.Errorf("%w: export %s would have no initiators left; remove the group from it or delete the export first", store.ErrInvalidArgument, e.Name))
			return
		}
	}

	updated, err := s.store.MutateInitiatorGroup(id, func(g *store.InitiatorGroup) error {
		apply(g)
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	if len(dependents) > 0 {
		if err := s.requireSPDK(); err != nil {
			writeError(w, err)
			return
		}
	}
	var failed []string
	for _, e := range dependents {
		vol, err := s.store.GetVolume(e.VolumeID)
		if err == nil {
			err = dataplane.EnsureExport(r.Context(), s.spdk, s.store, e, vol)
		}
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", e.Name, err))
		}
	}
	if len(failed) > 0 {
		// The group is saved; the reconciler keeps retrying these exports.
		writeError(w, fmt.Errorf("initiator group saved, but applying it failed for: %s", strings.Join(failed, "; ")))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"initiator_group": updated, "exports_updated": len(dependents)})
}

// resolveWithOverride resolves an ACL as if group override had already been saved.
func (s *Server) resolveWithOverride(groupIDs []string, override *store.InitiatorGroup) (dataplane.ACL, error) {
	var others []string
	for _, gid := range groupIDs {
		if gid != override.ID {
			others = append(others, gid)
		}
	}
	acl, err := dataplane.ResolveACL(s.store, others)
	if err != nil {
		return acl, err
	}
	extra, err := dataplane.ResolveGroup(s.store, override)
	if err != nil {
		return acl, err
	}
	acl.HostNQNs = append(acl.HostNQNs, extra.HostNQNs...)
	acl.IQNs = append(acl.IQNs, extra.IQNs...)
	acl.CIDRs = append(acl.CIDRs, extra.CIDRs...)
	return acl, nil
}

// handleVolumeUsage reports how much of a volume is actually allocated in its pool.
func (s *Server) handleVolumeUsage(w http.ResponseWriter, r *http.Request) {
	vol, err := s.liveVolume(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.requireSPDK(); err != nil {
		writeError(w, err)
		return
	}
	pool, err := s.store.GetPool(vol.PoolID)
	if err != nil {
		writeError(w, err)
		return
	}
	lvols, err := s.spdk.BdevLvolGetLvols(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	cluster := uint64(pool.ClusterSizeBytes)
	if stores, err := s.spdk.BdevLvolGetLvstores(r.Context(), "", pool.LVSUUID); err == nil && len(stores) > 0 && stores[0].ClusterSize > 0 {
		cluster = uint64(stores[0].ClusterSize)
	}
	for _, l := range lvols {
		if l.UUID == vol.UUID {
			writeJSON(w, http.StatusOK, map[string]any{
				"size_bytes":      vol.SizeBytes,
				"allocated_bytes": l.NumAllocatedClusters * cluster,
				"thin_provision":  l.IsThinProvisioned,
			})
			return
		}
	}
	writeError(w, fmt.Errorf("%w: lvol %s is not present in SPDK", store.ErrNotFound, vol.UUID))
}
