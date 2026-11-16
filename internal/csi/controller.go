package csi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/velocor/velocor/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	defaultVolumeSizeBytes int64 = 1 << 30 // 1 GiB
	labelCSINodeID               = "csi.velocor.io/node-id"
	labelCSISourceVolume         = "csi.velocor.io/source-volume"
)

// ControllerServer implements the CSI Controller service.
type ControllerServer struct {
	csi.UnimplementedControllerServer
	client     VelocorClient
	knownNodes map[string]bool
	mu         sync.Mutex
}

// NewControllerServer creates a CSI ControllerServer.
func NewControllerServer(client VelocorClient, knownNodes ...string) *ControllerServer {
	kn := make(map[string]bool, len(knownNodes))
	for _, n := range knownNodes {
		if n != "" {
			kn[n] = true
		}
	}
	return &ControllerServer{client: client, knownNodes: kn}
}

func (s *ControllerServer) ControllerGetCapabilities(_ context.Context, _ *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	caps := []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
		csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
		csi.ControllerServiceCapability_RPC_LIST_VOLUMES_PUBLISHED_NODES,
		csi.ControllerServiceCapability_RPC_GET_CAPACITY,
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
		csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS,
		csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
		csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
		csi.ControllerServiceCapability_RPC_GET_VOLUME,
		csi.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
	}
	out := make([]*csi.ControllerServiceCapability, 0, len(caps))
	for _, c := range caps {
		out = append(out, &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{Type: c},
			},
		})
	}
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: out}, nil
}

func (s *ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume name is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	if err := validateVolumeCapabilities(req.GetVolumeCapabilities()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	reqBytes := req.GetCapacityRange().GetRequiredBytes()
	limBytes := req.GetCapacityRange().GetLimitBytes()
	if reqBytes < 0 || limBytes < 0 || (limBytes > 0 && reqBytes > limBytes) {
		return nil, status.Error(codes.InvalidArgument, "invalid capacity range")
	}
	if reqBytes == 0 {
		reqBytes = defaultVolumeSizeBytes
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Idempotency check by name.
	existingVols, err := s.client.ListVolumes(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list volumes: %v", err)
	}
	for _, v := range existingVols {
		if v.Name == req.GetName() {
			if int64(v.SizeBytes) < reqBytes {
				return nil, status.Errorf(codes.AlreadyExists, "volume %q already exists with smaller size (%d < %d)", req.GetName(), v.SizeBytes, reqBytes)
			}
			return &csi.CreateVolumeResponse{
				Volume: toCSIVolume(v, req.GetParameters(), req.GetVolumeContentSource()),
			}, nil
		}
	}

	var vol *store.Volume
	if src := req.GetVolumeContentSource(); src != nil {
		vol, err = s.createFromSource(ctx, req.GetName(), uint64(reqBytes), src)
		if err != nil {
			return nil, err
		}
	} else {
		poolID, err := s.selectPool(ctx, req.GetParameters()["pool"], uint64(reqBytes))
		if err != nil {
			return nil, err
		}
		thin := true
		if strings.EqualFold(req.GetParameters()["thin"], "false") {
			thin = false
		}
		vol, err = s.client.CreateVolume(ctx, CreateVolumeRequest{
			Name:          req.GetName(),
			PoolID:        poolID,
			SizeBytes:     uint64(reqBytes),
			ThinProvision: thin,
			Labels:        map[string]string{"csi.velocor.io/provisioned": "true"},
		})
		if err != nil {
			return nil, mapAPIError(err)
		}
	}

	return &csi.CreateVolumeResponse{
		Volume: toCSIVolume(vol, req.GetParameters(), req.GetVolumeContentSource()),
	}, nil
}

func (s *ControllerServer) createFromSource(ctx context.Context, name string, reqBytes uint64, src *csi.VolumeContentSource) (*store.Volume, error) {
	switch t := src.GetType().(type) {
	case *csi.VolumeContentSource_Snapshot:
		snapID := t.Snapshot.GetSnapshotId()
		if snapID == "" {
			return nil, status.Error(codes.InvalidArgument, "source snapshot_id is required")
		}
		if _, err := s.client.GetSnapshot(ctx, snapID); err != nil {
			return nil, mapAPIError(err)
		}
		vol, err := s.client.CloneSnapshot(ctx, snapID, name, map[string]string{"csi.velocor.io/provisioned": "true"})
		if err != nil {
			return nil, mapAPIError(err)
		}
		if reqBytes > vol.SizeBytes {
			vol, err = s.client.ResizeVolume(ctx, vol.ID, reqBytes)
			if err != nil {
				return nil, mapAPIError(err)
			}
		}
		return vol, nil

	case *csi.VolumeContentSource_Volume:
		srcVolID := t.Volume.GetVolumeId()
		if srcVolID == "" {
			return nil, status.Error(codes.InvalidArgument, "source volume_id is required")
		}
		if _, err := s.client.GetVolume(ctx, srcVolID); err != nil {
			return nil, mapAPIError(err)
		}
		snapName := "csi-clone-" + name
		snap, err := s.client.CreateSnapshot(ctx, srcVolID, snapName, map[string]string{labelCSISourceVolume: srcVolID})
		if err != nil {
			return nil, mapAPIError(err)
		}
		vol, err := s.client.CloneSnapshot(ctx, snap.ID, name, map[string]string{
			"csi.velocor.io/provisioned": "true",
			labelCSISourceVolume:         srcVolID,
		})
		if err != nil {
			return nil, mapAPIError(err)
		}
		if reqBytes > vol.SizeBytes {
			vol, err = s.client.ResizeVolume(ctx, vol.ID, reqBytes)
			if err != nil {
				return nil, mapAPIError(err)
			}
		}
		return vol, nil

	default:
		return nil, status.Error(codes.InvalidArgument, "unsupported volume content source")
	}
}

func (s *ControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	exports, err := s.client.ListExports(ctx)
	if err == nil {
		for _, exp := range exports {
			if exp.VolumeID == req.GetVolumeId() {
				return nil, status.Errorf(codes.FailedPrecondition, "volume %s still has active export %s", req.GetVolumeId(), exp.ID)
			}
		}
	}

	// Clean up internal clone snapshots created for VolumeContentSource_Volume if no clones remain.
	if snaps, err := s.client.ListSnapshots(ctx); err == nil {
		vols, _ := s.client.ListVolumes(ctx)
		for _, sn := range snaps {
			if sn.VolumeID == req.GetVolumeId() && strings.HasPrefix(sn.Name, "csi-clone-") {
				inUse := false
				for _, v := range vols {
					if v.ParentSnapshotID == sn.ID {
						inUse = true
						break
					}
				}
				if !inUse {
					_ = s.client.DeleteSnapshot(ctx, sn.ID)
				}
			}
		}
	}

	if err := s.client.DeleteVolume(ctx, req.GetVolumeId()); err != nil {
		if IsNotFound(err) {
			return &csi.DeleteVolumeResponse{}, nil
		}
		return nil, mapAPIError(err)
	}
	return &csi.DeleteVolumeResponse{}, nil
}

func (s *ControllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	if req.GetNodeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "node_id is required")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume_capability is required")
	}
	if err := validateVolumeCapabilities([]*csi.VolumeCapability{req.GetVolumeCapability()}); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if len(s.knownNodes) > 0 && !s.knownNodes[req.GetNodeId()] {
		return nil, status.Errorf(codes.NotFound, "node %q not found", req.GetNodeId())
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	vol, err := s.client.GetVolume(ctx, req.GetVolumeId())
	if err != nil {
		return nil, mapAPIError(err)
	}

	proto := normalizeProtocol(req.GetVolumeContext()["protocol"])
	exports, err := s.client.ListExports(ctx)
	if err != nil {
		return nil, mapAPIError(err)
	}
	portals, err := s.client.ListPortals(ctx)
	if err != nil {
		return nil, mapAPIError(err)
	}
	// Exports are per (volume, node). Every access mode we support is single-node, so a
	// volume already published to a different node must not be handed to this one.
	for _, exp := range exports {
		if exp.VolumeID != vol.ID {
			continue
		}
		if exp.NodeID == req.GetNodeId() {
			return &csi.ControllerPublishVolumeResponse{
				PublishContext: buildPublishContext(exp, exportPortal(exp, portals), vol),
			}, nil
		}
		if exp.NodeID == "" {
			return nil, status.Errorf(codes.FailedPrecondition, "volume %s has a non-CSI export %s", vol.ID, exp.ID)
		}
		return nil, status.Errorf(codes.FailedPrecondition, "volume %s is already published to node %q", vol.ID, exp.NodeID)
	}

	var portal *store.Portal
	for _, p := range portals {
		if p.Protocol == proto {
			portal = p
			break
		}
	}
	if portal == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "no %s storage portal configured in Velocor", proto)
	}

	igID, err := s.ensureNodeInitiatorGroup(ctx, req.GetNodeId())
	if err != nil {
		return nil, mapAPIError(err)
	}

	exp, err := s.client.CreateExport(ctx, CreateExportRequest{
		Name:              exportNameFor(vol.ID, req.GetNodeId()),
		VolumeID:          vol.ID,
		Protocol:          proto,
		PortalIDs:         []string{portal.ID},
		InitiatorGroupIDs: []string{igID},
		AllowAnyHost:      false,
		NodeID:            req.GetNodeId(),
	})
	if err != nil {
		return nil, mapAPIError(err)
	}

	return &csi.ControllerPublishVolumeResponse{
		PublishContext: buildPublishContext(exp, portal, vol),
	}, nil
}

// exportNameFor derives a short, deterministic export name for a (volume, node) pair that
// is valid inside NQNs/IQNs regardless of how long or exotic the Kubernetes node name is.
func exportNameFor(volID, nodeID string) string {
	sum := sha256.Sum256([]byte(nodeID))
	return "csi-" + volID + "-" + hex.EncodeToString(sum[:6])
}

func exportPortal(exp *store.Export, portals []*store.Portal) *store.Portal {
	for _, pid := range exp.PortalIDs {
		for _, p := range portals {
			if p.ID == pid {
				return p
			}
		}
	}
	return nil
}

func (s *ControllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	exports, err := s.client.ListExports(ctx)
	if err != nil {
		return nil, mapAPIError(err)
	}
	for _, exp := range exports {
		// Only CSI-managed exports (NodeID set) are touched. An empty node_id means
		// "unpublish from all nodes" per the CSI spec.
		if exp.VolumeID != req.GetVolumeId() || exp.NodeID == "" {
			continue
		}
		if req.GetNodeId() != "" && exp.NodeID != req.GetNodeId() {
			continue
		}
		if err := s.client.DeleteExport(ctx, exp.ID); err != nil && !IsNotFound(err) {
			return nil, mapAPIError(err)
		}
	}
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (s *ControllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume_capabilities are required")
	}
	if _, err := s.client.GetVolume(ctx, req.GetVolumeId()); err != nil {
		return nil, mapAPIError(err)
	}
	if err := validateVolumeCapabilities(req.GetVolumeCapabilities()); err != nil {
		return &csi.ValidateVolumeCapabilitiesResponse{
			Message: err.Error(),
		}, nil
	}
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.GetVolumeCapabilities(),
			VolumeContext:      req.GetVolumeContext(),
			Parameters:         req.GetParameters(),
		},
	}, nil
}

func (s *ControllerServer) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	vols, err := s.client.ListVolumes(ctx)
	if err != nil {
		return nil, mapAPIError(err)
	}
	pubByVol, err := s.publishedNodes(ctx)
	if err != nil {
		return nil, err
	}

	start, end, nextToken, err := paginate(len(vols), req.GetStartingToken(), req.GetMaxEntries())
	if err != nil {
		return nil, err
	}

	entries := make([]*csi.ListVolumesResponse_Entry, 0, end-start)
	for _, v := range vols[start:end] {
		entries = append(entries, &csi.ListVolumesResponse_Entry{
			Volume: toCSIVolume(v, nil, nil),
			Status: &csi.ListVolumesResponse_VolumeStatus{
				PublishedNodeIds: pubByVol[v.ID],
			},
		})
	}
	return &csi.ListVolumesResponse{
		Entries:   entries,
		NextToken: nextToken,
	}, nil
}

func (s *ControllerServer) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	if len(req.GetVolumeCapabilities()) > 0 {
		if err := validateVolumeCapabilities(req.GetVolumeCapabilities()); err != nil {
			return &csi.GetCapacityResponse{AvailableCapacity: 0}, nil
		}
	}
	pools, err := s.client.ListPools(ctx)
	if err != nil {
		return nil, mapAPIError(err)
	}
	poolFilter := req.GetParameters()["pool"]
	var totalFree int64
	for _, p := range pools {
		if poolFilter != "" && p.ID != poolFilter && p.Name != poolFilter {
			continue
		}
		if p.HealthStatus == store.HealthStatusOnline || p.HealthStatus == store.HealthStatusUnspecified {
			totalFree += int64(p.FreeBytes)
		}
	}
	return &csi.GetCapacityResponse{AvailableCapacity: totalFree}, nil
}

func (s *ControllerServer) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot name is required")
	}
	if req.GetSourceVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "source_volume_id is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	snaps, err := s.client.ListSnapshots(ctx)
	if err != nil {
		return nil, mapAPIError(err)
	}
	for _, sn := range snaps {
		if sn.Name == req.GetName() {
			if sn.VolumeID != req.GetSourceVolumeId() {
				return nil, status.Errorf(codes.AlreadyExists, "snapshot %q already exists for volume %s", req.GetName(), sn.VolumeID)
			}
			return &csi.CreateSnapshotResponse{Snapshot: toCSISnapshot(sn)}, nil
		}
	}

	if _, err := s.client.GetVolume(ctx, req.GetSourceVolumeId()); err != nil {
		return nil, mapAPIError(err)
	}

	sn, err := s.client.CreateSnapshot(ctx, req.GetSourceVolumeId(), req.GetName(), map[string]string{"csi.velocor.io/provisioned": "true"})
	if err != nil {
		return nil, mapAPIError(err)
	}
	return &csi.CreateSnapshotResponse{Snapshot: toCSISnapshot(sn)}, nil
}

func (s *ControllerServer) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	if req.GetSnapshotId() == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot_id is required")
	}
	if err := s.client.DeleteSnapshot(ctx, req.GetSnapshotId()); err != nil {
		if IsNotFound(err) {
			return &csi.DeleteSnapshotResponse{}, nil
		}
		return nil, mapAPIError(err)
	}
	return &csi.DeleteSnapshotResponse{}, nil
}

func (s *ControllerServer) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	snaps, err := s.client.ListSnapshots(ctx)
	if err != nil {
		return nil, mapAPIError(err)
	}
	filtered := make([]*store.Snapshot, 0, len(snaps))
	for _, sn := range snaps {
		if req.GetSnapshotId() != "" && sn.ID != req.GetSnapshotId() {
			continue
		}
		if req.GetSourceVolumeId() != "" && sn.VolumeID != req.GetSourceVolumeId() {
			continue
		}
		filtered = append(filtered, sn)
	}

	start, end, nextToken, err := paginate(len(filtered), req.GetStartingToken(), req.GetMaxEntries())
	if err != nil {
		return nil, err
	}
	entries := make([]*csi.ListSnapshotsResponse_Entry, 0, end-start)
	for _, sn := range filtered[start:end] {
		entries = append(entries, &csi.ListSnapshotsResponse_Entry{
			Snapshot: toCSISnapshot(sn),
		})
	}
	return &csi.ListSnapshotsResponse{
		Entries:   entries,
		NextToken: nextToken,
	}, nil
}

func (s *ControllerServer) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	reqBytes := req.GetCapacityRange().GetRequiredBytes()
	limBytes := req.GetCapacityRange().GetLimitBytes()
	if reqBytes <= 0 || (limBytes > 0 && reqBytes > limBytes) {
		return nil, status.Error(codes.InvalidArgument, "invalid capacity range")
	}

	vol, err := s.client.GetVolume(ctx, req.GetVolumeId())
	if err != nil {
		return nil, mapAPIError(err)
	}
	if int64(vol.SizeBytes) < reqBytes {
		vol, err = s.client.ResizeVolume(ctx, vol.ID, uint64(reqBytes))
		if err != nil {
			return nil, mapAPIError(err)
		}
	}

	nodeExpansionRequired := true
	if cap := req.GetVolumeCapability(); cap != nil && cap.GetBlock() != nil {
		nodeExpansionRequired = false
	}

	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         int64(vol.SizeBytes),
		NodeExpansionRequired: nodeExpansionRequired,
	}, nil
}

func (s *ControllerServer) ControllerGetVolume(ctx context.Context, req *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	vol, err := s.client.GetVolume(ctx, req.GetVolumeId())
	if err != nil {
		return nil, mapAPIError(err)
	}
	pubByVol, err := s.publishedNodes(ctx)
	if err != nil {
		return nil, err
	}
	return &csi.ControllerGetVolumeResponse{
		Volume: toCSIVolume(vol, nil, nil),
		Status: &csi.ControllerGetVolumeResponse_VolumeStatus{
			PublishedNodeIds: pubByVol[vol.ID],
		},
	}, nil
}

// publishedNodes maps volume IDs to the CSI node IDs they are published to.
func (s *ControllerServer) publishedNodes(ctx context.Context) (map[string][]string, error) {
	exports, err := s.client.ListExports(ctx)
	if err != nil {
		return nil, mapAPIError(err)
	}
	out := map[string][]string{}
	for _, exp := range exports {
		if exp.NodeID != "" {
			out[exp.VolumeID] = append(out[exp.VolumeID], exp.NodeID)
		}
	}
	return out, nil
}

func (s *ControllerServer) selectPool(ctx context.Context, poolParam string, requiredBytes uint64) (string, error) {
	pools, err := s.client.ListPools(ctx)
	if err != nil {
		return "", mapAPIError(err)
	}
	if len(pools) == 0 {
		return "", status.Error(codes.FailedPrecondition, "no storage pools available in Velocor")
	}
	if poolParam != "" {
		for _, p := range pools {
			if p.ID == poolParam || p.Name == poolParam {
				return p.ID, nil
			}
		}
		return "", status.Errorf(codes.InvalidArgument, "pool %q not found", poolParam)
	}
	for _, p := range pools {
		if p.FreeBytes >= requiredBytes {
			return p.ID, nil
		}
	}
	return "", status.Errorf(codes.ResourceExhausted, "no storage pool has %d bytes free", requiredBytes)
}

func (s *ControllerServer) ensureNodeInitiatorGroup(ctx context.Context, nodeID string) (string, error) {
	groupName := "csi-node-" + sanitizeName(nodeID)
	igs, err := s.client.ListInitiatorGroups(ctx)
	if err != nil {
		return "", err
	}
	for _, g := range igs {
		if g.Name == groupName {
			return g.ID, nil
		}
	}

	hosts, err := s.client.ListHosts(ctx)
	if err != nil {
		return "", err
	}
	var hostID string
	for _, h := range hosts {
		if h.Name == groupName {
			hostID = h.ID
			break
		}
	}
	if hostID == "" {
		nqn := nodeHostNQN(nodeID)
		iqn := nodeInitiatorIQN(nodeID)
		h, err := s.client.CreateHost(ctx, groupName, nqn, iqn, "")
		if err != nil {
			return "", err
		}
		hostID = h.ID
	}
	ig, err := s.client.CreateInitiatorGroup(ctx, groupName, []string{hostID})
	if err != nil {
		return "", err
	}
	return ig.ID, nil
}

func nodeHostNQN(nodeID string) string {
	return "nqn.2026-01.io.velocor:node:" + sanitizeName(nodeID)
}

func nodeInitiatorIQN(nodeID string) string {
	return "iqn.2026-01.io.velocor:" + sanitizeName(nodeID)
}

func sanitizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "default"
	}
	return b.String()
}

func normalizeProtocol(p string) store.ExportProtocol {
	switch strings.ToUpper(strings.TrimSpace(p)) {
	case "ISCSI":
		return store.ExportProtocolISCSI
	default:
		return store.ExportProtocolNVMeoFTCP
	}
}

func buildPublishContext(exp *store.Export, portal *store.Portal, vol *store.Volume) map[string]string {
	ctx := map[string]string{
		"export_id": exp.ID,
		"protocol":  string(exp.Protocol),
	}
	if vol != nil && vol.UUID != "" {
		// The node verifies the attached namespace carries this UUID before formatting/mounting.
		ctx["volume_uuid"] = vol.UUID
	}
	if portal != nil {
		ctx["portal_ip"] = portal.IPAddress
		ctx["portal_port"] = strconv.FormatUint(uint64(portal.Port), 10)
	}
	if exp.SubsystemNQN != "" {
		ctx["target_nqn"] = exp.SubsystemNQN
		ctx["namespace_id"] = strconv.FormatUint(uint64(exp.NamespaceID), 10)
	}
	if exp.TargetIQN != "" {
		ctx["target_iqn"] = exp.TargetIQN
		ctx["lun_id"] = strconv.FormatUint(uint64(exp.LUNID), 10)
	}
	return ctx
}

func toCSIVolume(v *store.Volume, params map[string]string, src *csi.VolumeContentSource) *csi.Volume {
	proto := string(store.ExportProtocolNVMeoFTCP)
	if params != nil && params["protocol"] != "" {
		proto = string(normalizeProtocol(params["protocol"]))
	}
	return &csi.Volume{
		VolumeId:      v.ID,
		CapacityBytes: int64(v.SizeBytes),
		ContentSource: src,
		VolumeContext: map[string]string{
			"pool_id":  v.PoolID,
			"protocol": proto,
		},
	}
}

func toCSISnapshot(sn *store.Snapshot) *csi.Snapshot {
	return &csi.Snapshot{
		SnapshotId:     sn.ID,
		SourceVolumeId: sn.VolumeID,
		CreationTime:   timestamppb.New(sn.CreatedAt),
		SizeBytes:      int64(sn.SizeBytes),
		ReadyToUse:     true,
	}
}

func validateVolumeCapabilities(caps []*csi.VolumeCapability) error {
	for _, cap := range caps {
		if cap.GetMount() == nil && cap.GetBlock() == nil {
			return fmt.Errorf("volume capability must specify mount or block")
		}
		mode := cap.GetAccessMode().GetMode()
		switch mode {
		case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
			csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
			csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER:
		default:
			return fmt.Errorf("unsupported access mode %v", mode)
		}
	}
	return nil
}

func paginate(total int, startingToken string, maxEntries int32) (int, int, string, error) {
	start := 0
	if startingToken != "" {
		n, err := strconv.Atoi(startingToken)
		if err != nil || n < 0 || n > total {
			return 0, 0, "", status.Errorf(codes.Aborted, "invalid starting_token %q", startingToken)
		}
		start = n
	}
	end := total
	if maxEntries > 0 && start+int(maxEntries) < end {
		end = start + int(maxEntries)
	}
	next := ""
	if end < total {
		next = strconv.Itoa(end)
	}
	return start, end, next, nil
}

func mapAPIError(err error) error {
	if err == nil {
		return nil
	}
	if IsNotFound(err) {
		return status.Error(codes.NotFound, err.Error())
	}
	if IsAlreadyExists(err) {
		return status.Error(codes.AlreadyExists, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}
