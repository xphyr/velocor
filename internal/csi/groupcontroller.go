package csi

import (
	"context"
	"errors"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/velocor/velocor/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// GroupControllerServer implements the CSI v1.x GroupController service for
// crash-consistent multi-volume group snapshots (KEP-3476).
type GroupControllerServer struct {
	csi.UnimplementedGroupControllerServer
	client VelocorClient
}

// NewGroupControllerServer creates a GroupControllerServer backed by VelocorClient.
func NewGroupControllerServer(client VelocorClient) *GroupControllerServer {
	return &GroupControllerServer{client: client}
}

func (s *GroupControllerServer) GroupControllerGetCapabilities(_ context.Context, _ *csi.GroupControllerGetCapabilitiesRequest) (*csi.GroupControllerGetCapabilitiesResponse, error) {
	return &csi.GroupControllerGetCapabilitiesResponse{
		Capabilities: []*csi.GroupControllerServiceCapability{
			{
				Type: &csi.GroupControllerServiceCapability_Rpc{
					Rpc: &csi.GroupControllerServiceCapability_RPC{
						Type: csi.GroupControllerServiceCapability_RPC_CREATE_DELETE_GET_VOLUME_GROUP_SNAPSHOT,
					},
				},
			},
		},
	}, nil
}

func (s *GroupControllerServer) CreateVolumeGroupSnapshot(ctx context.Context, req *csi.CreateVolumeGroupSnapshotRequest) (*csi.CreateVolumeGroupSnapshotResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "group snapshot name is required")
	}
	if len(req.GetSourceVolumeIds()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "source_volume_ids must be non-empty")
	}
	for _, id := range req.GetSourceVolumeIds() {
		if id == "" {
			return nil, status.Error(codes.InvalidArgument, "source_volume_ids must not contain empty IDs")
		}
	}

	res, err := s.client.CreateVolumeGroupSnapshot(ctx, req.GetName(), req.GetSourceVolumeIds())
	if err != nil {
		return nil, mapGroupControllerError(err)
	}
	return &csi.CreateVolumeGroupSnapshotResponse{
		GroupSnapshot: toCSIVolumeGroupSnapshot(res),
	}, nil
}

func (s *GroupControllerServer) GetVolumeGroupSnapshot(ctx context.Context, req *csi.GetVolumeGroupSnapshotRequest) (*csi.GetVolumeGroupSnapshotResponse, error) {
	if req.GetGroupSnapshotId() == "" {
		return nil, status.Error(codes.InvalidArgument, "group_snapshot_id is required")
	}

	res, err := s.client.GetVolumeGroupSnapshot(ctx, req.GetGroupSnapshotId())
	if err != nil {
		return nil, mapGroupControllerError(err)
	}
	if len(req.GetSnapshotIds()) > 0 && !sameIDSet(res.GroupSnapshot.SnapshotIDs, req.GetSnapshotIds()) {
		return nil, status.Errorf(codes.InvalidArgument, "snapshot_ids do not match group snapshot %s", req.GetGroupSnapshotId())
	}
	return &csi.GetVolumeGroupSnapshotResponse{
		GroupSnapshot: toCSIVolumeGroupSnapshot(res),
	}, nil
}

func (s *GroupControllerServer) DeleteVolumeGroupSnapshot(ctx context.Context, req *csi.DeleteVolumeGroupSnapshotRequest) (*csi.DeleteVolumeGroupSnapshotResponse, error) {
	if req.GetGroupSnapshotId() == "" {
		return nil, status.Error(codes.InvalidArgument, "group_snapshot_id is required")
	}

	if len(req.GetSnapshotIds()) > 0 {
		res, err := s.client.GetVolumeGroupSnapshot(ctx, req.GetGroupSnapshotId())
		if err != nil {
			if isNotFoundErr(err) {
				return &csi.DeleteVolumeGroupSnapshotResponse{}, nil
			}
			return nil, mapGroupControllerError(err)
		}
		if !sameIDSet(res.GroupSnapshot.SnapshotIDs, req.GetSnapshotIds()) {
			return nil, status.Errorf(codes.InvalidArgument, "snapshot_ids do not match group snapshot %s", req.GetGroupSnapshotId())
		}
	}

	if err := s.client.DeleteVolumeGroupSnapshot(ctx, req.GetGroupSnapshotId()); err != nil {
		if isNotFoundErr(err) {
			return &csi.DeleteVolumeGroupSnapshotResponse{}, nil
		}
		return nil, mapGroupControllerError(err)
	}
	return &csi.DeleteVolumeGroupSnapshotResponse{}, nil
}

func toCSIVolumeGroupSnapshot(res *VolumeGroupSnapshotResult) *csi.VolumeGroupSnapshot {
	if res == nil || res.GroupSnapshot == nil {
		return nil
	}
	vgs := res.GroupSnapshot
	snaps := make([]*csi.Snapshot, 0, len(res.Snapshots))
	for _, snap := range res.Snapshots {
		snaps = append(snaps, &csi.Snapshot{
			SnapshotId:     snap.ID,
			SourceVolumeId: snap.VolumeID,
			CreationTime:   timestamppb.New(snap.CreatedAt),
			ReadyToUse:     true,
			SizeBytes:      int64(snap.SizeBytes),
		})
	}
	return &csi.VolumeGroupSnapshot{
		GroupSnapshotId: vgs.ID,
		Snapshots:       snaps,
		CreationTime:    timestamppb.New(vgs.CreatedAt),
		ReadyToUse:      vgs.ReadyToUse,
	}
}

func sameIDSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]struct{}, len(a))
	for _, id := range a {
		seen[id] = struct{}{}
	}
	for _, id := range b {
		if _, ok := seen[id]; !ok {
			return false
		}
	}
	return true
}

func isNotFoundErr(err error) bool {
	return errors.Is(err, store.ErrNotFound) || IsNotFound(err)
}

func mapGroupControllerError(err error) error {
	var apiErr *ErrAPI
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case 400:
			return status.Error(codes.InvalidArgument, apiErr.Message)
		case 404:
			return status.Error(codes.NotFound, apiErr.Message)
		case 409:
			return status.Error(codes.AlreadyExists, apiErr.Message)
		case 412:
			return status.Error(codes.FailedPrecondition, apiErr.Message)
		case 503: // e.g. the members' I/O didn't pause in time: retry later
			return status.Error(codes.Unavailable, apiErr.Message)
		default:
			return status.Error(codes.Internal, apiErr.Message)
		}
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, store.ErrAlreadyExists):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, store.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}
