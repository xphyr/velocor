package csi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const stageMetaFilename = ".velocor-stage.json"

type stageMetadata struct {
	DevicePath     string            `json:"device_path"`
	PublishContext map[string]string `json:"publish_context"`
	IsBlock        bool              `json:"is_block"`
}

// NodeServer implements the CSI Node service.
type NodeServer struct {
	csi.UnimplementedNodeServer
	nodeID           string
	mounter          Mounter
	publishedTargets map[string]map[string]bool
	mu               sync.Mutex
}

// NewNodeServer creates a CSI NodeServer.
func NewNodeServer(nodeID string, mounter Mounter) *NodeServer {
	return &NodeServer{
		nodeID:           nodeID,
		mounter:          mounter,
		publishedTargets: map[string]map[string]bool{},
	}
}

func (s *NodeServer) NodeGetCapabilities(_ context.Context, _ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	caps := []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
		csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
		csi.NodeServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
	}
	out := make([]*csi.NodeServiceCapability, 0, len(caps))
	for _, c := range caps {
		out = append(out, &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{Type: c},
			},
		})
	}
	return &csi.NodeGetCapabilitiesResponse{Capabilities: out}, nil
}

func (s *NodeServer) NodeGetInfo(_ context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId: s.nodeID,
	}, nil
}

func (s *NodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	if req.GetVolumeId() == "" || req.GetStagingTargetPath() == "" || req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume_id, staging_target_path, and volume_capability are required")
	}
	if err := validateVolumeCapabilities([]*csi.VolumeCapability{req.GetVolumeCapability()}); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	devPath, err := s.mounter.ConnectTarget(ctx, req.GetPublishContext(), nodeHostNQN(s.nodeID), nodeInitiatorIQN(s.nodeID))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "connect target: %v", err)
	}

	isBlock := req.GetVolumeCapability().GetBlock() != nil
	if isBlock {
		if err := os.MkdirAll(req.GetStagingTargetPath(), 0750); err != nil {
			return nil, status.Errorf(codes.Internal, "mkdir staging: %v", err)
		}
	} else {
		mnt := req.GetVolumeCapability().GetMount()
		mountPath := filepath.Join(req.GetStagingTargetPath(), "mount")
		if err := s.mounter.FormatAndMount(ctx, devPath, mountPath, mnt.GetFsType(), mnt.GetMountFlags()); err != nil {
			return nil, status.Errorf(codes.Internal, "format and mount: %v", err)
		}
	}

	meta := stageMetadata{
		DevicePath:     devPath,
		PublishContext: req.GetPublishContext(),
		IsBlock:        isBlock,
	}
	if b, err := json.Marshal(meta); err == nil {
		_ = os.WriteFile(filepath.Join(req.GetStagingTargetPath(), stageMetaFilename), b, 0600)
	}
	return &csi.NodeStageVolumeResponse{}, nil
}

func (s *NodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	if req.GetVolumeId() == "" || req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id and staging_target_path are required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	mountPath := filepath.Join(req.GetStagingTargetPath(), "mount")
	if err := s.mounter.Unmount(ctx, mountPath); err != nil {
		return nil, status.Errorf(codes.Internal, "unmount staging: %v", err)
	}
	_ = os.Remove(mountPath)

	metaPath := filepath.Join(req.GetStagingTargetPath(), stageMetaFilename)
	if raw, err := os.ReadFile(metaPath); err == nil {
		var meta stageMetadata
		if json.Unmarshal(raw, &meta) == nil {
			_ = s.mounter.DisconnectTarget(ctx, meta.PublishContext)
		}
		_ = os.Remove(metaPath)
	}
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (s *NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if req.GetVolumeId() == "" || req.GetTargetPath() == "" || req.GetStagingTargetPath() == "" || req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume_id, staging_target_path, target_path, and volume_capability are required")
	}
	if err := validateVolumeCapabilities([]*csi.VolumeCapability{req.GetVolumeCapability()}); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if req.GetVolumeCapability().GetAccessMode().GetMode() == csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER {
		for tp := range s.publishedTargets[req.GetVolumeId()] {
			if tp != req.GetTargetPath() {
				return nil, status.Errorf(codes.FailedPrecondition, "volume %s already published at %s", req.GetVolumeId(), tp)
			}
		}
	}

	var meta stageMetadata
	if raw, err := os.ReadFile(filepath.Join(req.GetStagingTargetPath(), stageMetaFilename)); err == nil {
		_ = json.Unmarshal(raw, &meta)
	}

	var opts []string
	if req.GetReadonly() {
		opts = append(opts, "ro")
	}
	if mnt := req.GetVolumeCapability().GetMount(); mnt != nil {
		opts = append(opts, mnt.GetMountFlags()...)
	}

	isBlock := req.GetVolumeCapability().GetBlock() != nil
	source := filepath.Join(req.GetStagingTargetPath(), "mount")
	if isBlock {
		source = meta.DevicePath
		if source == "" {
			return nil, status.Error(codes.FailedPrecondition, "staged block device path not found")
		}
	}

	if err := s.mounter.BindMount(ctx, source, req.GetTargetPath(), opts, isBlock); err != nil {
		return nil, status.Errorf(codes.Internal, "bind mount: %v", err)
	}
	if s.publishedTargets[req.GetVolumeId()] == nil {
		s.publishedTargets[req.GetVolumeId()] = map[string]bool{}
	}
	s.publishedTargets[req.GetVolumeId()][req.GetTargetPath()] = true
	return &csi.NodePublishVolumeResponse{}, nil
}

func (s *NodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if req.GetVolumeId() == "" || req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id and target_path are required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.mounter.Unmount(ctx, req.GetTargetPath()); err != nil {
		return nil, status.Errorf(codes.Internal, "unmount target: %v", err)
	}
	_ = os.RemoveAll(req.GetTargetPath())
	if targets := s.publishedTargets[req.GetVolumeId()]; targets != nil {
		delete(targets, req.GetTargetPath())
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (s *NodeServer) NodeGetVolumeStats(_ context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	if req.GetVolumeId() == "" || req.GetVolumePath() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id and volume_path are required")
	}
	if _, err := os.Stat(req.GetVolumePath()); os.IsNotExist(err) {
		return nil, status.Errorf(codes.NotFound, "volume_path %q does not exist", req.GetVolumePath())
	}

	stats, err := s.mounter.GetVolumeStats(req.GetVolumePath())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get volume stats: %v", err)
	}

	usage := []*csi.VolumeUsage{
		{
			Unit:      csi.VolumeUsage_BYTES,
			Available: stats.AvailableBytes,
			Total:     stats.TotalBytes,
			Used:      stats.UsedBytes,
		},
	}
	if !stats.IsBlock && stats.TotalInodes > 0 {
		usage = append(usage, &csi.VolumeUsage{
			Unit:      csi.VolumeUsage_INODES,
			Available: stats.AvailableInodes,
			Total:     stats.TotalInodes,
			Used:      stats.UsedInodes,
		})
	}
	return &csi.NodeGetVolumeStatsResponse{Usage: usage}, nil
}

func (s *NodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	if req.GetVolumeId() == "" || req.GetVolumePath() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id and volume_path are required")
	}
	if _, err := os.Stat(req.GetVolumePath()); os.IsNotExist(err) {
		return nil, status.Errorf(codes.NotFound, "volume_path %q does not exist", req.GetVolumePath())
	}

	var devPath string
	if req.GetStagingTargetPath() != "" {
		if raw, err := os.ReadFile(filepath.Join(req.GetStagingTargetPath(), stageMetaFilename)); err == nil {
			var meta stageMetadata
			if json.Unmarshal(raw, &meta) == nil {
				devPath = meta.DevicePath
			}
		}
	}

	if err := s.mounter.ResizeFilesystem(ctx, devPath, req.GetVolumePath()); err != nil {
		return nil, status.Errorf(codes.Internal, "resize filesystem: %v", err)
	}

	return &csi.NodeExpandVolumeResponse{
		CapacityBytes: req.GetCapacityRange().GetRequiredBytes(),
	}, nil
}
