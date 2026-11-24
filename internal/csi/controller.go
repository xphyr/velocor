package csi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
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
	// defaultPortals is used for volumes whose StorageClass sets no "portals" parameter
	// (--default-portals; names or IDs, either protocol).
	defaultPortals string
	mu             sync.Mutex
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

	// Check the portals now, so a class that can't be published fails at provisioning with a
	// message, not later on every node that tries to use the volume.
	if err := s.checkPortals(ctx, req.GetParameters()); err != nil {
		return nil, err
	}

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
		vol, err = s.client.CreateVolume(ctx, CreateVolumeRequest{
			Name:      req.GetName(),
			PoolID:    poolID,
			SizeBytes: uint64(reqBytes),
			Labels:    map[string]string{"csi.velocor.io/provisioned": "true"},
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
		if IsAlreadyExists(err) { // 409: protected, exported or attached
			return nil, status.Error(codes.FailedPrecondition, err.Error())
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
	// Single-node volumes get one export per (volume, node); a volume already published to a
	// different node must not be handed to this one. Multi-node (shared) volumes get one export
	// that every node using them is allowed into.
	shared := isSharedMode(req.GetVolumeCapability())
	var sharedExp *store.Export
	for _, exp := range exports {
		if exp.VolumeID != vol.ID {
			continue
		}
		switch {
		case exp.Shared && shared:
			sharedExp = exp
		case exp.Shared:
			return nil, status.Errorf(codes.FailedPrecondition, "volume %s is published as a shared (multi-node) volume", vol.ID)
		case exp.NodeID == "":
			return nil, status.Errorf(codes.FailedPrecondition, "volume %s has a non-CSI export %s", vol.ID, exp.ID)
		case exp.NodeID == req.GetNodeId() && !shared:
			return &csi.ControllerPublishVolumeResponse{
				PublishContext: buildPublishContext(exp, exportPortals(exp, portals), vol),
			}, nil
		default:
			return nil, status.Errorf(codes.FailedPrecondition, "volume %s is already published to node %q", vol.ID, exp.NodeID)
		}
	}
	if sharedExp != nil {
		if slices.Contains(sharedExp.NodeIDs, req.GetNodeId()) {
			return &csi.ControllerPublishVolumeResponse{
				PublishContext: buildPublishContext(sharedExp, exportPortals(sharedExp, portals), vol),
			}, nil
		}
		igID, err := s.ensureNodeInitiatorGroup(ctx, req.GetNodeId())
		if err != nil {
			return nil, mapAPIError(err)
		}
		// Added while the other nodes stay connected (a live migration's target joins its source).
		igs := append(slices.Clone(sharedExp.InitiatorGroupIDs), igID)
		nodes := append(slices.Clone(sharedExp.NodeIDs), req.GetNodeId())
		updated, err := s.client.UpdateExport(ctx, sharedExp.ID, UpdateExportRequest{InitiatorGroupIDs: &igs, NodeIDs: &nodes})
		if err != nil {
			return nil, mapAPIError(err)
		}
		return &csi.ControllerPublishVolumeResponse{
			PublishContext: buildPublishContext(updated, exportPortals(updated, portals), vol),
		}, nil
	}

	// The volume context is fixed when the volume is provisioned, so a portal list that no
	// longer resolves is a precondition failure, not a bad request.
	chosen, err := s.resolvePortals(req.GetVolumeContext()[paramPortals], proto, portals)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	portalIDs := make([]string, len(chosen))
	for i, p := range chosen {
		portalIDs[i] = p.ID
	}

	igID, err := s.ensureNodeInitiatorGroup(ctx, req.GetNodeId())
	if err != nil {
		return nil, mapAPIError(err)
	}

	create := CreateExportRequest{
		Name:              exportNameFor(vol.ID, req.GetNodeId()),
		VolumeID:          vol.ID,
		Protocol:          proto,
		PortalIDs:         portalIDs,
		InitiatorGroupIDs: []string{igID},
		AllowAnyHost:      false,
		NodeID:            req.GetNodeId(),
	}
	if shared {
		create.Name, create.NodeID, create.Shared, create.NodeIDs = "csi-"+vol.ID+"-shared", "", true, []string{req.GetNodeId()}
	}
	exp, err := s.client.CreateExport(ctx, create)
	if err != nil {
		return nil, mapAPIError(err)
	}

	return &csi.ControllerPublishVolumeResponse{
		PublishContext: buildPublishContext(exp, chosen, vol),
	}, nil
}

// exportNameFor derives a short, deterministic export name for a (volume, node) pair that
// is valid inside NQNs/IQNs regardless of how long or exotic the Kubernetes node name is.
func exportNameFor(volID, nodeID string) string {
	sum := sha256.Sum256([]byte(nodeID))
	return "csi-" + volID + "-" + hex.EncodeToString(sum[:6])
}

// exportPortals returns an export's portals in the export's order.
func exportPortals(exp *store.Export, portals []*store.Portal) []*store.Portal {
	var out []*store.Portal
	for _, pid := range exp.PortalIDs {
		for _, p := range portals {
			if p.ID == pid {
				out = append(out, p)
			}
		}
	}
	return out
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
		if exp.VolumeID == req.GetVolumeId() && exp.Shared {
			if err := s.unpublishShared(ctx, exp, req.GetNodeId()); err != nil {
				return nil, mapAPIError(err)
			}
			continue
		}
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
		if poolServes(p) {
			totalFree += int64(poolAvailable(p))
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
// unpublishShared takes a node off a shared export (all nodes if nodeID is empty), and deletes
// the export once no node uses it.
func (s *ControllerServer) unpublishShared(ctx context.Context, exp *store.Export, nodeID string) error {
	if nodeID != "" && !slices.Contains(exp.NodeIDs, nodeID) {
		return nil
	}
	nodes := slices.DeleteFunc(slices.Clone(exp.NodeIDs), func(n string) bool { return nodeID == "" || n == nodeID })
	if len(nodes) == 0 {
		if err := s.client.DeleteExport(ctx, exp.ID); err != nil && !IsNotFound(err) {
			return err
		}
		return nil
	}
	igs, err := s.client.ListInitiatorGroups(ctx)
	if err != nil {
		return err
	}
	groupName := "csi-node-" + sanitizeName(nodeID)
	keep := slices.DeleteFunc(slices.Clone(exp.InitiatorGroupIDs), func(id string) bool {
		for _, g := range igs {
			if g.ID == id && g.Name == groupName {
				return true
			}
		}
		return false
	})
	_, err = s.client.UpdateExport(ctx, exp.ID, UpdateExportRequest{InitiatorGroupIDs: &keep, NodeIDs: &nodes})
	return err
}

// isSharedMode reports access modes in which several nodes use a volume at once.
func isSharedMode(cap *csi.VolumeCapability) bool {
	switch cap.GetAccessMode().GetMode() {
	case csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
		return true
	}
	return false
}

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
		out[exp.VolumeID] = append(out[exp.VolumeID], exp.NodeIDs...)
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
	// Pick by what admission would accept (committed space, overcommit policy, reserve, space
	// state), not raw free space: thin volumes make free space meaningless for this.
	for _, p := range pools {
		if poolServes(p) && poolAvailable(p) >= requiredBytes {
			return p.ID, nil
		}
	}
	return "", status.Errorf(codes.ResourceExhausted, "no storage pool can take %d more bytes (see each pool's available_bytes and space_state)", requiredBytes)
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

func buildPublishContext(exp *store.Export, portals []*store.Portal, vol *store.Volume) map[string]string {
	ctx := map[string]string{
		"export_id": exp.ID,
		"protocol":  string(exp.Protocol),
	}
	if vol != nil && vol.UUID != "" {
		// The node verifies the attached namespace carries this UUID before formatting/mounting.
		ctx["volume_uuid"] = vol.UUID
	}
	if len(portals) > 0 {
		// portal_ip/portal_port name the first portal, for node plugins older than "portals".
		ctx["portal_ip"] = portals[0].IPAddress
		ctx["portal_port"] = strconv.FormatUint(uint64(portals[0].Port), 10)
		addrs := make([]string, len(portals))
		for i, p := range portals {
			addrs[i] = net.JoinHostPort(p.IPAddress, strconv.FormatUint(uint64(p.Port), 10))
		}
		ctx["portals"] = strings.Join(addrs, ",")
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
		VolumeContext: volumeContext(v, params, proto),
	}
}

func volumeContext(v *store.Volume, params map[string]string, proto string) map[string]string {
	vc := map[string]string{"pool_id": v.PoolID, "protocol": proto}
	if spec := strings.TrimSpace(params[paramPortals]); spec != "" {
		vc[paramPortals] = spec
	}
	return vc
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
		case csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
			csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
			csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
			// A filesystem mounted on several nodes is corrupted (even read-only nodes cache
			// stale metadata). Raw block is what KubeVirt live migration needs; the users of a
			// shared device coordinate their writes and use direct I/O, as QEMU does.
			if cap.GetBlock() == nil {
				return fmt.Errorf("access mode %v is only supported for raw block volumes (volumeMode: Block)", mode)
			}
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
	var apiErr *ErrAPI
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusInsufficientStorage {
		return status.Error(codes.ResourceExhausted, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}

// poolServes reports whether a pool can take new volumes: its RAID serves I/O (a degraded or
// rebuilding pool still does).
func poolServes(p *store.Pool) bool {
	switch p.HealthStatus {
	case store.HealthStatusFaulted, store.HealthStatusUnavailable, store.HealthStatusReconciling, store.HealthStatusOffline:
		return false
	}
	return true
}

// poolAvailable is how much more a pool accepts. A velod without space accounting (older than
// P0-2, e.g. while the CSI driver is upgraded first) reports no space state: fall back to its
// free space.
func poolAvailable(p *store.Pool) uint64 {
	if p.SpaceState == "" {
		return p.FreeBytes
	}
	return p.AvailableBytes
}
